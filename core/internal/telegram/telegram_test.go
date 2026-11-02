package telegram

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

func TestEscapeAndPlain(t *testing.T) {
	if got := escape("a < b & c > d"); got != "a &lt; b &amp; c &gt; d" {
		t.Errorf("escape: %q", got)
	}
	html := `<b>Fett</b> und <a href="https://example.org/x?a=1&amp;b=2">Link</a> &lt;3`
	if got := plain(html, true); got != "Fett und Link (https://example.org/x?a=1&b=2) <3" {
		t.Errorf("plain with links: %q", got)
	}
	if got := plain(html, false); got != "Fett und Link <3" {
		t.Errorf("plain without links: %q", got)
	}
	if got := plain("broken <b", true); got != "broken <b" {
		t.Errorf("plain open tag: %q", got)
	}
}

func TestLinksToHTML(t *testing.T) {
	in := "Zeit ([timeanddate.de](https://x.example/a?b=1&c=2)) ok [kein link] und [a](nicht)"
	want := `Zeit (<a href="https://x.example/a?b=1&amp;c=2">timeanddate.de</a>) ok [kein link] und [a](nicht)`
	if got := linksToHTML(in); got != want {
		t.Errorf("linksToHTML:\n got %q\nwant %q", got, want)
	}
	if got := linksToHTML("[x](https://a/b\"c)"); got != `<a href="https://a/b%22c">x</a>` {
		t.Errorf("quote in url: %q", got)
	}
}

func TestSpoken(t *testing.T) {
	answer := "Es ist **zehn** Uhr ([timeanddate.de](https://x.example)) , siehe https://foo.example/a\n• Punkt"
	got := spoken(answer, 1500)
	if strings.Contains(got, "http") || strings.Contains(got, "([") || strings.Contains(got, "•") {
		t.Errorf("spoken keeps links or marks: %q", got)
	}
	if got := spoken(strings.Repeat("a", 2000), 100); !strings.HasSuffix(got, " … Den Rest findest du im Chat.") || len(got) > 140 {
		t.Errorf("spoken long: %q", got)
	}
}

func TestCommandName(t *testing.T) {
	for in, want := range map[string]string{"/neu": "neu", "/Neu@AssistantBot": "neu", "/skills extra": "skills", "/a\nb": "a"} {
		if got := commandName(in); got != want {
			t.Errorf("commandName(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestParseUpdates(t *testing.T) {
	data := `[{"update_id": 7, "message": {"from": {"id": 42, "first_name": "A", "username": "a"}, "chat": {"id": 42},
		"date": 1700000000, "text": "hi", "voice": {"file_id": "v1", "duration": 4},
		"photo": [{"file_id": "p1", "file_size": 100}, {"file_id": "p2", "file_size": 5000000}],
		"document": {"file_id": "d1", "file_name": "x.csv", "mime_type": "text/csv", "file_size": 12}}},
		{"update_id": 8}]`
	updates, err := parseUpdates(json.RawMessage(data))
	if err != nil || len(updates) != 2 {
		t.Fatalf("parseUpdates: %v %v", updates, err)
	}
	m := updates[0].Message
	if m == nil || m.From.ID != 42 || m.Chat.ID != 42 || m.Text != "hi" || m.Voice.Duration != 4 || m.Document.FileName != "x.csv" {
		t.Errorf("message: %+v", m)
	}
	if updates[1].UpdateID != 8 || updates[1].Message != nil {
		t.Errorf("second update: %+v", updates[1])
	}
	if id := pickPhoto(m.Photo, 1024*1024); id != "p1" {
		t.Errorf("pickPhoto = %q", id)
	}
	if id := pickPhoto(m.Photo, 10); id != "" {
		t.Errorf("pickPhoto too small = %q", id)
	}
}

// A local test server stands in for Telegram: HTML the API refuses is sent again as text.
func TestSendTextFallsBackToPlain(t *testing.T) {
	var bodies []map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		data, _ := io.ReadAll(r.Body)
		var body map[string]any
		json.Unmarshal(data, &body)
		bodies = append(bodies, body)
		if !strings.HasPrefix(r.URL.Path, "/botTOKEN/sendMessage") {
			t.Errorf("path %s", r.URL.Path)
		}
		if body["parse_mode"] == "HTML" {
			w.Write([]byte(`{"ok": false, "description": "Bad Request: can't parse entities: x"}`))
			return
		}
		w.Write([]byte(`{"ok": true, "result": {}}`))
	}))
	defer srv.Close()
	b := &Bot{token: "TOKEN", base: srv.URL, client: netx.Client(0)}
	if got := b.sendText(context.Background(), 5, `<b>Hi</b> <a href="https://e.example">e</a>`); got != "sent" {
		t.Fatalf("sendText = %q", got)
	}
	if len(bodies) != 2 || bodies[1]["parse_mode"] != nil || bodies[1]["text"] != "Hi e (https://e.example)" {
		t.Errorf("bodies: %v", bodies)
	}
}

func TestSendNotConfigured(t *testing.T) {
	b := &Bot{}
	if got := b.Send("x", ""); got != "Error: Telegram is not set up" {
		t.Errorf("Send = %q", got)
	}
}

func TestParseButton(t *testing.T) {
	for _, c := range []struct {
		in, tool, data string
		ok             bool
	}{
		{"mcp:task_decide:ab12:y", "task_decide", "ab12:y", true},
		{"mcp:task_decide", "", "", false},
		{"mcp::x", "", "", false},
		{"other:x:y", "", "", false},
	} {
		tool, data, ok := parseButton(c.in)
		if ok != c.ok || ok && (tool != c.tool || data != c.data) {
			t.Errorf("parseButton(%q) = %q, %q, %v", c.in, tool, data, ok)
		}
	}
}
