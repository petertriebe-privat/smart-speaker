package mail

import (
	"bufio"
	"bytes"
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"net"
	"strconv"
	"strings"
	"time"

	"echo-assistant/internal/netx"
)

// A small IMAP client for what the assistant needs: LOGIN, SELECT/EXAMINE, UID SEARCH/FETCH/STORE/
// COPY/MOVE/EXPUNGE, CREATE, SUBSCRIBE, LIST, APPEND. One connection, one command at a time,
// implicit TLS (port 993). The M5 does not use IDLE (it polls), so there is none here.

const (
	tlsTimeout     = 15 * time.Second // handshake (the M5's MAIL_TLS_TIMEOUT_S)
	commandTimeout = 60 * time.Second // one command without data transfer
)

// fail makes an error whose text is what the model reads (M5 results start with "Error: ").
func fail(format string, args ...any) error { return fmt.Errorf("Error: "+format, args...) }

// dialTLS connects over IPv4 with implicit TLS and the embedded root CAs.
func dialTLS(host string, port int) (net.Conn, error) {
	ctx, cancel := context.WithTimeout(context.Background(), tlsTimeout)
	defer cancel()
	raw, err := netx.Dial(ctx, "tcp", net.JoinHostPort(host, strconv.Itoa(port)))
	if err != nil {
		return nil, err
	}
	cfg := netx.TLSConfig()
	cfg.ServerName = host
	conn := tls.Client(raw, cfg)
	if err := conn.HandshakeContext(ctx); err != nil {
		raw.Close()
		return nil, err
	}
	return conn, nil
}

// reply is the answer to one command: the tagged status and every untagged response before it.
type reply struct {
	Status   string // OK, NO or BAD
	Text     string // the tagged line after the status
	Untagged [][]byte
}

func (r *reply) ok() bool { return r.Status == "OK" }

type imapClient struct {
	conn   net.Conn
	br     *bufio.Reader
	tag    int
	status string // the last server message, for error reports (M5: lastStatus)
}

// mailboxInfo is what SELECT/EXAMINE says about a folder.
type mailboxInfo struct {
	UIDValidity uint32
	UIDNext     uint32
}

// dialIMAP connects and logs in.
func dialIMAP(host string, port int, user, password string) (*imapClient, error) {
	conn, err := dialTLS(host, port)
	if err != nil {
		return nil, fail("mailbox not reachable (%v)", err)
	}
	c := &imapClient{conn: conn, br: bufio.NewReaderSize(conn, 32<<10)}
	conn.SetDeadline(time.Now().Add(commandTimeout))
	greeting, err := readResponse(c.br)
	if err != nil || !bytes.HasPrefix(greeting, []byte("* OK")) && !bytes.HasPrefix(greeting, []byte("* PREAUTH")) {
		conn.Close()
		return nil, fail("mailbox not reachable (%s)", oneLine(string(greeting), err))
	}
	if bytes.HasPrefix(greeting, []byte("* PREAUTH")) {
		return c, nil
	}
	r, err := c.command("LOGIN " + quoteString(user) + " " + quoteString(password))
	if err != nil || !r.ok() {
		conn.Close()
		return nil, fail("mailbox not reachable (%s)", oneLine(c.status, err))
	}
	return c, nil
}

func oneLine(text string, err error) string {
	if err != nil && strings.TrimSpace(text) == "" {
		text = err.Error()
	}
	return strings.Join(strings.Fields(text), " ")
}

// command sends one command and reads up to its tagged answer. The error is for a broken
// connection only; NO and BAD come back in the reply.
func (c *imapClient) command(line string) (*reply, error) {
	return c.commandWithTimeout(line, commandTimeout)
}

func (c *imapClient) commandWithTimeout(line string, timeout time.Duration) (*reply, error) {
	c.tag++
	tag := "J" + strconv.Itoa(c.tag)
	c.conn.SetDeadline(time.Now().Add(timeout))
	if _, err := c.conn.Write([]byte(tag + " " + line + "\r\n")); err != nil {
		return nil, err
	}
	return c.readTagged(tag)
}

func (c *imapClient) readTagged(tag string) (*reply, error) {
	r := &reply{}
	for {
		raw, err := readResponse(c.br)
		if err != nil {
			return nil, err
		}
		if !bytes.HasPrefix(raw, []byte(tag+" ")) {
			r.Untagged = append(r.Untagged, raw)
			continue
		}
		status, text, _ := strings.Cut(strings.TrimSpace(string(raw[len(tag)+1:])), " ")
		r.Status, r.Text = strings.ToUpper(status), text
		c.status = text
		return r, nil
	}
}

// do runs a command and turns NO/BAD into an error carrying the server's text.
func (c *imapClient) do(line string) (*reply, error) {
	r, err := c.command(line)
	if err != nil {
		c.status = err.Error()
		return nil, err
	}
	if !r.ok() {
		return r, errors.New(r.Text)
	}
	return r, nil
}

// selectFolder opens a folder (EXAMINE when readOnly: flags stay as they are).
func (c *imapClient) selectFolder(folder string, readOnly bool) (mailboxInfo, error) {
	verb := "SELECT"
	if readOnly {
		verb = "EXAMINE"
	}
	r, err := c.do(verb + " " + quoteString(folder))
	if err != nil {
		return mailboxInfo{}, err
	}
	var info mailboxInfo
	for _, raw := range r.Untagged {
		if v, ok := responseCode(string(raw), "UIDVALIDITY"); ok {
			n, _ := strconv.ParseUint(v, 10, 32)
			info.UIDValidity = uint32(n)
		}
		if v, ok := responseCode(string(raw), "UIDNEXT"); ok {
			n, _ := strconv.ParseUint(v, 10, 32)
			info.UIDNext = uint32(n)
		}
	}
	return info, nil
}

// ensureFolder selects a folder, creating it on first use.
func (c *imapClient) ensureFolder(folder string, readOnly bool) error {
	if _, err := c.selectFolder(folder, readOnly); err == nil {
		return nil
	}
	c.command("CREATE " + quoteString(folder))
	if _, err := c.selectFolder(folder, readOnly); err != nil {
		return fail("folder %s not available (%s)", folder, c.status)
	}
	return nil
}

// search runs UID SEARCH and returns the UIDs, oldest first. Non-ASCII criteria need CHARSET
// UTF-8. found is false if the server sent no SEARCH line (the search failed).
func (c *imapClient) search(criteria string) (uids []uint32, found bool, err error) {
	line := "UID SEARCH "
	if !isASCII(criteria) {
		line += "CHARSET UTF-8 "
	}
	r, err := c.command(line + criteria)
	if err != nil {
		return nil, false, err
	}
	uids, found = parseSearch(r.Untagged)
	if !r.ok() {
		return nil, false, nil
	}
	return uids, found, nil
}

// fetch runs UID FETCH for a set and returns the parsed responses.
func (c *imapClient) fetch(set, items string, timeout time.Duration) ([]fetchData, error) {
	r, err := c.commandWithTimeout("UID FETCH "+set+" ("+items+")", timeout)
	if err != nil {
		return nil, err
	}
	if !r.ok() {
		return nil, errors.New(r.Text)
	}
	var out []fetchData
	for _, raw := range r.Untagged {
		if f, ok := parseFetch(raw); ok {
			out = append(out, f)
		}
	}
	return out, nil
}

// fetchHeaders returns the requested header lines of each UID ("FROM TO SUBJECT").
func (c *imapClient) fetchHeaders(uids []uint32, fields string) (map[uint32]string, error) {
	out := map[uint32]string{}
	if len(uids) == 0 {
		return out, nil
	}
	data, err := c.fetch(uidList(uids), "UID BODY.PEEK[HEADER.FIELDS ("+fields+")]", commandTimeout)
	if err != nil {
		return nil, err
	}
	for _, f := range data {
		if body, ok := f.Body(); ok {
			out[f.UID()] = body
		}
	}
	return out, nil
}

// fetchMessage returns the raw message (the first limit bytes if limit > 0) without touching its
// flags; ok is false if the mail is not there.
func (c *imapClient) fetchMessage(uid uint32, limit int) (msg []byte, ok bool, err error) {
	item := "BODY.PEEK[]"
	if limit > 0 {
		item += "<0." + strconv.Itoa(limit) + ">"
	}
	data, err := c.fetch(strconv.FormatUint(uint64(uid), 10), "UID "+item, 3*commandTimeout)
	if err != nil {
		return nil, false, err
	}
	for _, f := range data {
		if body, found := f.Body(); found && f.UID() == uid {
			return []byte(body), true, nil
		}
	}
	return nil, false, nil
}

// markSeen sets \Seen on a mail (what reading a mail in a mail program does).
func (c *imapClient) markSeen(uid uint32) error {
	_, err := c.do("UID STORE " + strconv.FormatUint(uint64(uid), 10) + " +FLAGS.SILENT (\\Seen)")
	return err
}

// appendMessage stores a message in a folder (APPEND) with the given flags, e.g. "\\Seen".
func (c *imapClient) appendMessage(folder, flags string, msg []byte) error {
	c.tag++
	tag := "J" + strconv.Itoa(c.tag)
	c.conn.SetDeadline(time.Now().Add(commandTimeout))
	line := fmt.Sprintf("%s APPEND %s", tag, quoteString(folder))
	if flags != "" {
		line += " (" + flags + ")"
	}
	line += fmt.Sprintf(" {%d}\r\n", len(msg))
	if _, err := c.conn.Write([]byte(line)); err != nil {
		return err
	}
	// The server answers "+ ..." when it wants the literal; a NO/BAD ends it here.
	for {
		raw, err := readResponse(c.br)
		if err != nil {
			return err
		}
		if bytes.HasPrefix(raw, []byte("+")) {
			break
		}
		if bytes.HasPrefix(raw, []byte(tag+" ")) {
			c.status = strings.TrimSpace(string(raw[len(tag)+1:]))
			return errors.New(c.status)
		}
	}
	c.conn.SetDeadline(time.Now().Add(commandTimeout + time.Duration(len(msg)/(32<<10))*time.Second))
	if _, err := c.conn.Write(append(append([]byte(nil), msg...), '\r', '\n')); err != nil {
		return err
	}
	r, err := c.readTagged(tag)
	if err != nil {
		return err
	}
	if !r.ok() {
		return errors.New(r.Text)
	}
	return nil
}

// logout ends the session politely and closes the connection.
func (c *imapClient) logout() {
	if c == nil {
		return
	}
	c.conn.SetDeadline(time.Now().Add(5 * time.Second))
	c.command("LOGOUT")
	c.conn.Close()
}

// stop closes the connection without a goodbye.
func (c *imapClient) stop() {
	if c != nil {
		c.conn.Close()
	}
}
