package images

import (
	"errors"
	"strings"
	"sync/atomic"
	"testing"

	"echo-assistant/internal/config"
	"echo-assistant/internal/mail"
	"echo-assistant/internal/tools"
)

func TestFilenameFor(t *testing.T) {
	for title, want := range map[string]string{
		"Katze im Weltall": "katze-im-weltall.jpg",
		"Schöne Grüße!":    "schoene-gruesse.jpg",
		"???":              "assistant-bild.jpg",
	} {
		if got := filenameFor(title); got != want {
			t.Errorf("filenameFor(%q) = %q, want %q", title, got, want)
		}
	}
}

func TestClassifyAndRetry(t *testing.T) {
	status, text := classify(errors.New("HTTP 429: slow down"))
	if status != 429 || text != "slow down" || !retryable(status, text) {
		t.Errorf("429: %d %q", status, text)
	}
	status, text = classify(errors.New("HTTP 400: Your request was rejected by the safety system."))
	if status != 400 || text != "rejected by the content filter" || retryable(status, text) {
		t.Errorf("400: %d %q", status, text)
	}
	status, text = classify(errors.New("dial tcp: connection refused"))
	if status != 0 || !retryable(status, text) {
		t.Errorf("refused: %d %q", status, text)
	}
	if retryable(0, "Zeitueberschreitung beim Generieren") {
		t.Error("a timeout must not be retried")
	}
}

type fakeMail struct{ sent []string }

func (f *fakeMail) Configured() bool { return true }
func (f *fakeMail) Send(to, subject, html string, a *mail.Attachment) string {
	f.sent = append(f.sent, to+"|"+subject)
	return "sent"
}

func TestGenerateRetries(t *testing.T) {
	s := New(config.Secrets{}, nil)
	var calls atomic.Int32
	s.retryPause = func(int) {}
	s.generateJPG = func(string, string) ([]byte, int, error) {
		if calls.Add(1) < 3 {
			return nil, 503, errors.New("busy")
		}
		return []byte("jpeg"), 200, nil
	}
	if jpeg, err := s.generate("x", ""); err != nil || string(jpeg) != "jpeg" || calls.Load() != 3 {
		t.Errorf("generate: %q %v after %d calls", jpeg, err, calls.Load())
	}
	calls.Store(0)
	s.generateJPG = func(string, string) ([]byte, int, error) { calls.Add(1); return nil, 400, errors.New("bad") }
	if _, err := s.generate("x", ""); err == nil || calls.Load() != 1 {
		t.Errorf("a 400 must not be retried (%d calls)", calls.Load())
	}
}

func TestCallValidation(t *testing.T) {
	s := New(config.Secrets{"OWNER_EMAIL": "owner@example.org"}, nil)
	s.Mail = &fakeMail{}
	if got := s.call("create_image", map[string]any{"prompt": "a cat"}, false, ""); got != "Error: give print, email_to or telegram" {
		t.Error(got)
	}
	if got := s.call("create_image", map[string]any{"print": true, "prompt": "a cat"}, false, ""); got != "Error: no printer configured" {
		t.Error(got)
	}
	if got := s.call("create_image", map[string]any{"telegram": true, "prompt": "a cat"}, false, ""); got != "Error: Telegram is not set up" {
		t.Error(got)
	}
	if got := s.call("create_image", map[string]any{"email_to": "nobody", "prompt": "a cat"}, false, ""); !strings.HasPrefix(got, "Error: e-mail not possible") {
		t.Error(got)
	}
	if got := s.call("create_image", map[string]any{"email_to": "a@b.c", "prompt": ""}, false, ""); got != "Error: no image description" {
		t.Error(got)
	}
}

func TestRunMailsImage(t *testing.T) {
	s := New(config.Secrets{"OWNER_EMAIL": "owner@example.org"}, nil)
	m := &fakeMail{}
	s.Mail = m
	s.generateJPG = func(string, string) ([]byte, int, error) { return []byte("jpeg"), 200, nil }
	s.run(&job{prompt: "a cat", title: "Cat", emailTo: "a@b.c"})
	r, ok := s.TakeResult()
	if !ok || !r.OK || !strings.Contains(r.Message, `image "Cat" mailed to a@b.c`) {
		t.Errorf("result %+v", r)
	}
	if len(m.sent) != 1 || m.sent[0] != "a@b.c|Cat" {
		t.Errorf("sent %v", m.sent)
	}
	// send_last_image: the same image again, nothing generated.
	s.generateJPG = func(string, string) ([]byte, int, error) {
		t.Error("must not generate")
		return nil, 0, errors.New("x")
	}
	s.run(&job{reuseLast: true, emailTo: "c@d.e"})
	if r, _ := s.TakeResult(); !r.OK || len(m.sent) != 2 {
		t.Errorf("reuse %+v %v", r, m.sent)
	}
}

func TestToolsRegister(t *testing.T) {
	s := New(config.Secrets{}, nil)
	s.Mail = &fakeMail{}
	var r tools.Registry
	s.AddTools(&r) // schemas must parse
	if names := r.Names(); len(names) != 2 {
		t.Errorf("tools %v", names)
	}
}
