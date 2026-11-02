package telegram

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
	"strconv"
	"strings"
	"time"

	"echo-assistant/internal/chatcore"
	"echo-assistant/internal/prompts"
	"echo-assistant/internal/textutil"
)

func timeout() time.Duration {
	return time.Duration(prompts.Int("TELEGRAM_TIMEOUT_MS", 30000)) * time.Millisecond
}

// netError words a transport failure. The token is in the request path: the *url.Error (which
// carries the URL) is unwrapped and never logged.
func netError(err error) string {
	var ue *url.Error
	if errors.As(err, &ue) {
		err = ue.Err
	}
	var op *net.OpError
	if errors.As(err, &op) && op.Op == "dial" {
		return "no connection to Telegram"
	}
	return "Telegram not reachable (" + err.Error() + ")"
}

// request does one HTTP request with a time limit and returns status and body.
func (b *Bot) request(ctx context.Context, method, path, contentType string, body []byte, limit time.Duration) (int, []byte, error) {
	ctx, cancel := context.WithTimeout(ctx, limit)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, method, b.base+path, bytes.NewReader(body))
	if err != nil {
		return 0, nil, errors.New("Telegram not reachable (bad request)")
	}
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	resp, err := b.client.Do(req)
	if err != nil {
		return 0, nil, errors.New(netError(err))
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, maxReply+1))
	if err != nil {
		return resp.StatusCode, nil, errors.New("Telegram not reachable (" + err.Error() + ")")
	}
	return resp.StatusCode, data, nil
}

const maxReply = 8 << 20

// call is one Bot API request; the result is Telegram's "result" member. The error is Telegram's
// description or a text about the transport.
func (b *Bot) call(ctx context.Context, method, contentType string, body []byte, limit time.Duration) (json.RawMessage, error) {
	status, data, err := b.request(ctx, "POST", "/bot"+b.token+"/"+method, contentType, body, limit)
	if err != nil {
		return nil, err
	}
	var reply struct {
		OK          bool            `json:"ok"`
		Result      json.RawMessage `json:"result"`
		Description string          `json:"description"`
	}
	if json.Unmarshal(data, &reply) != nil {
		return nil, fmt.Errorf("answer from Telegram not readable (HTTP %d)", status)
	}
	if reply.OK {
		return reply.Result, nil
	}
	if reply.Description != "" {
		return nil, errors.New(reply.Description)
	}
	return nil, fmt.Errorf("HTTP %d", status)
}

func (b *Bot) callJSON(ctx context.Context, method string, body map[string]any, limit time.Duration) (json.RawMessage, error) {
	payload, err := json.Marshal(body)
	if err != nil {
		return nil, err
	}
	return b.call(ctx, method, "application/json", payload, limit)
}

// sendText sends HTML to a chat as one or more messages; falls back to plain text if Telegram
// rejects the markup. Returns "sent" or "Error: ...".
func (b *Bot) sendText(ctx context.Context, chat int64, html string) string {
	for _, message := range chatcore.Split(linksToHTML(html), prompts.Int("TELEGRAM_MESSAGE_CHARS", 4000)) {
		body := map[string]any{"chat_id": chat, "text": textutil.Repair(message), "parse_mode": "HTML"}
		_, err := b.callJSON(ctx, "sendMessage", body, timeout())
		if err != nil && strings.Contains(err.Error(), "can't parse entities") {
			logf("HTML refused (%v), sending as text", err)
			delete(body, "parse_mode")
			body["text"] = plain(message, true)
			_, err = b.callJSON(ctx, "sendMessage", body, timeout())
		}
		if err != nil {
			return "Error: " + err.Error()
		}
	}
	return "sent"
}

// sendFile sends a file to a chat as multipart/form-data: sendPhoto (field "photo"), sendVoice
// ("voice", Ogg Opus) or sendDocument ("document"). Returns "sent" or "Error: ...".
func (b *Bot) sendFile(ctx context.Context, chat int64, method, field, filename, contentType string, data []byte, caption string) string {
	var body bytes.Buffer
	w := multipart.NewWriter(&body)
	w.WriteField("chat_id", strconv.FormatInt(chat, 10))
	if caption != "" {
		w.WriteField("caption", textutil.Prefix(caption, 1000))
	}
	header := textproto.MIMEHeader{}
	header.Set("Content-Disposition", fmt.Sprintf(`form-data; name=%q; filename=%q`, field, filename))
	header.Set("Content-Type", contentType)
	part, err := w.CreatePart(header)
	if err != nil {
		return "Error: " + err.Error()
	}
	part.Write(data)
	w.Close()
	if _, err := b.call(ctx, method, w.FormDataContentType(), body.Bytes(), 2*timeout()); err != nil {
		return "Error: " + err.Error()
	}
	return "sent"
}

// download fetches a file a person sent (voice note, photo, document) by its file_id: at most
// maxBytes. The errors are texts for the person.
func (b *Bot) download(ctx context.Context, fileID string, maxBytes int) ([]byte, error) {
	result, err := b.callJSON(ctx, "getFile", map[string]any{"file_id": fileID}, timeout())
	if err != nil {
		return nil, err
	}
	var file struct {
		Path string `json:"file_path"`
		Size int    `json:"file_size"`
	}
	json.Unmarshal(result, &file)
	if file.Path == "" {
		return nil, errors.New("file not available at Telegram")
	}
	if file.Size > maxBytes {
		return nil, errors.New("file too large")
	}
	status, data, err := b.request(ctx, "GET", "/file/bot"+b.token+"/"+file.Path, "", nil, 2*timeout())
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
