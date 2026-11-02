package mail

import (
	"bufio"
	"bytes"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"io"
	"mime"
	"mime/multipart"
	"mime/quotedprintable"
	"net/mail"
	"net/textproto"
	"strings"
	"time"
	"unicode/utf8"
)

// --- Charsets -----------------------------------------------------------------------------------

// cp1252High: Windows-1252 for 0x80..0x9F (the rest equals ISO-8859-1).
var cp1252High = [32]rune{
	0x20AC, 0x81, 0x201A, 0x0192, 0x201E, 0x2026, 0x2020, 0x2021, 0x02C6, 0x2030, 0x0160, 0x2039, 0x0152, 0x8D, 0x017D, 0x8F,
	0x90, 0x2018, 0x2019, 0x201C, 0x201D, 0x2022, 0x2013, 0x2014, 0x02DC, 0x2122, 0x0161, 0x203A, 0x0153, 0x9D, 0x017E, 0x0178,
}

// latin9: the eight letters ISO-8859-15 changed from ISO-8859-1.
var latin9 = map[byte]rune{0xA4: '€', 0xA6: 'Š', 0xA8: 'š', 0xB4: 'Ž', 0xB8: 'ž', 0xBC: 'Œ', 0xBD: 'œ', 0xBE: 'Ÿ'}

// toUTF8 converts text of the given charset to UTF-8. UTF-8, ASCII, ISO-8859-1/-15 and
// Windows-1252 (the German mail world) are converted; other charsets are passed through with
// invalid bytes replaced (the M5 does the same: utf8::repair), as there is no conversion table
// in the standard library.
func toUTF8(charset string, data []byte) string {
	switch strings.ToLower(strings.TrimSpace(charset)) {
	case "iso-8859-1", "iso8859-1", "latin1", "l1", "iso_8859-1":
		return latin1(data, nil, false)
	case "iso-8859-15", "iso8859-15", "latin9", "iso_8859-15":
		return latin1(data, latin9, false)
	case "windows-1252", "cp1252", "x-cp1252":
		return latin1(data, nil, true)
	case "us-ascii", "ascii":
		if !utf8.Valid(data) { // 8-bit text declared as ASCII is almost always Windows-1252
			return latin1(data, nil, true)
		}
	}
	return strings.ToValidUTF8(string(data), "?")
}

func latin1(data []byte, overrides map[byte]rune, windows bool) string {
	var sb strings.Builder
	sb.Grow(len(data) + len(data)/8)
	for _, b := range data {
		r := rune(b)
		if v, ok := overrides[b]; ok {
			r = v
		} else if windows && b >= 0x80 && b < 0xA0 {
			r = cp1252High[b-0x80]
		}
		sb.WriteRune(r)
	}
	return sb.String()
}

var wordDecoder = &mime.WordDecoder{CharsetReader: func(charset string, input io.Reader) (io.Reader, error) {
	data, err := io.ReadAll(input)
	if err != nil {
		return nil, err
	}
	return strings.NewReader(toUTF8(charset, data)), nil
}}

// decodeHeader makes an RFC 2047 header ("=?utf-8?B?VGVzdA==?=", "=?iso-8859-1?Q?M=FCller?=")
// plain text; folding is undone. Words it cannot decode stay as they are.
func decodeHeader(value string) string {
	value = strings.NewReplacer("\r\n", "", "\n", "", "\r", "").Replace(value)
	if !strings.Contains(value, "=?") {
		return strings.ToValidUTF8(strings.TrimSpace(value), "?")
	}
	decoded, err := wordDecoder.DecodeHeader(value)
	if err != nil {
		decoded = value
	}
	return strings.ToValidUTF8(strings.TrimSpace(decoded), "?")
}

// encodeHeader makes text a header value: ASCII as it is, else RFC 2047 encoded words.
func encodeHeader(value string) string {
	value = strings.NewReplacer("\r", " ", "\n", " ").Replace(value)
	return mime.QEncoding.Encode("utf-8", value)
}

// --- Transfer encodings -------------------------------------------------------------------------

// decodeTransfer decodes a part body according to Content-Transfer-Encoding. It is forgiving:
// what cannot be decoded is dropped, not an error (the M5 had to fight garbage at the end of a
// base64 part, too).
func decodeTransfer(r io.Reader, encoding string, limit int64) []byte {
	if limit > 0 {
		r = io.LimitReader(r, limit)
	}
	switch strings.ToLower(strings.TrimSpace(encoding)) {
	case "base64":
		raw, _ := io.ReadAll(r)
		return decodeBase64(raw)
	case "quoted-printable":
		data, _ := io.ReadAll(quotedprintable.NewReader(r))
		return data
	}
	data, _ := io.ReadAll(r)
	return data
}

// decodeBase64 ignores everything that is not in the alphabet (line breaks, padding, junk) and a
// dangling last character.
func decodeBase64(raw []byte) []byte {
	clean := make([]byte, 0, len(raw))
	for _, b := range raw {
		if b >= 'A' && b <= 'Z' || b >= 'a' && b <= 'z' || b >= '0' && b <= '9' || b == '+' || b == '/' {
			clean = append(clean, b)
		}
	}
	if len(clean)%4 == 1 {
		clean = clean[:len(clean)-1]
	}
	out := make([]byte, base64.RawStdEncoding.DecodedLen(len(clean)))
	n, _ := base64.RawStdEncoding.Decode(out, clean)
	return out[:n]
}

// --- Parsing incoming mails ---------------------------------------------------------------------

// Attachment: a file in or on a mail.
type Attachment struct {
	Filename string
	MIME     string
	Data     []byte // incoming: only when the parse asked for it
}

type parsed struct {
	Header      mail.Header
	Plain, HTML string
	Attachments []Attachment
}

type parseOptions struct {
	textLimit int // characters of text kept per kind (0 = no limit)
	// keep says whether the data of an attachment is wanted; nil = never (they are only listed).
	keep func(a Attachment) bool
}

// parseMessage splits a raw mail into its headers, plain text, HTML and attachments. Multipart
// nesting, quoted-printable, base64 and the charsets of toUTF8 are handled; a mail cut off in the
// middle (a partial fetch) gives what came before the cut.
func parseMessage(raw []byte, o parseOptions) (*parsed, error) {
	msg, err := mail.ReadMessage(bytes.NewReader(raw))
	if err != nil {
		return nil, err
	}
	p := &parsed{Header: msg.Header}
	p.walk(textproto.MIMEHeader(msg.Header), msg.Body, 0, o)
	return p, nil
}

// partName: the file name of a part (Content-Disposition filename or Content-Type name, RFC 2231
// and RFC 2047 decoded).
func partName(h textproto.MIMEHeader, typeParams map[string]string) string {
	name := typeParams["name"]
	if _, params, err := mime.ParseMediaType(h.Get("Content-Disposition")); err == nil && params["filename"] != "" {
		name = params["filename"]
	} else if cd := h.Get("Content-Disposition"); name == "" && cd != "" {
		// Unparsable parameters ("filename=a b.pdf"): take what follows filename=.
		if _, after, ok := strings.Cut(strings.ToLower(cd), "filename="); ok {
			name = strings.Trim(strings.TrimSpace(cd[len(cd)-len(after):]), `";`)
		}
	}
	return decodeHeader(name)
}

func (p *parsed) walk(h textproto.MIMEHeader, body io.Reader, depth int, o parseOptions) {
	mediaType, params, err := mime.ParseMediaType(h.Get("Content-Type"))
	if mediaType == "" {
		if err != nil && h.Get("Content-Type") != "" {
			mediaType = strings.ToLower(strings.TrimSpace(strings.Split(h.Get("Content-Type"), ";")[0]))
		}
		if mediaType == "" {
			mediaType = "text/plain"
		}
	}
	if strings.HasPrefix(mediaType, "multipart/") && params["boundary"] != "" && depth < 8 {
		mr := multipart.NewReader(body, params["boundary"])
		for {
			part, err := mr.NextRawPart()
			if err != nil {
				return
			}
			p.walk(part.Header, part, depth+1, o)
		}
	}
	filename := partName(h, params)
	disposition, _, _ := mime.ParseMediaType(h.Get("Content-Disposition"))
	isText := mediaType == "text/plain" || mediaType == "text/html"
	if !isText || disposition == "attachment" || filename != "" {
		a := Attachment{Filename: filename, MIME: mediaType}
		if a.Filename == "" && strings.HasPrefix(mediaType, "multipart/") {
			return // a multipart that could not be opened
		}
		if o.keep != nil && o.keep(a) {
			a.Data = decodeTransfer(body, h.Get("Content-Transfer-Encoding"), 0)
		}
		p.Attachments = append(p.Attachments, a)
		return
	}
	limit := int64(0)
	if o.textLimit > 0 {
		limit = int64(o.textLimit) * 4 // encoded text is longer than its characters
	}
	text := toUTF8(params["charset"], decodeTransfer(body, h.Get("Content-Transfer-Encoding"), limit))
	text = strings.ReplaceAll(text, "\r\n", "\n")
	target := &p.Plain
	if mediaType == "text/html" {
		target = &p.HTML
	}
	if *target != "" {
		*target += "\n"
	}
	*target += text
}

// Text is the text of the mail: the plain part if there is one, else the HTML part as text.
func (p *parsed) Text() string {
	if p.Plain != "" {
		return CleanText(p.Plain)
	}
	return HTMLToText(p.HTML)
}

// headerText is a decoded header value of the mail.
func (p *parsed) headerText(name string) string { return decodeHeader(p.Header.Get(name)) }

// parseHeaderBlock reads the header lines a FETCH HEADER.FIELDS returns.
func parseHeaderBlock(block string) mail.Header {
	block = strings.TrimRight(block, "\r\n") + "\r\n\r\n"
	h, _ := textproto.NewReader(bufio.NewReader(strings.NewReader(block))).ReadMIMEHeader()
	return mail.Header(h)
}

// parseDate reads an RFC 5322 date ("Sat, 3 Oct 2026 21:03:26 +0200"); the zero time if unreadable.
func parseDate(date string) time.Time {
	date = strings.TrimSpace(date)
	if t, err := mail.ParseDate(date); err == nil {
		return t
	}
	// Trailing comments and odd zone names ("... +0200 (CEST)", "... GMT+2") are common.
	if i := strings.Index(date, " ("); i > 0 {
		if t, err := mail.ParseDate(date[:i]); err == nil {
			return t
		}
	}
	return time.Time{}
}

// --- Building outgoing mails --------------------------------------------------------------------

// outMessage is a mail the assistant writes.
type outMessage struct {
	From, To, Subject string
	Cc, ReplyTo       string      // optional
	Text, HTML        string      // both: multipart/alternative; only Text: a single text part
	Attachment        *Attachment // optional
	Date              time.Time
	Base64Text        bool // a single text part as base64 (long lines, umlauts) instead of quoted-printable
}

func randomHex(n int) string {
	b := make([]byte, n)
	rand.Read(b)
	return hex.EncodeToString(b)
}

// wrap64 writes base64 in lines of 76 characters.
func wrap64(w io.Writer, data []byte) {
	enc := base64.StdEncoding.EncodeToString(data)
	for len(enc) > 76 {
		io.WriteString(w, enc[:76]+"\r\n")
		enc = enc[76:]
	}
	io.WriteString(w, enc+"\r\n")
}

func writeQP(w io.Writer, text string) {
	qp := quotedprintable.NewWriter(w)
	qp.Write([]byte(strings.ReplaceAll(strings.ReplaceAll(text, "\r\n", "\n"), "\n", "\r\n")))
	qp.Close()
	io.WriteString(w, "\r\n")
}

func (m outMessage) textPart(w io.Writer, contentType, text string, b64 bool) {
	if b64 {
		fmt.Fprintf(w, "Content-Type: %s; charset=utf-8\r\nContent-Transfer-Encoding: base64\r\n\r\n", contentType)
		wrap64(w, []byte(strings.ReplaceAll(strings.ReplaceAll(text, "\r\n", "\n"), "\n", "\r\n")))
		return
	}
	fmt.Fprintf(w, "Content-Type: %s; charset=utf-8\r\nContent-Transfer-Encoding: quoted-printable\r\n\r\n", contentType)
	writeQP(w, text)
}

// bytes renders the mail with CRLF line ends, ready for SMTP DATA or IMAP APPEND.
func (m outMessage) bytes() []byte {
	var b bytes.Buffer
	date := m.Date
	if date.IsZero() {
		date = time.Now()
	}
	domain := "localhost"
	if _, after, ok := strings.Cut(m.From[strings.LastIndexByte(m.From, '<')+1:], "@"); ok {
		domain = strings.TrimRight(after, "> ")
	}
	fmt.Fprintf(&b, "Date: %s\r\nMessage-ID: <%s@%s>\r\nMIME-Version: 1.0\r\n", date.Format(time.RFC1123Z), randomHex(12), domain)
	oneLine := strings.NewReplacer("\r", " ", "\n", " ")
	fmt.Fprintf(&b, "Subject: %s\r\nFrom: %s\r\nTo: %s\r\n", encodeHeader(m.Subject), m.From, oneLine.Replace(m.To))
	if m.Cc != "" {
		fmt.Fprintf(&b, "Cc: %s\r\n", oneLine.Replace(m.Cc))
	}
	if m.ReplyTo != "" {
		fmt.Fprintf(&b, "Reply-To: %s\r\n", oneLine.Replace(m.ReplyTo))
	}

	body := func(w *bytes.Buffer) {
		if m.HTML == "" {
			m.textPart(w, "text/plain", m.Text, m.Base64Text)
			return
		}
		boundary := "alt_" + randomHex(12)
		fmt.Fprintf(w, "Content-Type: multipart/alternative; boundary=\"%s\"\r\n\r\n", boundary)
		fmt.Fprintf(w, "--%s\r\n", boundary)
		m.textPart(w, "text/plain", m.Text, false)
		fmt.Fprintf(w, "--%s\r\n", boundary)
		m.textPart(w, "text/html", m.HTML, false)
		fmt.Fprintf(w, "--%s--\r\n", boundary)
	}
	if m.Attachment == nil {
		body(&b)
		return b.Bytes()
	}
	boundary := "mix_" + randomHex(12)
	fmt.Fprintf(&b, "Content-Type: multipart/mixed; boundary=\"%s\"\r\n\r\n--%s\r\n", boundary, boundary)
	body(&b)
	name := encodeHeader(m.Attachment.Filename)
	mt := m.Attachment.MIME
	if mt == "" {
		mt = "application/octet-stream"
	}
	fmt.Fprintf(&b, "--%s\r\nContent-Type: %s; name=\"%s\"\r\nContent-Disposition: attachment; filename=\"%s\"\r\n"+
		"Content-Transfer-Encoding: base64\r\n\r\n", boundary, mt, name, name)
	wrap64(&b, m.Attachment.Data)
	fmt.Fprintf(&b, "--%s--\r\n", boundary)
	return b.Bytes()
}
