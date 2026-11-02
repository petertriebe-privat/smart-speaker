package screen

import (
	"encoding/json"
	"strings"
	"sync"
	"testing"
	"time"

	"echo-assistant/internal/tools"
)

// fakeApp plays the Fire TV app: it records what is published and acks every card.
type fakeApp struct {
	mu       sync.Mutex
	s        *Service
	topics   []string
	cards    []card
	silent   bool
	retained map[string][]byte // as the broker keeps them
}

func (f *fakeApp) Publish(topic string, payload []byte, retain bool) error {
	f.mu.Lock()
	f.topics = append(f.topics, topic)
	if retain {
		if f.retained == nil {
			f.retained = map[string][]byte{}
		}
		f.retained[topic] = payload
	}
	f.mu.Unlock()
	if strings.HasSuffix(topic, "/show") {
		var c card
		json.Unmarshal(payload, &c)
		f.mu.Lock()
		f.cards = append(f.cards, c)
		f.mu.Unlock()
		if !f.silent {
			go f.s.receive(strings.TrimSuffix(topic, "/show")+"/ack", []byte(c.ID))
		}
	}
	return nil
}

func setup(t *testing.T) (*Service, *fakeApp) {
	t.Helper()
	ackTimeout, startTimeout, poll = 300*time.Millisecond, 300*time.Millisecond, 10*time.Millisecond
	f := &fakeApp{}
	s := newService(f)
	f.s = s
	s.receive(prefix+"room_a/status", []byte(`{"state":"online","display":"on","room":"Room A"}`))
	return s, f
}

func args(js string) map[string]any {
	var m map[string]any
	json.Unmarshal([]byte(js), &m)
	return m
}

func TestShowOverlay(t *testing.T) {
	s, f := setup(t)
	out := s.run(args(`{"room":"","mode":"overlay","title":"Einkauf","text":"","items":["Milch"," ",""],
		"image_url":"","seconds":0}`), tools.Context{})
	if !strings.HasPrefix(out, "Shown on the TV in Room A") {
		t.Fatalf("run: %q", out)
	}
	c := f.cards[0]
	if f.topics[0] != prefix+"room_a/show" || c.Mode != "overlay" || len(c.Items) != 1 || c.ID == "" {
		t.Fatalf("published %v %+v", f.topics, c)
	}
}

func TestRoomChoice(t *testing.T) {
	s, _ := setup(t)
	s.receive(prefix+"room_b/status", []byte(`{"state":"online","display":"on","room":"Room B"}`))
	show := `{"room":"%s","mode":"overlay","title":"x","text":"","items":[],"image_url":"","seconds":0}`
	if out := s.run(args(strings.Replace(show, "%s", "", 1)), tools.Context{}); !strings.Contains(out, "which room") {
		t.Fatalf("no room with two screens: %q", out)
	}
	if out := s.run(args(strings.Replace(show, "%s", "", 1)), tools.Context{Room: "Room B"}); !strings.Contains(out, "Room B") {
		t.Fatalf("the conversation's room: %q", out)
	}
	if out := s.run(args(strings.Replace(show, "%s", "Küche", 1)), tools.Context{}); !strings.Contains(out, "no TV screen in Küche") {
		t.Fatalf("unknown room: %q", out)
	}
	// A retained status removed: the screen is gone.
	s.receive(prefix+"room_b/status", nil)
	if got := s.rooms(); len(got) != 1 {
		t.Fatalf("rooms after removal: %v", got)
	}
}

func TestWakeDarkScreen(t *testing.T) {
	s, f := setup(t)
	s.receive(prefix+"room_a/status", []byte(`{"state":"offline","room":"Room A"}`))
	show := args(`{"room":"Room A","mode":"fullscreen","title":"x","text":"","items":[],"image_url":"","seconds":0}`)
	if out := s.run(show, tools.Context{}); !strings.Contains(out, "cannot be woken") || len(f.cards) != 0 {
		t.Fatalf("without Start: %q", out)
	}
	var started string
	s.Start = func(room, command string) string {
		started = room + "|" + command
		go s.receive(prefix+"room_a/status", []byte(`{"state":"online","display":"on","room":"Room A"}`))
		return ""
	}
	if out := s.run(show, tools.Context{}); !strings.HasPrefix(out, "Shown") || started != "Room A|"+StartCommand {
		t.Fatalf("woken: %q, started %q", out, started)
	}
}

func TestNoAck(t *testing.T) {
	s, f := setup(t)
	f.silent = true
	out := s.run(args(`{"room":"","mode":"overlay","title":"x","text":"","items":[],"image_url":"","seconds":0}`), tools.Context{})
	if !strings.Contains(out, "did not confirm") {
		t.Fatalf("run: %q", out)
	}
	if len(s.acks) != 0 {
		t.Fatalf("ack waiters left: %d", len(s.acks))
	}
}

func TestCardOf(t *testing.T) {
	if _, errText := cardOf(args(`{"title":"","text":"","items":[],"image_url":""}`), "overlay"); errText == "" {
		t.Fatal("empty card accepted")
	}
	if _, errText := cardOf(args(`{"title":"x","image_url":"file:///etc/passwd"}`), "overlay"); errText == "" {
		t.Fatal("non-http image accepted")
	}
	c, _ := cardOf(args(`{"title":"x","seconds":99999}`), "fullscreen")
	if c.Seconds != maxSeconds {
		t.Fatalf("seconds %d", c.Seconds)
	}
	a, _ := cardOf(args(`{"title":"x"}`), "overlay")
	b, _ := cardOf(args(`{"title":"x"}`), "overlay")
	if a.ID == b.ID {
		t.Fatal("card ids repeat")
	}
}

func TestClear(t *testing.T) {
	s, f := setup(t)
	out := s.run(args(`{"room":"Room A","mode":"clear","title":"","text":"","items":[],"image_url":"","seconds":0}`), tools.Context{})
	if out != "Screen in Room A cleared" || f.topics[0] != prefix+"room_a/clear" {
		t.Fatalf("clear: %q %v", out, f.topics)
	}
}

// Kinds and blocks pass through; a url is a web page (mode web), with kind video a video block.
func TestRichCards(t *testing.T) {
	c, errText := cardOf(args(`{"kind":"energy","title":"Strom","blocks":[{"type":"stats","items":[{"label":"Heute","value":"8,4"}]},
		{"no":"type"},"x"]}`), "auto")
	if errText != "" || c.Kind != "energy" || len(c.Blocks) != 1 || c.Blocks[0]["type"] != "stats" {
		t.Fatalf("blocks: %q %+v", errText, c)
	}
	if c, _ := cardOf(args(`{"kind":"nonsense","title":"x"}`), "auto"); c.Kind != "" {
		t.Fatalf("unknown kind kept: %q", c.Kind)
	}
	web, _ := cardOf(args(`{"url":"https://de.wikipedia.org/wiki/Mond"}`), "auto")
	if web.Mode != "web" || web.Kind != "web" || web.URL == "" {
		t.Fatalf("web: %+v", web)
	}
	video, _ := cardOf(args(`{"kind":"video","title":"Clip","url":"https://example.org/a.mp4"}`), "auto")
	if video.Mode != "auto" || video.URL != "" || len(video.Blocks) != 1 || video.Blocks[0]["type"] != "video" {
		t.Fatalf("video: %+v", video)
	}
	if _, errText := cardOf(args(`{"url":"file:///x"}`), "auto"); errText == "" {
		t.Fatal("a file url accepted")
	}
}

// Room "all": every TV that is on, none woken, no wait.
func TestAllRooms(t *testing.T) {
	s, f := setup(t)
	s.receive(prefix+"room_b/status", []byte(`{"state":"online","display":"off","room":"Room B"}`))
	out := s.run(args(`{"room":"all","mode":"auto","title":"Hallo"}`), tools.Context{})
	if out != "Shown on the TVs that are on: Room A" || len(f.topics) != 2 || f.topics[1] != prefix+"room_a/shown" {
		t.Fatalf("all: %q %v", out, f.topics)
	}
}

// What a room's TV showed and its last conversation reach the next conversation there.
func TestRoomContext(t *testing.T) {
	s, f := setup(t)
	if s.RoomContext("Room A") != "" {
		t.Fatal("context without anything")
	}
	if out := s.run(args(`{"room":"Room A","mode":"auto","kind":"calendar","title":"Nächste Woche","items":["Do: 1. Elternbeiratssitzung"]}`), tools.Context{}); !strings.HasPrefix(out, "Shown") {
		t.Fatal(out)
	}
	s.Talked("Room A", "Du: Zeig mir den Kalender\nJarvis: Steht auf dem Fernseher.", "")
	got := s.RoomContext("Room A")
	if !strings.Contains(got, "Elternbeiratssitzung") || !strings.Contains(got, "Zeig mir den Kalender") {
		t.Fatalf("context %q", got)
	}
	// another device in the house gets it from the broker's retained messages
	other := newService(f)
	for _, kind := range []string{"shown", "talk"} {
		other.receive(prefix+"room_a/"+kind, f.retained[prefix+"room_a/"+kind])
	}
	if other.RoomContext("Room A") != got {
		t.Fatalf("other device: %q", other.RoomContext("Room A"))
	}
	s.run(args(`{"room":"Room A","mode":"clear"}`), tools.Context{})
	if strings.Contains(s.RoomContext("Room A"), "On the TV") {
		t.Fatal("cleared card still in the context")
	}
}
