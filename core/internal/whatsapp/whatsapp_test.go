package whatsapp

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"echo-assistant/internal/netx"
)

func TestPlainLinks(t *testing.T) {
	in := "Zeit ([timeanddate.de](https://x.example/a)) und [Karte hier](https://m.example/b) [kein](link)"
	want := "Zeit (https://x.example/a) und Karte hier (https://m.example/b) [kein](link)"
	if got := plainLinks(in); got != want {
		t.Errorf("plainLinks:\n got %q\nwant %q", got, want)
	}
}

func TestSpoken(t *testing.T) {
	got := spoken("*Fett* und _schief_ ([a.de](https://a.de)) mehr https://b.de/x \n• Punkt", 1500)
	if strings.ContainsAny(got, "*_•") || strings.Contains(got, "http") {
		t.Errorf("spoken: %q", got)
	}
}

func TestWaID(t *testing.T) {
	for _, c := range []struct {
		phones []string
		want   string
	}{
		{[]string{"**610", "030123456", "01711234567"}, "491711234567"},
		{[]string{"**610", "030123456"}, "4930123456"},
		{[]string{"0049301234"}, "49301234"},
		{[]string{"**610"}, ""},
		{nil, ""},
	} {
		if got := waID(c.phones); got != c.want {
			t.Errorf("waID(%v) = %q, want %q", c.phones, got, c.want)
		}
	}
}

func TestRelayHost(t *testing.T) {
	if got := relayHostOf("https://name.account.workers.dev/poll"); got != "name.account.workers.dev" {
		t.Errorf("relayHostOf: %q", got)
	}
}

func TestParseEvents(t *testing.T) {
	data := `{"events": [
	 {"id": 5, "at": 1, "value": {"metadata": {"phone_number_id": "N1"},
	   "contacts": [{"wa_id": "491", "profile": {"name": "Name"}}],
	   "messages": [{"from": "491", "id": "wamid.1", "timestamp": "1700000000", "type": "text", "text": {"body": "Hallo"}},
	                {"from": "491", "id": "wamid.2", "timestamp": "1700000001", "type": "document",
	                 "document": {"id": "m1", "filename": "a.pdf", "mime_type": "application/pdf", "caption": "schau"}}]}},
	 {"id": 6, "at": 2, "value": {"announcement": {"source": "Agent", "text": "fertig", "deliver": "call"}}},
	 {"id": 7, "at": 3, "value": {"google_login": {"code": "c", "state": "s"}}}]}`
	events, err := parseEvents([]byte(data))
	if err != nil || len(events) != 3 {
		t.Fatalf("parseEvents: %v %v", events, err)
	}
	v := events[0].Value
	if events[0].ID != 5 || v.Metadata.PhoneNumberID != "N1" || v.Contacts[0].Profile.Name != "Name" || len(v.Messages) != 2 {
		t.Errorf("event 0: %+v", events[0])
	}
	text := parseMessage(v.Messages[0])
	if text.From != "491" || text.Type != "text" || text.Text != "Hallo" || text.ID != "wamid.1" {
		t.Errorf("text message: %+v", text)
	}
	doc := parseMessage(v.Messages[1])
	if doc.Media.ID != "m1" || doc.Media.Filename != "a.pdf" || doc.Text != "schau" {
		t.Errorf("document message: %+v", doc)
	}
	if a := events[1].Value.Announcement; a == nil || a.Source != "Agent" || a.Deliver != "call" {
		t.Errorf("announcement: %+v", a)
	}
	if g := events[2].Value.GoogleLogin; g == nil || g.Code != "c" || g.State != "s" {
		t.Errorf("google login: %+v", g)
	}
}

func TestGraphError(t *testing.T) {
	reply := map[string]any{"error": map[string]any{"message": "Boese", "code": float64(131047)}}
	if got := graphError(reply, 400); got != "Boese (131047)" {
		t.Errorf("graphError: %q", got)
	}
	if got := graphError(map[string]any{}, 500); got != "HTTP 500" {
		t.Errorf("graphError without message: %q", got)
	}
}

// A local test server stands in for the Graph API.
func TestSendTextAndErrors(t *testing.T) {
	var last map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		data, _ := io.ReadAll(r.Body)
		json.Unmarshal(data, &last)
		if r.Header.Get("Authorization") != "Bearer ACCESS" || !strings.HasSuffix(r.URL.Path, "/PNID/messages") {
			t.Errorf("request %s auth %q", r.URL.Path, r.Header.Get("Authorization"))
		}
		if last["to"] == "fail" {
			w.WriteHeader(400)
			w.Write([]byte(`{"error": {"message": "outside window", "code": 131047}}`))
			return
		}
		w.Write([]byte(`{"messages": [{"id": "x"}]}`))
	}))
	defer srv.Close()
	s := &Service{accessToken: "ACCESS", phoneNumberID: "PNID", graphBase: srv.URL, client: netx.Client(0)}
	if got := s.sendText(context.Background(), "49", "Link: ([a.de](https://a.de))"); got != "sent" {
		t.Fatalf("sendText = %q", got)
	}
	text := last["text"].(map[string]any)
	if text["body"] != "Link: (https://a.de)" || last["messaging_product"] != "whatsapp" || last["type"] != "text" {
		t.Errorf("body: %v", last)
	}
	if got := s.sendText(context.Background(), "fail", "x"); got != "Error: outside window (131047)" {
		t.Errorf("error text: %q", got)
	}
}

func TestSendNotConfigured(t *testing.T) {
	s := &Service{}
	if got := s.Send("x", ""); got != "Error: WhatsApp is not set up" {
		t.Errorf("Send = %q", got)
	}
}
