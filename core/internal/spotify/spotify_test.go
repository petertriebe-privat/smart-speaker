package spotify

import (
	"strings"
	"testing"
	"time"

	"echo-assistant/internal/config"
	"echo-assistant/internal/people"
	"echo-assistant/internal/store"
	"echo-assistant/internal/tools"
)

func TestParseAccounts(t *testing.T) {
	want := []account{{"anna", "tok1"}, {"ben", "tok2"}}
	cases := map[string]any{
		"brace list": `{"anna", "tok1"}, {"ben","tok2"}`,
		"json text":  `[["anna","tok1"],["ben","tok2"]]`,
		"pairs":      []any{[]any{"anna", "tok1"}, []any{"ben", "tok2"}},
		"objects":    []any{map[string]any{"person": "anna", "refresh": "tok1"}, map[string]any{"name": "ben", "token": "tok2"}},
		"map":        map[string]any{"ben": "tok2", "anna": "tok1"},
	}
	for name, in := range cases {
		got := parseAccounts(in)
		if len(got) != 2 || got[0] != want[0] || got[1] != want[1] {
			t.Errorf("%s: got %v", name, got)
		}
	}
	if got := parseAccounts(nil); len(got) != 0 {
		t.Errorf("nil: %v", got)
	}
	if got := parseAccounts(`{"", "tok"}`); len(got) != 1 || got[0].person != "" {
		t.Errorf("admin entry: %v", got)
	}
}

func TestRoomSpeakers(t *testing.T) {
	got := parseRoomSpeakers(" Echo Wohnzimmer , ,Küche")
	if len(got) != 2 || got[0] != "echo wohnzimmer" || got[1] != "küche" {
		t.Errorf("got %v", got)
	}
	s := &Service{}
	if !s.inRoom("Anything", "Speaker") || s.inRoom("Phone", "Smartphone") {
		t.Error("default room")
	}
	s.roomSpeakers = got
	if !s.inRoom("KÜCHE", "Speaker") || s.inRoom("Bad", "Speaker") {
		t.Error("listed room")
	}
}

func TestFormatting(t *testing.T) {
	if got := urlEncode("Daft Punk & Co ü"); got != "Daft%20Punk%20%26%20Co%20%C3%BC" {
		t.Error(got)
	}
	if got := clock(125000); got != "2:05" {
		t.Error(got)
	}
	track := map[string]any{"type": "track", "name": "Song", "artists": []any{
		map[string]any{"name": "A"}, map[string]any{"name": "B"}}}
	if got := describe(track); got != "'Song' by A, B" {
		t.Error(got)
	}
	if got := describe(map[string]any{"type": "episode", "name": "E"}); got != "episode 'E' of a podcast" {
		t.Error(got)
	}
	now := time.Date(2026, 1, 2, 12, 0, 0, 0, time.UTC)
	for stamp, want := range map[string]string{
		"2026-01-02T11:30:00.000Z": "30 min ago",
		"2026-01-02T07:00:00Z":     "5 h ago",
		"2025-12-29T12:00:00Z":     "4 days ago",
		"junk":                     "",
		"2026-01-03T00:00:00Z":     "",
	} {
		if got := ago(stamp, now); got != want {
			t.Errorf("ago(%s) = %q, want %q", stamp, got, want)
		}
	}
}

func TestErrorText(t *testing.T) {
	if got := errorText(429, nil, nil); !strings.Contains(got, "rate limit") {
		t.Error(got)
	}
	if got := errorText(403, []byte(`{"error":{"message":"Player command failed: Premium required"}}`), nil); !strings.Contains(got, "needs Spotify Premium") {
		t.Error(got)
	}
	if got := errorText(400, []byte(`{"error":"invalid_grant","error_description":"Refresh token revoked"}`), nil); got != "HTTP 400: Refresh token revoked" {
		t.Error(got)
	}
	if got := errorText(500, []byte(`{"error":"boom"}`), nil); got != "HTTP 500: boom" {
		t.Error(got)
	}
}

func devices() []device {
	return []device{
		{id: "1", name: "Echo Wohnzimmer", typ: "Speaker", volume: -1},
		{id: "2", name: "Echo Küche", typ: "Speaker", volume: -1},
		{id: "3", name: "Annas Handy", typ: "Smartphone", volume: -1},
		{id: "4", name: "Radio Bad", typ: "Speaker", volume: -1},
	}
}

func TestFindDevice(t *testing.T) {
	for wanted, id := range map[string]string{
		"echo wohnzimmer": "1", "Küche": "2", "Handy": "3", "annas handy": "3", "Bad": "4", "wohnzimmers echo": "1",
	} {
		d, errText := findDevice(devices(), wanted)
		if d == nil || d.id != id {
			t.Errorf("%q: %v %s", wanted, d, errText)
		}
	}
	if d, errText := findDevice(devices(), "echo"); d != nil || !strings.Contains(errText, "several speakers") {
		t.Errorf("echo: %v %s", d, errText)
	}
	if d, errText := findDevice(devices(), "garage"); d != nil || !strings.Contains(errText, "does not see") {
		t.Errorf("garage: %v %s", d, errText)
	}
}

// Two Echos "Jarvis" and "Jarvis 2": a number tells them apart, also when one is not listed.
func TestFindDeviceNumbers(t *testing.T) {
	both := []device{{id: "a", name: "Jarvis"}, {id: "b", name: "Jarvis 2"}, {id: "c", name: "Echo Küche"}}
	for wanted, id := range map[string]string{"Jarvis": "a", "jarvis 2": "b", "Jarvis 2": "b"} {
		if d, errText := findDevice(both, wanted); d == nil || d.id != id {
			t.Errorf("%q: %v %s", wanted, d, errText)
		}
	}
	if d, _ := findDevice([]device{{id: "a", name: "Jarvis"}}, "Jarvis 2"); d != nil {
		t.Errorf("Jarvis 2 found as %s", d.name)
	}
	if d, _ := findDevice([]device{{id: "b", name: "Jarvis 2"}}, "Jarvis"); d != nil {
		t.Errorf("Jarvis found as %s", d.name)
	}
	rooms := []device{{id: "e", name: "Echo Wohnzimmer"}, {id: "j", name: "Jarvis Wohnzimmer"}, {id: "k", name: "Echo Küche"}}
	if d, errText := findDevice(rooms, "wohnzimmer"); d == nil || d.id != "e" {
		t.Errorf("a room with Amazon's and Jarvis' speaker: Amazon's (the better one): %v %s", d, errText)
	}
	if d, _ := findDevice(rooms, "Jarvis Wohnzimmer"); d == nil || d.id != "j" {
		t.Error("Jarvis' own by its full name")
	}
	s := &Service{LocalSpeaker: "Jarvis 2"}
	if d := s.ownDevice([]device{{id: "a", name: "Jarvis"}}); d != nil {
		t.Error("own speaker found under another Echo's name")
	}
	if !s.isLocal("") || !s.isLocal("jarvis 2") || s.isLocal("Echo Küche") {
		t.Error("isLocal")
	}
	if s := (&Service{LocalSpeaker: "Jarvis"}); s.isLocal("Jarvis 2") {
		t.Error("the first Echo takes the second one's name for its own")
	}
}

func TestFindPlaylist(t *testing.T) {
	all := []playlist{{name: "Morning Mix", own: true}, {name: "Morning Coffee", own: true}, {name: "Party", owner: "X"}}
	if p, _ := findPlaylist(all, "party", false); p == nil || p.name != "Party" {
		t.Error("exact")
	}
	if p, errText := findPlaylist(all, "morning", true); p != nil || !strings.Contains(errText, "several") {
		t.Error(errText)
	}
	if p, errText := findPlaylist(all, "party", true); p != nil || !strings.Contains(errText, "belongs to X") {
		t.Error(errText)
	}
}

func TestAddTools(t *testing.T) {
	store.Dir = t.TempDir()
	secrets := config.Secrets{"OWNER_NAME": "Admin", "SPOTIFY_CLIENT_ID": "id", "SPOTIFY_CLIENT_SECRET": "secret",
		"SPOTIFY_REFRESH_TOKEN": "refresh"}
	s := New(secrets, people.New(secrets))
	if !s.Configured() {
		t.Fatal("not configured")
	}
	var r tools.Registry
	s.AddTools(&r) // panics on a schema that is not JSON
	if got := len(r.Names()); got != 6 {
		t.Errorf("%d tools", got)
	}
	var none tools.Registry
	New(config.Secrets{}, nil).AddTools(&none)
	if len(none.Names()) != 0 {
		t.Error("tools without configuration")
	}
}
