package mail

import (
	"strings"
	"time"

	"echo-assistant/internal/textutil"
)

// Other people's mailboxes (for the mail watch, assistant/mail_watch on the M5), read only except
// MoveOther. Same IMAP port as the assistant's own mailbox (issue #30). Errors carry "Error: ..." texts.

// Account is an IMAP account other than the assistant's own.
type Account struct{ Host, User, Password string }

// Incoming is a mail of a watched mailbox, as text for the classifier.
type Incoming struct {
	Ref         string // IMAP UID (or "<uid>@<folder>")
	From, To    string
	Subject     string
	Date        string    // "02.01.2006 15:04" when the Date header was readable
	At          time.Time // when it was sent, zero if unknown
	Hints       string    // signals of bulk mail (mailing list, automatic)
	Attachments string    // "name (type), ..."
	Text        string    // the start of the body as plain text
}

// session holds the assistant's mailbox lock and a logged-in session on another account.
type session struct {
	c      *imapClient
	unlock func()
}

func (s session) close() {
	s.c.logout()
	s.unlock()
}

// openOther logs in to another account and selects `folder` ("" = none), by default read only
// (EXAMINE: fetching leaves the owner's flags, e.g. seen, as they are).
func (s *Service) openOther(a Account, folder string, readOnly bool) (session, mailboxInfo, error) {
	unlock := s.lock()
	c, err := s.login(&a)
	if err != nil {
		unlock()
		return session{}, mailboxInfo{}, err
	}
	var info mailboxInfo
	if folder != "" {
		if info, err = c.selectFolder(folder, readOnly); err != nil {
			status := c.status
			c.logout()
			unlock()
			return session{}, mailboxInfo{}, fail("folder %s not available (%s)", folder, oneLine(status, nil))
		}
	}
	return session{c, unlock}, info, nil
}

// fetchOther reads the headers of one mail; with textLimit also the start of its text and the
// attachments. False if the mail is not there.
func (s *Service) fetchOther(c *imapClient, uid uint32, textLimit int) (Incoming, bool) {
	headers, err := c.fetchHeaders([]uint32{uid},
		"FROM TO SUBJECT DATE LIST-ID LIST-UNSUBSCRIBE PRECEDENCE AUTO-SUBMITTED")
	block, ok := headers[uid]
	if err != nil || !ok {
		return Incoming{}, false
	}
	h := parseHeaderBlock(block)
	m := Incoming{Ref: uitoa(uid), From: decodeHeader(h.Get("From")), To: decodeHeader(h.Get("To")),
		Subject: decodeHeader(h.Get("Subject")), Date: strings.TrimSpace(h.Get("Date"))}
	m.At = parseDate(m.Date)
	if !m.At.IsZero() {
		m.Date = m.At.Local().Format("02.01.2006 15:04")
	}
	if h.Get("List-Unsubscribe") != "" || h.Get("List-Id") != "" {
		m.Hints = "mailing list (List-Unsubscribe)"
	}
	if precedence := strings.TrimSpace(h.Get("Precedence") + h.Get("Auto-Submitted")); precedence != "" {
		if m.Hints != "" {
			m.Hints += ", "
		}
		m.Hints += "automatic: " + precedence
	}
	if textLimit > 0 {
		// HTML loses most of its length as text.
		if p, _ := c.fetchParsed(uid, parseOptions{textLimit: textLimit * 4}, bodyFetchLimit); p != nil {
			m.Text = textutil.Prefix(p.Text(), textLimit)
			m.Attachments = attachmentList(p)
		}
	}
	m.From, m.To, m.Subject, m.Text = textutil.Repair(m.From), textutil.Repair(m.To), textutil.Repair(m.Subject), textutil.Repair(m.Text)
	return m, true
}

// FetchNew returns the new mails in the inbox of another account, read only. `validity` / `last`:
// the state of the last check (UIDVALIDITY, highest UID seen); on the first check (or after the
// server renumbered) they are set to the newest mail and nothing is returned. At most `max` mails,
// oldest first, each with up to `textLimit` bytes of text; `last` moves past them.
func (s *Service) FetchNew(a Account, validity, last *uint32, max, textLimit int) ([]Incoming, error) {
	sess, info, err := s.openOther(a, "INBOX", true)
	if err != nil {
		return nil, err
	}
	defer sess.close()
	if info.UIDValidity == 0 || info.UIDNext == 0 {
		return nil, fail("the server sent no UIDVALIDITY / UIDNEXT")
	}
	if info.UIDValidity != *validity { // first check, or the server renumbered: start from now
		*validity = info.UIDValidity
		*last = info.UIDNext - 1
		return nil, nil
	}
	if info.UIDNext-1 <= *last {
		return nil, nil
	}
	// "n:*" also matches the newest mail when it is below n: filtered.
	uids, _, err := sess.c.search("UID " + uitoa(*last+1) + ":*")
	if err != nil {
		return nil, fail("mailbox not reachable (%v)", err)
	}
	var mails []Incoming
	for _, uid := range uids {
		if uid <= *last {
			continue
		}
		if len(mails) >= max {
			break
		}
		if m, ok := s.fetchOther(sess.c, uid, textLimit); ok {
			mails = append(mails, m)
		}
		*last = uid
	}
	return mails, nil
}

// searchWords builds the search for the mails of the last `days` days containing every word
// (headers or text; quotes would end the string).
func searchWords(words string, days int, now time.Time) string {
	criteria := "SINCE " + imapDate(now.AddDate(0, 0, -days))
	for _, word := range strings.FieldsFunc(words, func(r rune) bool { return r == ' ' || r == ',' || r == ';' }) {
		if word = searchString(word); word != "" {
			criteria += ` TEXT "` + word + `"`
		}
	}
	return criteria
}

// SearchOther finds mails of the last `days` days in the inbox, then in `folders` (server names),
// containing every word of `words` (the server's IMAP search; "" = all), newest first per folder,
// at most `max` per folder, headers only. A mail outside the inbox has the ref "<uid>@<folder>".
func (s *Service) SearchOther(a Account, words string, days, max int, folders []string) ([]Incoming, error) {
	sess, _, err := s.openOther(a, "INBOX", true)
	if err != nil {
		return nil, err
	}
	defer sess.close()
	criteria := searchWords(words, days, time.Now())
	var mails []Incoming
	for f := 0; f <= len(folders); f++ {
		// A folder that is gone (renamed by the owner) is skipped; the inbox is already selected.
		if f > 0 {
			if _, err := sess.c.selectFolder(folders[f-1], true); err != nil {
				continue
			}
		}
		uids, found, err := sess.c.search(criteria)
		// No SEARCH line at all: the search failed (a first full-text search once timed out, as in
		// a folder new to the server's index), not "nothing". In a sorting folder: skipped.
		if err != nil || !found {
			if f > 0 {
				continue
			}
			status := oneLine(sess.c.status, err)
			if status == "" {
				status = "no answer"
			}
			return nil, fail("the mail server's search failed (%s); try again", status)
		}
		count := 0
		for i := len(uids) - 1; i >= 0 && count < max; i-- { // newest first
			m, ok := s.fetchOther(sess.c, uids[i], 0)
			if !ok {
				continue
			}
			if f > 0 {
				m.Ref += "@" + folders[f-1]
			}
			mails = append(mails, m)
			count++
		}
	}
	return mails, nil
}

// ReadOther reads one mail of the account's `folder` (server name) with up to `textLimit` bytes of
// text. Read only.
func (s *Service) ReadOther(a Account, folder string, uid uint32, textLimit int) (Incoming, error) {
	sess, _, err := s.openOther(a, folder, true)
	if err != nil {
		return Incoming{}, err
	}
	defer sess.close()
	m, ok := s.fetchOther(sess.c, uid, textLimit)
	if !ok {
		return Incoming{}, fail("mail %d not found", uid)
	}
	return m, nil
}

// ListOther returns the UIDs of the inbox's mails of the last `days` days, oldest first. Read only.
func (s *Service) ListOther(a Account, days int) ([]uint32, error) {
	sess, _, err := s.openOther(a, "INBOX", true)
	if err != nil {
		return nil, err
	}
	defer sess.close()
	uids, _, err := sess.c.search("SINCE " + imapDate(time.Now().AddDate(0, 0, -days)))
	if err != nil {
		return nil, fail("mailbox not reachable (%v)", err)
	}
	return uids, nil
}

// FetchOthers reads several mails of the inbox in one session, each with up to `textLimit` bytes
// of text. Read only.
func (s *Service) FetchOthers(a Account, uids []uint32, textLimit int) ([]Incoming, error) {
	sess, _, err := s.openOther(a, "INBOX", true)
	if err != nil {
		return nil, err
	}
	defer sess.close()
	var mails []Incoming
	for _, uid := range uids {
		if m, ok := s.fetchOther(sess.c, uid, textLimit); ok {
			mails = append(mails, m)
		}
	}
	return mails, nil
}

// --- Attachments ----------------------------------------------------------------------------------

// AttachmentInfo describes a downloaded attachment.
type AttachmentInfo struct{ Filename, MIME string }

// pickAttachment finds the file in a mail: the first whose name contains `name` (any case; ""
// = the first one). The error names the attachments there are.
func pickAttachment(p *parsed, name string) (*Attachment, error) {
	wanted := strings.ToLower(name)
	var names []string
	var chosen *Attachment
	for i := range p.Attachments {
		a := &p.Attachments[i]
		if a.Filename == "" {
			continue
		}
		names = append(names, a.Filename)
		if chosen == nil && strings.Contains(strings.ToLower(a.Filename), wanted) {
			chosen = a
		}
	}
	if chosen != nil {
		return chosen, nil
	}
	if len(names) > 0 {
		return nil, fail("no attachment \"%s\"; the mail has: %s", name, strings.Join(names, ", "))
	}
	return nil, fail("the mail has no attachments.")
}

// fetchAttachment downloads one attachment of mail `uid` in the selected folder, at most maxBytes.
func fetchAttachment(c *imapClient, uid uint32, name string, maxBytes int) ([]byte, AttachmentInfo, error) {
	// Base64 makes a file 4/3 of its size; text and headers come along.
	limit := maxBytes/3*4 + 256<<10
	data, err := c.fetch(uitoa(uid), "UID RFC822.SIZE", commandTimeout)
	if err != nil {
		return nil, AttachmentInfo{}, fail("mail %d not found (%s)", uid, oneLine(c.status, err))
	}
	if len(data) == 0 {
		return nil, AttachmentInfo{}, fail("mail %d not found (%s)", uid, oneLine(c.status, nil))
	}
	if size := int(data[0].Size()); size > limit {
		return nil, AttachmentInfo{}, fail("the mail is too large (%d KB, at most %d KB for a file).", size/1024, maxBytes/1024)
	}
	// Only the wanted attachment is decoded (the first whose name matches), as pickAttachment
	// will choose it.
	wanted, taken := strings.ToLower(name), false
	p, err := c.fetchParsed(uid, parseOptions{keep: func(a Attachment) bool {
		if taken || a.Filename == "" || !strings.Contains(strings.ToLower(a.Filename), wanted) {
			return false
		}
		taken = true
		return true
	}}, limit)
	if err != nil || p == nil {
		return nil, AttachmentInfo{}, fail("mail %d not found (%s)", uid, oneLine(c.status, err))
	}
	a, err := pickAttachment(p, name)
	if err != nil {
		return nil, AttachmentInfo{}, err
	}
	if len(a.Data) > maxBytes {
		return nil, AttachmentInfo{}, fail("%s is too large (at most %d KB).", a.Filename, maxBytes/1024)
	}
	if len(a.Data) == 0 {
		return nil, AttachmentInfo{}, fail("%s could not be downloaded.", a.Filename)
	}
	return a.Data, AttachmentInfo{a.Filename, a.MIME}, nil
}

// Attachment downloads one attachment of mail `uid` in the assistant's inbox (or sent folder): the first
// whose file name contains `name` (any case; "" = the first), at most `maxBytes`. Read only (the
// mail stays unread). The error names the attachments there are.
//
// Difference to the M5: a mail larger than maxBytes/3*4 + 256 KB in total is refused as a whole
// (the M5 compared only the attachment's own size).
func (s *Service) Attachment(uid uint32, sent bool, name string, maxBytes int) ([]byte, AttachmentInfo, error) {
	unlock := s.lock()
	defer unlock()
	folder := "INBOX"
	if sent {
		folder = s.sentFolder
	}
	c, err := s.openFolder(folder, true)
	if err != nil {
		return nil, AttachmentInfo{}, err
	}
	defer c.logout()
	return fetchAttachment(c, uid, name, maxBytes)
}

// AttachmentOther is Attachment in `folder` (server name) of another account. Read only.
func (s *Service) AttachmentOther(a Account, folder string, uid uint32, name string, maxBytes int) ([]byte, AttachmentInfo, error) {
	sess, _, err := s.openOther(a, folder, true)
	if err != nil {
		return nil, AttachmentInfo{}, err
	}
	defer sess.close()
	return fetchAttachment(sess.c, uid, name, maxBytes)
}

// --- Folders and moving ---------------------------------------------------------------------------

// Folder is a folder of another IMAP account.
type Folder struct {
	Name   string // for people: decoded, the hierarchy separated by "/", without "INBOX/"
	Server string // as the server names it, for commands
	Use    string // special use (RFC 6154): "trash", "archive", "junk", "sent", "drafts" or ""
}

// FoldersOther lists that account's folders (IMAP LIST); separator is the server's hierarchy
// delimiter. Read only.
func (s *Service) FoldersOther(a Account) (folders []Folder, separator string, err error) {
	sess, _, err := s.openOther(a, "", true)
	if err != nil {
		return nil, "", err
	}
	defer sess.close()
	r, err := sess.c.command(`LIST "" "*"`)
	if err != nil || !r.ok() {
		return nil, "", fail("the folder list failed (%s)", oneLine(sess.c.status, err))
	}
	for _, raw := range r.Untagged {
		e, ok := parseListEntry(raw)
		if !ok || strings.Contains(e.Attributes, `\noselect`) || strings.Contains(e.Attributes, `\nonexistent`) {
			continue
		}
		f := Folder{Server: e.Name}
		for _, use := range []string{"trash", "archive", "junk", "sent", "drafts"} {
			if strings.Contains(e.Attributes, `\`+use) {
				f.Use = use
			}
		}
		if e.Delimiter != "" {
			separator = e.Delimiter
		}
		f.Name = decodeFolder(e.Name)
		if separator != "" && separator != "/" {
			f.Name = strings.ReplaceAll(f.Name, separator, "/")
		}
		f.Name = strings.TrimPrefix(f.Name, "INBOX/") // people say "Werbung"
		folders = append(folders, f)
	}
	return folders, separator, nil
}

// Moved is one mail to move (MoveOther).
type Moved struct {
	UID       uint32 // in the source folder; 0 = look it up by MessageID there
	MessageID string // filled in when empty
	NewUID    uint32 // in the target folder after the move, 0 if the server did not say
}

// parseCopyUID reads "[COPYUID <validity> <source uids> <target uids>]" (UIDPLUS) from server
// responses: source UID to target UID.
func parseCopyUID(text string) map[uint32]uint32 {
	code, ok := responseCode(text, "COPYUID")
	if !ok {
		return nil
	}
	fields := strings.Fields(code)
	if len(fields) < 3 {
		return nil
	}
	source, target := uidSet(fields[1]), uidSet(fields[2])
	out := map[uint32]uint32{}
	for i := 0; i < len(source) && i < len(target); i++ {
		out[source[i]] = target[i]
	}
	return out
}

// MoveOther moves mails of another account from folder `from` to `to` (server names; `to` is
// created first when `create`). Mails not found are left out of the result. The only write to a
// household member's mailbox: on their own request or rule (assistant/mail_sort). Never deletes:
// no EXPUNGE of anything but the moved mails (UID EXPUNGE).
func (s *Service) MoveOther(a Account, from, to string, create bool, mails []Moved) ([]Moved, error) {
	sess, _, err := s.openOther(a, from, false) // SELECT: read-write
	if err != nil {
		return nil, err
	}
	defer sess.close()
	c := sess.c
	for i := range mails {
		m := &mails[i]
		if m.UID == 0 && m.MessageID != "" {
			id := strings.NewReplacer(`"`, "", `\`, "").Replace(m.MessageID)
			if found, _, _ := c.search(`HEADER Message-ID "` + id + `"`); len(found) > 0 {
				m.UID = found[len(found)-1]
			}
		}
	}
	kept := mails[:0]
	for _, m := range mails {
		if m.UID != 0 {
			kept = append(kept, m)
		}
	}
	mails = kept
	if len(mails) == 0 {
		return nil, nil
	}
	uids := make([]uint32, len(mails))
	for i, m := range mails {
		uids[i] = m.UID
	}
	set := uidList(uids)
	// The Message-IDs first: the way to find a mail again after the owner or the assistant moved it.
	if blocks, err := c.fetchHeaders(uids, "MESSAGE-ID"); err == nil {
		for i := range mails {
			if mails[i].MessageID == "" {
				mails[i].MessageID = strings.TrimSpace(parseHeaderBlock(blocks[mails[i].UID]).Get("Message-ID"))
			}
		}
	}
	if create {
		c.command("CREATE " + quoteString(to)) // "already exists" is fine
	}
	// Mail programs (Outlook, iOS) show only subscribed folders: a folder the assistant made was invisible.
	if !strings.EqualFold(to, "INBOX") {
		c.command("SUBSCRIBE " + quoteString(to))
	}
	var text strings.Builder
	r, err := c.command("UID MOVE " + set + " " + quoteString(to))
	if err != nil {
		return nil, fail("moving to %s failed (%v)", decodeFolder(to), err)
	}
	if !r.ok() {
		// Without MOVE (RFC 6851): copy, then mark and remove only the copied originals (UIDPLUS).
		// A plain EXPUNGE would also remove what the owner marked deleted: never.
		r, err = c.command("UID COPY " + set + " " + quoteString(to))
		if err != nil || !r.ok() {
			return nil, fail("moving to %s failed (%s)", decodeFolder(to), oneLine(c.status, err))
		}
		copied := r
		if r, err = c.command("UID STORE " + set + " +FLAGS.SILENT (\\Deleted)"); err != nil || !r.ok() {
			return nil, fail("copied to %s, but the server cannot remove only these mails from the source; they are marked deleted there", decodeFolder(to))
		}
		if r, err = c.command("UID EXPUNGE " + set); err != nil || !r.ok() {
			return nil, fail("copied to %s, but the server cannot remove only these mails from the source; they are marked deleted there", decodeFolder(to))
		}
		r = copied
	}
	for _, raw := range r.Untagged {
		text.Write(raw)
	}
	text.WriteString(r.Text)
	if mapping := parseCopyUID(text.String()); mapping != nil {
		for i := range mails {
			mails[i].NewUID = mapping[mails[i].UID]
		}
	}
	return mails, nil
}
