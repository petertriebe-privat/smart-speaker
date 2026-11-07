package mailwatch

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/url"
	"strconv"
	"strings"
	"time"

	"echo-assistant/internal/google"
	"echo-assistant/internal/mail"
	"echo-assistant/internal/textutil"
)

const (
	gmailBase = "https://gmail.googleapis.com/gmail/v1/users/me/"
	bodyBytes = 60000 // of a Gmail part before it becomes text
)

// gmailDo is a Gmail REST call for the mailbox's account (box.Secret = its refresh token, ""
// the admin's). Returns the HTTP status (200 for every 2xx; the API's status with an error;
// 0 when the answer is not readable or the account is missing).
func (s *Service) gmailDo(ctx context.Context, box Mailbox, method, path string, in, out any) (int, error) {
	if s.Google == nil {
		return 0, errors.New("Error: Google is not configured")
	}
	var body []byte
	contentType := ""
	if in != nil {
		var err error
		if body, err = json.Marshal(in); err != nil {
			return 0, err
		}
		contentType = "application/json"
	} else if method != "GET" {
		body, contentType = []byte{}, "application/json"
	}
	data, err := s.Google.Request(ctx, method, gmailBase+path, contentType, body, box.Secret)
	if err != nil {
		return google.StatusOf(err), err
	}
	if out != nil && len(data) > 0 && json.Unmarshal(data, out) != nil {
		return 0, errors.New("Error: Gmail answer not readable")
	}
	return 200, nil
}

type gPart struct {
	MimeType string `json:"mimeType"`
	Filename string `json:"filename"`
	Headers  []struct {
		Name  string `json:"name"`
		Value string `json:"value"`
	} `json:"headers"`
	Body struct {
		Data         string `json:"data"`
		Size         int    `json:"size"`
		AttachmentID string `json:"attachmentId"`
	} `json:"body"`
	Parts []gPart `json:"parts"`
}

type gMessage struct {
	ID           string   `json:"id"`
	LabelIDs     []string `json:"labelIds"`
	InternalDate string   `json:"internalDate"`
	Snippet      string   `json:"snippet"`
	Payload      gPart    `json:"payload"`
}

func (p *gPart) header(name string) string {
	for _, h := range p.Headers {
		if strings.EqualFold(h.Name, name) {
			return h.Value
		}
	}
	return ""
}

// decodeBase64URL decodes Gmail's base64url (padding or not) into at most max bytes (max < 0:
// all). ok=false if it is not base64.
func decodeBase64URL(data string, max int) ([]byte, bool) {
	raw, err := base64.RawURLEncoding.DecodeString(strings.TrimRight(data, "="))
	if err != nil {
		return nil, false
	}
	if max >= 0 && len(raw) > max {
		raw = raw[:max]
	}
	return raw, true
}

// decodePart: a base64url body part as text, at most bodyBytes, Latin-1 converted to UTF-8.
func decodePart(data string, latin1 bool) string {
	raw, ok := decodeBase64URL(data, bodyBytes)
	if !ok {
		return ""
	}
	if !latin1 {
		return string(raw)
	}
	var b strings.Builder
	for _, c := range raw {
		b.WriteRune(rune(c))
	}
	return b.String()
}

// walkParts finds the first text/plain and text/html part of a message (nested multiparts) and
// the attachment names ("name (type), ...").
func walkParts(p *gPart, plain, html, attachments *string) {
	if p.Filename != "" {
		if *attachments != "" {
			*attachments += ", "
		}
		*attachments += p.Filename + " (" + p.MimeType + ")"
		return
	}
	if p.Body.Data != "" && (p.MimeType == "text/plain" || p.MimeType == "text/html") {
		ct := strings.ToLower(p.header("Content-Type"))
		latin1 := strings.Contains(ct, "iso-8859-1") || strings.Contains(ct, "windows-1252") || strings.Contains(ct, "iso-8859-15")
		target := plain
		if p.MimeType == "text/html" {
			target = html
		}
		if *target == "" {
			*target = decodePart(p.Body.Data, latin1)
		}
	}
	for i := range p.Parts {
		walkParts(&p.Parts[i], plain, html, attachments)
	}
}

// gmailMessage reads one message: headers, hints and, with textLimit, the start of its text and
// the attachments; without, only Gmail's preview (metadata only: fast). Returns the HTTP status.
func (s *Service) gmailMessage(ctx context.Context, box Mailbox, id string, textLimit int) (mail.Incoming, int, error) {
	path := "messages/" + id + "?format=metadata&metadataHeaders=From&metadataHeaders=To&metadataHeaders=Subject" +
		"&fields=id,labelIds,internalDate,snippet,payload/headers"
	if textLimit > 0 {
		path = "messages/" + id + "?format=full&fields=id,labelIds,internalDate,payload"
	}
	var msg gMessage
	status, err := s.gmailDo(ctx, box, "GET", path, nil, &msg)
	if err != nil {
		return mail.Incoming{}, status, err
	}
	return incomingFromGmail(&msg, id, textLimit), 200, nil
}

func incomingFromGmail(msg *gMessage, id string, textLimit int) mail.Incoming {
	p := &msg.Payload
	m := mail.Incoming{Ref: id, From: p.header("From"), To: p.header("To"), Subject: p.header("Subject")}
	if ms, err := strconv.ParseInt(msg.InternalDate, 10, 64); err == nil && ms > 0 {
		m.At = time.UnixMilli(ms)
		m.Date = m.At.Local().Format("02.01.2006 15:04")
	} else {
		m.Date = p.header("Date")
	}
	if p.header("List-Unsubscribe") != "" || p.header("List-Id") != "" {
		m.Hints = "mailing list (List-Unsubscribe)"
	}
	if automatic := p.header("Precedence") + p.header("Auto-Submitted"); automatic != "" {
		if m.Hints != "" {
			m.Hints += ", "
		}
		m.Hints += "automatic: " + automatic
	}
	var labels []string
	for _, l := range msg.LabelIDs {
		if strings.HasPrefix(l, "CATEGORY_") || l == "IMPORTANT" || l == "SENT" {
			labels = append(labels, l)
		}
	}
	if len(labels) > 0 {
		if m.Hints != "" {
			m.Hints += ", "
		}
		m.Hints += "Gmail labels: " + strings.Join(labels, " ")
	}
	if textLimit > 0 {
		var plain, html string
		walkParts(p, &plain, &html, &m.Attachments)
		if plain != "" {
			m.Text = mail.CleanText(plain)
		} else {
			m.Text = mail.HTMLToText(html)
		}
		m.Text = textutil.Prefix(m.Text, textLimit)
	} else {
		m.Text = mail.HTMLToText(msg.Snippet) // Gmail escapes the preview as HTML
	}
	m.From, m.To, m.Subject, m.Text = textutil.Repair(m.From), textutil.Repair(m.To), textutil.Repair(m.Subject), textutil.Repair(m.Text)
	return m
}

func (s *Service) gmailStart(ctx context.Context, box Mailbox) (string, error) {
	var profile struct {
		HistoryID string `json:"historyId"`
	}
	if _, err := s.gmailDo(ctx, box, "GET", "profile?fields=historyId", nil, &profile); err != nil {
		return "", err
	}
	if profile.HistoryID == "" {
		return "", errors.New("Error: Gmail sent no historyId")
	}
	return profile.HistoryID, nil
}

// address is the mailbox's address: the IMAP user, for Gmail asked once and kept in the state
// ("" if Gmail cannot be reached).
func (s *Service) address(ctx context.Context, box Mailbox) string {
	if !box.Gmail() {
		return box.User
	}
	s.mu.Lock()
	if b := s.st.Boxes[box.key()]; b != nil && b.Address != "" {
		known := b.Address
		s.mu.Unlock()
		return known
	}
	s.mu.Unlock()
	var profile struct {
		EmailAddress string `json:"emailAddress"`
	}
	if _, err := s.gmailDo(ctx, box, "GET", "profile?fields=emailAddress", nil, &profile); err != nil {
		return ""
	}
	s.mu.Lock()
	s.st.box(box.key()).Address = profile.EmailAddress
	s.save()
	s.mu.Unlock()
	return profile.EmailAddress
}

// gmailNew returns the new mails in the inbox since `history` (Gmail's history) and the new
// history id. The first time only the starting point.
func (s *Service) gmailNew(ctx context.Context, box Mailbox, history string) ([]mail.Incoming, string, error) {
	if history == "" {
		h, err := s.gmailStart(ctx, box)
		return nil, h, err
	}
	var changes struct {
		History []struct {
			MessagesAdded []struct {
				Message struct {
					ID       string   `json:"id"`
					LabelIDs []string `json:"labelIds"`
				} `json:"message"`
			} `json:"messagesAdded"`
		} `json:"history"`
		HistoryID string `json:"historyId"`
	}
	status, err := s.gmailDo(ctx, box, "GET", "history?startHistoryId="+url.QueryEscape(history)+
		"&historyTypes=messageAdded&labelId=INBOX&maxResults=100&fields=history/messagesAdded/message(id,labelIds),historyId",
		nil, &changes)
	if status == 404 { // the starting point is too old (about a week): start from now
		log.Printf("[MailWatch] %s: history expired, starting anew", box.Label)
		h, err := s.gmailStart(ctx, box)
		return nil, h, err
	}
	if err != nil {
		return nil, history, err
	}
	var ids []string
	for _, entry := range changes.History {
		for _, added := range entry.MessagesAdded {
			own := false // drafts and the person's own mails to themselves
			for _, l := range added.Message.LabelIDs {
				own = own || l == "DRAFT" || l == "SENT"
			}
			id := added.Message.ID
			if id != "" && !own && !contains(ids, id) {
				ids = append(ids, id)
			}
		}
	}
	batch := lim("MAILWATCH_BATCH", 10)
	if len(ids) > batch {
		log.Printf("[MailWatch] %s: %d new mails, the oldest %d skipped", box.Label, len(ids), len(ids)-batch)
		ids = ids[len(ids)-batch:]
	}
	var mails []mail.Incoming
	for _, id := range ids {
		m, status, err := s.gmailMessage(ctx, box, id, fetchChars())
		if status == 404 {
			continue // deleted meanwhile
		}
		if err != nil {
			return nil, history, err
		}
		mails = append(mails, m)
	}
	if changes.HistoryID != "" {
		history = changes.HistoryID
	}
	return mails, history, nil
}

// gmailQuery percent-encodes a search as the M5 does (alphanumerics and - _ . : stay).
func gmailQuery(q string) string {
	var b strings.Builder
	for i := 0; i < len(q); i++ {
		c := q[i]
		if c >= '0' && c <= '9' || c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c == '-' || c == '_' || c == '.' || c == ':' {
			b.WriteByte(c)
		} else {
			fmt.Fprintf(&b, "%%%02X", c)
		}
	}
	return b.String()
}

// gmailSearch finds mails matching Gmail's search `query` (its own syntax and full text) of the
// last `n` days, newest first, headers and Gmail's preview.
func (s *Service) gmailSearch(ctx context.Context, box Mailbox, query string, n int) ([]mail.Incoming, error) {
	q := strings.TrimSpace(query + " newer_than:" + strconv.Itoa(n) + "d")
	var list struct {
		Messages []struct {
			ID string `json:"id"`
		} `json:"messages"`
	}
	if _, err := s.gmailDo(ctx, box, "GET", "messages?q="+gmailQuery(q)+"&maxResults="+strconv.Itoa(lim("MAILWATCH_SEARCH_MAX", 40))+
		"&fields=messages/id", nil, &list); err != nil {
		return nil, err
	}
	var mails []mail.Incoming
	for _, entry := range list.Messages {
		m, status, err := s.gmailMessage(ctx, box, entry.ID, 0)
		if status == 404 {
			continue
		}
		if err != nil {
			return nil, err
		}
		mails = append(mails, m)
	}
	return mails, nil
}

// gmailAttachmentPart: the part of a message whose file name contains `wanted` (lower case; ""
// = the first attachment), nested multiparts included; the names seen go into names.
func gmailAttachmentPart(p *gPart, wanted string, names *[]string) *gPart {
	if p.Filename != "" {
		*names = append(*names, p.Filename)
		if strings.Contains(strings.ToLower(p.Filename), wanted) {
			return p
		}
	}
	for i := range p.Parts {
		if found := gmailAttachmentPart(&p.Parts[i], wanted, names); found != nil {
			return found
		}
	}
	return nil
}

func contains(list []string, item string) bool {
	for _, s := range list {
		if s == item {
			return true
		}
	}
	return false
}
