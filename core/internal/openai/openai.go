// Package openai: HTTPS requests to api.openai.com (Responses, speech, transcription), as
// devices/atoms3r/src/services/openai.cpp. The voice conversation has its own WebSocket (live).
package openai

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"strings"
	"time"

	"echo-assistant/internal/netx"
	"echo-assistant/internal/prompts"
)

var base = "https://api.openai.com" // a test server in tests (SetBase)

// SetBase: the API's address (tests).
func SetBase(url string) { base = url }

// ErrNothing: the transcription found no words in the audio (silence, noise, music).
var ErrNothing = errors.New("nothing understandable in the voice note")

var (
	APIKey string
	// One kept-alive client: the handshake costs a second or two, later requests are fast.
	client = netx.Client(time.Duration(prompts.Int("OPENAI_TIMEOUT_MS", 180000)) * time.Millisecond)
)

// Post sends a JSON body and decodes the JSON reply into out (if not nil).
func Post(ctx context.Context, path string, body any, out any) error {
	data, err := json.Marshal(body)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, "POST", base+path, bytes.NewReader(data))
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+APIKey)
	req.Header.Set("Content-Type", "application/json")
	return do(req, out)
}

func do(req *http.Request, out any) error {
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return err
	}
	if resp.StatusCode != 200 {
		return fmt.Errorf("HTTP %d: %s", resp.StatusCode, errorMessage(data))
	}
	if out == nil {
		return nil
	}
	if raw, ok := out.(*[]byte); ok {
		*raw = data
		return nil
	}
	return json.Unmarshal(data, out)
}

func errorMessage(data []byte) string {
	var e struct {
		Error struct {
			Message string `json:"message"`
		} `json:"error"`
	}
	if json.Unmarshal(data, &e) == nil && e.Error.Message != "" {
		return e.Error.Message
	}
	return strings.TrimSpace(string(data[:min(len(data), 300)]))
}

// Response of /v1/responses (the parts we read).
type Response struct {
	Output []struct {
		Type      string `json:"type"`
		Name      string `json:"name"`
		Arguments string `json:"arguments"`
		CallID    string `json:"call_id"`
		Content   []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"content"`
	} `json:"output"`
	Usage Usage `json:"usage"`
}

type Usage struct {
	InputTokens        int `json:"input_tokens"`
	OutputTokens       int `json:"output_tokens"`
	InputTokensDetails struct {
		CachedTokens int `json:"cached_tokens"`
	} `json:"input_tokens_details"`
}

// Text: the reply's message text.
func (r *Response) Text() string {
	var b strings.Builder
	for _, item := range r.Output {
		if item.Type != "message" {
			continue
		}
		for _, part := range item.Content {
			b.WriteString(part.Text)
		}
	}
	return b.String()
}

// Responses runs one request of the Responses API.
func Responses(ctx context.Context, body map[string]any) (*Response, error) {
	var r Response
	if err := Post(ctx, "/v1/responses", body, &r); err != nil {
		return nil, err
	}
	return &r, nil
}

// Speech: text to speech in the assistant's voice. format "pcm" (24 kHz PCM16) or "opus".
func Speech(ctx context.Context, text, format string) ([]byte, error) {
	var audio []byte
	err := Post(ctx, "/v1/audio/speech", map[string]any{
		"model": prompts.Text("TTS_MODEL"), "voice": prompts.Text("LIVE_VOICE"), "input": text,
		"instructions": prompts.Text("TTS_INSTRUCTIONS"), "response_format": format,
	}, &audio)
	return audio, err
}

// Transcribe: speech to text (German). filename tells the format ("voice.ogg").
func Transcribe(ctx context.Context, audio []byte, filename string) (string, error) {
	return TranscribeHint(ctx, audio, filename, "")
}

// TranscribeHint: Transcribe with words the audio likely contains (the API's prompt), e.g. the
// quick music commands.
func TranscribeHint(ctx context.Context, audio []byte, filename, hint string) (string, error) {
	var body bytes.Buffer
	w := multipart.NewWriter(&body)
	w.WriteField("model", prompts.Text("TRANSCRIBE_MODEL"))
	w.WriteField("language", "de")
	if hint != "" {
		w.WriteField("prompt", hint)
	}
	w.WriteField("response_format", "json")
	part, err := w.CreateFormFile("file", filename)
	if err != nil {
		return "", err
	}
	part.Write(audio)
	w.Close()
	req, err := http.NewRequestWithContext(ctx, "POST", base+"/v1/audio/transcriptions", &body)
	if err != nil {
		return "", err
	}
	req.Header.Set("Authorization", "Bearer "+APIKey)
	req.Header.Set("Content-Type", w.FormDataContentType())
	var out struct {
		Text string `json:"text"`
	}
	if err := do(req, &out); err != nil {
		return "", err
	}
	text := strings.TrimSpace(out.Text)
	if text == "" {
		return "", ErrNothing
	}
	return text, nil
}
