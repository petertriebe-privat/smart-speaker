package whatsapp

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime/multipart"
	"net"
	"net/http"
	"net/textproto"
	"net/url"
	"strings"
	"time"

	"echo-assistant/internal/chatcore"
	"echo-assistant/internal/prompts"
	"echo-assistant/internal/textutil"
)

func timeout() time.Duration {
	return time.Duration(prompts.Int("WHATSAPP_TIMEOUT_MS", 30000)) * time.Millisecond
}

const maxReply = 8 << 20

// netError words a transport failure without the URL (*url.Error carries it).
func netError(err error, service string) string {
	var ue *url.Error
	if errors.As(err, &ue) {
		err = ue.Err
	}
	var op *net.OpError
	if errors.As(err, &op) && op.Op == "dial" {
		return "no connection to " + service
	}
	return service + " not reachable (" + err.Error() + ")"
}

// do sends a request with a time limit and returns status and body. The access token goes in
// the header when auth is set: nothing here logs the request.
func (s *Service) do(ctx context.Context, method, target, auth, contentType string, body []byte, limit time.Duration, service string) (int, []byte, error) {
	ctx, cancel := context.WithTimeout(ctx, limit)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, method, target, bytes.NewReader(body))
	if err != nil {
		return 0, nil, errors.New(service + " not reachable (bad request)")
	}
	if auth != "" {
		req.Header.Set("Authorization", "Bearer "+auth)
	}
	req.Header.Set("User-Agent", prompts.Name())
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	resp, err := s.client.Do(req)
	if err != nil {
		return 0, nil, errors.New(netError(err, service))
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, maxReply+1))
	if err != nil {
		return resp.StatusCode, nil, errors.New(service + " not reachable (" + err.Error() + ")")
	}
	return resp.StatusCode, data, nil
}

// graph is one Graph API request (path after the version, e.g. "123/messages"); reply is the
// decoded answer. The error is Meta's message when it gives one.
func (s *Service) graph(ctx context.Context, method, path, contentType string, body []byte, limit time.Duration) (map[string]any, error) {
	status, data, err := s.do(ctx, method, s.graphBase+"/"+prompts.Text("WHATSAPP_GRAPH_VERSION")+"/"+path,
		s.accessToken, contentType, body, limit, "WhatsApp")
	if err != nil {
		return nil, err
	}
	reply := map[string]any{}
	if json.Unmarshal(data, &reply) != nil {
		return nil, fmt.Errorf("answer from WhatsApp not readable (HTTP %d)", status)
	}
	if status >= 200 && status < 300 {
		return reply, nil
	}
	return nil, errors.New(graphError(reply, status))
}

// graphError: Meta's error message with its code, or "HTTP n".
func graphError(reply map[string]any, status int) string {
	e, _ := reply["error"].(map[string]any)
	message, _ := e["message"].(string)
	code, _ := e["code"].(float64)
	if message == "" {
		message = fmt.Sprintf("HTTP %d", status)
	}
	if code != 0 {
		message += fmt.Sprintf(" (%d)", int(code))
	}
	return message
}

func (s *Service) graphJSON(ctx context.Context, path string, body map[string]any) (map[string]any, error) {
	payload, err := json.Marshal(body)
	if err != nil {
		return nil, err
	}
	return s.graph(ctx, "POST", path, "application/json", payload, timeout())
}

func (s *Service) messagesPath() string { return s.phoneNumberID + "/messages" }

// sendText sends text to a WhatsApp id as one or more messages. Returns "sent" or "Error: ...".
func (s *Service) sendText(ctx context.Context, to, text string) string {
	for _, message := range chatcore.Split(plainLinks(text), prompts.Int("WHATSAPP_MESSAGE_CHARS", 4000)) {
		_, err := s.graphJSON(ctx, s.messagesPath(), map[string]any{
			"messaging_product": "whatsapp", "recipient_type": "individual", "to": to, "type": "text",
			"text": map[string]any{"preview_url": true, "body": textutil.Repair(message)},
		})
		if err != nil {
			return "Error: " + err.Error()
		}
	}
	return "sent"
}

// sendVoice sends a voice note (Ogg Opus): uploaded as media, then sent by its id.
func (s *Service) sendVoice(ctx context.Context, to string, ogg []byte) string {
	var body bytes.Buffer
	w := multipart.NewWriter(&body)
	w.WriteField("messaging_product", "whatsapp")
	w.WriteField("type", "audio/ogg")
	header := textproto.MIMEHeader{}
	header.Set("Content-Disposition", `form-data; name="file"; filename="assistant.ogg"`)
	header.Set("Content-Type", "audio/ogg")
	part, err := w.CreatePart(header)
	if err != nil {
		return "Error: " + err.Error()
	}
	part.Write(ogg)
	w.Close()
	reply, err := s.graph(ctx, "POST", s.phoneNumberID+"/media", w.FormDataContentType(), body.Bytes(), 2*timeout())
	if err != nil {
		return "Error: upload: " + err.Error()
	}
	media, _ := reply["id"].(string)
	audio := map[string]any{"id": media, "voice": true} // shown as a voice note, not as an audio file
	message := map[string]any{"messaging_product": "whatsapp", "to": to, "type": "audio", "audio": audio}
	if _, err = s.graphJSON(ctx, s.messagesPath(), message); err != nil {
		// Older API behaviour: without the flag it still arrives as audio.
		delete(audio, "voice")
		_, err = s.graphJSON(ctx, s.messagesPath(), message)
	}
	if err != nil {
		return "Error: " + err.Error()
	}
	return "sent"
}

// markRead gives blue ticks and "schreibt …" while the agent works (WhatsApp shows it up to 25 s
// or until the answer).
func (s *Service) markRead(ctx context.Context, messageID string) {
	_, err := s.graphJSON(ctx, s.messagesPath(), map[string]any{
		"messaging_product": "whatsapp", "status": "read", "message_id": messageID,
		"typing_indicator": map[string]any{"type": "text"},
	})
	if err != nil {
		logf("read receipt: %v", err)
	}
}

// metaHost: the hosts media may be fetched from; the token goes only to Meta.
func metaHost(host string) bool {
	return strings.HasSuffix(host, ".fbsbx.com") || strings.HasSuffix(host, ".facebook.com") ||
		strings.HasSuffix(host, ".whatsapp.net")
}

// download fetches a media file a person sent (voice note, photo, document) by its id: its
// address from the Graph API, then the file from Meta's media host with the same token. At
// most maxBytes; the errors are texts for the person.
func (s *Service) download(ctx context.Context, mediaID string, maxBytes int) ([]byte, error) {
	info, err := s.graph(ctx, "GET", mediaID, "", nil, timeout())
	if err != nil {
		return nil, err
	}
	if size, _ := info["file_size"].(float64); int(size) > maxBytes {
		return nil, errors.New("file too large")
	}
	address, _ := info["url"].(string)
	if !strings.HasPrefix(address, "https://") {
		return nil, errors.New("no download address from WhatsApp")
	}
	u, err := url.Parse(address)
	if err != nil || !metaHost(u.Hostname()) {
		host := address[8:]
		if i := strings.IndexByte(host, '/'); i >= 0 {
			host = host[:i]
		}
		return nil, errors.New("unexpected download host " + host) // the token goes only to Meta
	}
	status, data, err := s.do(ctx, "GET", address, s.accessToken, "", nil, 2*timeout(), "WhatsApp")
	if err != nil {
		return nil, fmt.Errorf("Download fehlgeschlagen (%s)", err)
	}
	if status != 200 {
		return nil, fmt.Errorf("Download fehlgeschlagen (HTTP %d)", status)
	}
	if len(data) > maxBytes {
		return nil, errors.New("file too large")
	}
	return data, nil
}
