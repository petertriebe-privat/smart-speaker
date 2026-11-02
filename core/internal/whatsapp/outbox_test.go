package whatsapp

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"echo-assistant/internal/config"
	"echo-assistant/internal/netx"
	"echo-assistant/internal/people"
	"echo-assistant/internal/store"
)

func TestTemplateParam(t *testing.T) {
	got, cut := templateParam("Termin\n\n  morgen\t um   9\r\nim Büro")
	if got != "Termin · morgen um 9 · im Büro" || cut {
		t.Errorf("templateParam = %q, %v", got, cut)
	}
	long, cut := templateParam(strings.Repeat("a", 2000))
	if !cut || len(long) > 900 || !strings.HasSuffix(long, " …") {
		t.Errorf("long: %d bytes, cut %v", len(long), cut)
	}
}

// The Graph API as a local server: every message request is recorded by its type.
func outboxServer(t *testing.T, sent *[]map[string]any) *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/PNID/media") {
			w.Write([]byte(`{"id": "MEDIA1"}`))
			return
		}
		if strings.HasSuffix(r.URL.Path, "/PNID") {
			w.Write([]byte(`{"display_phone_number": "+49 30 1234567"}`))
			return
		}
		var body map[string]any
		data, _ := io.ReadAll(r.Body)
		json.Unmarshal(data, &body)
		*sent = append(*sent, body)
		w.Write([]byte(`{"messages": [{"id": "x"}]}`))
	}))
}

func TestOutsideWindow(t *testing.T) {
	store.Dir = t.TempDir()
	var sent []map[string]any
	srv := outboxServer(t, &sent)
	defer srv.Close()
	reg := people.New(config.Secrets{"OWNER_NAME": "Admin", "PHONE_OWNER": "01711234567"})
	s := &Service{accessToken: "A", phoneNumberID: "PNID", graphBase: srv.URL, client: netx.Client(0), reg: reg,
		relayHost: "r", relayToken: "t"}

	// Never wrote: a short text goes whole in the template, nothing waits.
	if got := s.Send("Paket ist da", ""); got != "sent" || len(sent) != 1 || sent[0]["type"] != "template" {
		t.Fatalf("short: %q %v", got, sent)
	}
	if len(s.loadOutbox()) != 0 {
		t.Errorf("short text queued")
	}
	// A photo: uploaded, the template now, the photo waits for the answer.
	if got := s.SendPhoto([]byte{1}, "Katze", ""); got != "sent" || len(sent) != 2 || sent[1]["type"] != "template" {
		t.Fatalf("photo: %q %v", got, sent)
	}
	if q := s.loadOutbox(); len(q) != 1 || q[0].Kind != "image" || q[0].MediaID != "MEDIA1" {
		t.Fatalf("outbox: %+v", q)
	}
	// The admin writes: the window opens and the photo goes out first.
	s.seen("491711234567", time.Now())
	s.flush(context.Background(), "491711234567")
	if len(sent) != 3 || sent[2]["type"] != "image" || len(s.loadOutbox()) != 0 {
		t.Fatalf("flush: %v, outbox %v", sent, s.loadOutbox())
	}
	// Within the window: free text.
	if got := s.Send("Noch was", ""); got != "sent" || sent[3]["type"] != "text" {
		t.Errorf("in window: %q %v", got, sent[3])
	}
	if number, link := s.Number(); number != "+49 30 1234567" || link != "https://wa.me/49301234567" {
		t.Errorf("Number = %q %q", number, link)
	}
}
