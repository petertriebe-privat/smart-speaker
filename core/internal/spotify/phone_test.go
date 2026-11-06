package spotify

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// fakePlayer: Spotify's token endpoint and /me/player, which reports playing as given.
func fakePlayer(t *testing.T, playing bool, deviceID string) *Service {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/token":
			w.Write([]byte(`{"access_token":"x","expires_in":3600}`))
		case "/v1/me/player":
			state := "false"
			if playing {
				state = "true"
			}
			w.Write([]byte(`{"is_playing":` + state + `,"device":{"id":"` + deviceID + `"}}`))
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	s := &Service{api: srv.URL, token: srv.URL + "/token", tokens: map[string]*accessToken{}, loaded: true,
		http: srv.Client()}
	return s
}

// A phone whose Spotify app takes the command but stays silent (no network under the battery
// saver, 06.11.2026) is reported with the phone's hint, not as playing.
func TestPhonePlays(t *testing.T) {
	phone := &device{id: "p1", name: "Phone", typ: "Smartphone"}
	if got := fakePlayer(t, true, "p1").phonePlays(session{refresh: "r"}, phone); got != "" {
		t.Fatalf("playing phone: %q", got)
	}
	s := fakePlayer(t, false, "p1")
	s.PhoneHint = func() string { return "The phone's battery saver is on." }
	got := s.phonePlays(session{refresh: "r"}, phone)
	if !strings.HasPrefix(got, "Error") || !strings.Contains(got, "battery saver") {
		t.Fatalf("silent phone: %q", got)
	}
	speaker := &device{id: "e1", name: "Echo", typ: "Speaker"}
	if got := s.phonePlays(session{refresh: "r"}, speaker); got != "" {
		t.Fatalf("speaker is not checked: %q", got)
	}
}
