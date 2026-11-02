package whatsapp

// Sending outside WhatsApp's 24-hour service window (#34). WhatsApp delivers free text, images
// and files only within 24 hours of the person's last message to the household's number; outside
// it only an approved template goes through. The assistant keeps the time of each person's last message
// (whatsapp_window.json; Meta reports a send outside the window only later by a status webhook,
// so the send result cannot be trusted for it). Outside the window he sends the template
// WHATSAPP_TEMPLATE (language WHATSAPP_TEMPLATE_LANG) with the message as its one body
// parameter, and what does not fit (a longer text, an image, a file) waits in the outbox
// (whatsapp_outbox.json) until the person writes back; then it goes out first.
//
// The template is created once in WhatsApp Manager (category Utility), body for example:
// "Hallo, hier ist the assistant mit einer Nachricht für dich: {{1}} Antworte einfach, dann schicke ich
// dir alles Weitere." Meta charges utility templates outside the window per message.

import (
	"bytes"
	"context"
	"mime/multipart"
	"net/textproto"
	"regexp"
	"strings"
	"time"

	"echo-assistant/internal/prompts"
	"echo-assistant/internal/store"
	"echo-assistant/internal/textutil"
)

const (
	windowFile = "whatsapp_window.json"
	outboxFile = "whatsapp_outbox.json"
	// Uploaded media stay at Meta for 30 days; what waits longer is dropped.
	outboxMaxAge = 29 * 24 * time.Hour
)

// queued is one message waiting in the outbox for a person's next message.
type queued struct {
	To       string `json:"to"`
	Kind     string `json:"kind"` // text, image, document
	Text     string `json:"text,omitempty"`
	MediaID  string `json:"media_id,omitempty"`
	Caption  string `json:"caption,omitempty"`
	Filename string `json:"filename,omitempty"`
	Queued   int64  `json:"queued"`
}

// window: the time of each WhatsApp id's last message, loaded once.
func (s *Service) window() map[string]int64 {
	if s.lastSeen == nil {
		s.lastSeen = map[string]int64{}
		store.LoadJSON(windowFile, &s.lastSeen)
	}
	return s.lastSeen
}

// seen notes a message from waID (caller holds sendMu).
func (s *Service) seen(waID string, at time.Time) {
	s.window()[waID] = at.Unix()
	if err := store.SaveJSON(windowFile, s.lastSeen); err != nil {
		logf("window not saved: %v", err)
	}
}

// inWindow: free messages reach waID now. A margin of WHATSAPP_WINDOW_MARGIN_S keeps clear of
// the edge (caller holds sendMu).
func (s *Service) inWindow(waID string, now time.Time) bool {
	last, ok := s.window()[waID]
	margin := time.Duration(prompts.Int("WHATSAPP_WINDOW_MARGIN_S", 600)) * time.Second
	return ok && now.Sub(time.Unix(last, 0)) < 24*time.Hour-margin
}

func (s *Service) loadOutbox() []queued {
	var list []queued
	store.LoadJSON(outboxFile, &list)
	return list
}

func (s *Service) saveOutbox(list []queued) {
	if list == nil {
		list = []queued{}
	}
	if err := store.SaveJSON(outboxFile, list); err != nil {
		logf("outbox not saved: %v", err)
	}
}

var spaces = regexp.MustCompile(`\s+`)

// templateParam: a template parameter may hold no new lines, no tabs and no more than four
// spaces in a row; text is cut to WHATSAPP_TEMPLATE_CHARS. cut: not all of it fits.
func templateParam(text string) (param string, cut bool) {
	lines := strings.FieldsFunc(text, func(r rune) bool { return r == '\n' || r == '\r' })
	for i, l := range lines {
		lines[i] = strings.TrimSpace(spaces.ReplaceAllString(l, " "))
	}
	param = strings.Join(nonEmpty(lines), " · ")
	limit := prompts.Int("WHATSAPP_TEMPLATE_CHARS", 900)
	if len(param) > limit {
		return textutil.Prefix(param, limit-4) + " …", true
	}
	return param, false
}

func nonEmpty(list []string) []string {
	out := list[:0]
	for _, s := range list {
		if s != "" {
			out = append(out, s)
		}
	}
	return out
}

// sendTemplate sends the notice template with param as its body parameter.
func (s *Service) sendTemplate(ctx context.Context, to, param string) string {
	_, err := s.graphJSON(ctx, s.messagesPath(), map[string]any{
		"messaging_product": "whatsapp", "to": to, "type": "template",
		"template": map[string]any{
			"name":     prompts.TextOr("WHATSAPP_TEMPLATE", prompts.Slug()+"_nachricht"),
			"language": map[string]any{"code": prompts.TextOr("WHATSAPP_TEMPLATE_LANG", "de")},
			"components": []any{map[string]any{"type": "body",
				"parameters": []any{map[string]any{"type": "text", "text": param}}}},
		},
	})
	if err != nil {
		return "Error: " + err.Error()
	}
	return "sent"
}

// outside delivers item to someone outside the window: the template with preview as its text,
// and item into the outbox unless the template carried all of it (full). Caller holds sendMu.
func (s *Service) outside(ctx context.Context, item queued, preview string, full bool) string {
	param, cut := templateParam(preview)
	if result := s.sendTemplate(ctx, item.To, param); result != "sent" {
		return "Error: outside WhatsApp's 24-hour window, and the template " +
			prompts.TextOr("WHATSAPP_TEMPLATE", prompts.Slug()+"_nachricht") + " failed: " + strings.TrimPrefix(result, "Error: ")
	}
	if full && !cut {
		logf("template to %s", item.To)
		return "sent"
	}
	item.Queued = time.Now().Unix()
	s.saveOutbox(append(s.loadOutbox(), item))
	logf("template to %s, %s waits for the answer", item.To, item.Kind)
	return "sent"
}

// flush sends what waits for waID, now that they wrote (caller holds sendMu).
func (s *Service) flush(ctx context.Context, waID string) {
	list := s.loadOutbox()
	var kept []queued
	now := time.Now()
	sent := 0
	for _, q := range list {
		if now.Sub(time.Unix(q.Queued, 0)) > outboxMaxAge {
			continue
		}
		if q.To != waID {
			kept = append(kept, q)
			continue
		}
		var result string
		switch q.Kind {
		case "text":
			result = s.sendText(ctx, q.To, q.Text)
		default:
			result = s.sendMedia(ctx, q.To, q.Kind, q.MediaID, q.Caption, q.Filename)
		}
		if result != "sent" {
			logf("outbox to %s: %s", q.To, result)
			kept = append(kept, q)
			continue
		}
		sent++
	}
	if len(kept) != len(list) {
		s.saveOutbox(kept)
	}
	if sent > 0 {
		logf("outbox: %d message(s) to %s delivered", sent, waID)
	}
}

// uploadMedia uploads a file to Meta and returns its media id.
func (s *Service) uploadMedia(ctx context.Context, data []byte, filename, mime string) (string, error) {
	var body bytes.Buffer
	w := multipart.NewWriter(&body)
	w.WriteField("messaging_product", "whatsapp")
	w.WriteField("type", mime)
	header := textproto.MIMEHeader{}
	header.Set("Content-Disposition", `form-data; name="file"; filename="`+strings.ReplaceAll(filename, `"`, "")+`"`)
	header.Set("Content-Type", mime)
	part, err := w.CreatePart(header)
	if err != nil {
		return "", err
	}
	part.Write(data)
	w.Close()
	reply, err := s.graph(ctx, "POST", s.phoneNumberID+"/media", w.FormDataContentType(), body.Bytes(), 2*timeout())
	if err != nil {
		return "", err
	}
	media, _ := reply["id"].(string)
	return media, nil
}

// sendMedia sends an uploaded image or document by its media id.
func (s *Service) sendMedia(ctx context.Context, to, kind, mediaID, caption, filename string) string {
	media := map[string]any{"id": mediaID}
	if caption != "" {
		media["caption"] = textutil.Prefix(plainLinks(caption), 1024)
	}
	if kind == "document" && filename != "" {
		media["filename"] = filename
	}
	_, err := s.graphJSON(ctx, s.messagesPath(), map[string]any{
		"messaging_product": "whatsapp", "to": to, "type": kind, kind: media})
	if err != nil {
		return "Error: " + err.Error()
	}
	return "sent"
}

// sendFile uploads and sends an image or document to a person ("" = the admin): at once within
// the window, else the template now and the file when they answer.
func (s *Service) sendFile(person, kind string, data []byte, filename, mime, caption, preview string) string {
	to, errText := s.recipient(person)
	if errText != "" {
		return errText
	}
	ctx := context.Background()
	s.sendMu.Lock()
	defer s.sendMu.Unlock()
	media, err := s.uploadMedia(ctx, data, filename, mime)
	if err != nil {
		return "Error: upload: " + err.Error()
	}
	if s.inWindow(to, time.Now()) {
		return s.sendMedia(ctx, to, kind, media, caption, filename)
	}
	return s.outside(ctx, queued{To: to, Kind: kind, MediaID: media, Caption: caption, Filename: filename}, preview, false)
}

// SendPhoto sends a JPEG with a caption to a person's WhatsApp ("" = the admin). Returns "sent"
// or an error. Outside the window the photo follows the person's next message.
func (s *Service) SendPhoto(jpeg []byte, caption, person string) string {
	return s.sendFile(person, "image", jpeg, "assistant.jpg", "image/jpeg", caption,
		"Ich habe ein Bild für dich: „"+caption+"“.")
}

// SendDocument sends a file as a document to a person's WhatsApp ("" = the admin). Returns
// "sent" or an error. Outside the window the file follows the person's next message.
func (s *Service) SendDocument(data []byte, filename, mime, caption, person string) string {
	preview := "Ich habe eine Datei für dich: „" + filename + "“."
	if caption != "" {
		preview += " " + caption
	}
	return s.sendFile(person, "document", data, filename, mime, caption, preview)
}

// Number: the household's WhatsApp number as Meta shows it and its wa.me link, read once from
// the Graph API; "" while unknown. It is people.Channels.WhatsAppLink.
func (s *Service) Number() (number, link string) {
	s.numberMu.Lock()
	defer s.numberMu.Unlock()
	if s.number == "" && s.Configured() {
		reply, err := s.graph(context.Background(), "GET", s.phoneNumberID+"?fields=display_phone_number", "", nil, timeout())
		if err != nil {
			logf("number: %v", err)
			return "", ""
		}
		s.number, _ = reply["display_phone_number"].(string)
	}
	digits := strings.Map(func(r rune) rune {
		if r >= '0' && r <= '9' {
			return r
		}
		return -1
	}, s.number)
	if digits == "" {
		return "", ""
	}
	return s.number, "https://wa.me/" + digits
}
