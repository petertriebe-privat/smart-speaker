// Package spotify: Spotify over its Web API (issue #39), as devices/atoms3r/src/services/spotify.cpp
// on the M5: play anything on any Spotify Connect device (Echos, phones, ...), control playback,
// manage playlists and the library, read top tracks and history. Needs Premium. One account per
// person: the admin's (SPOTIFY_REFRESH_TOKEN) and others (SPOTIFY_ACCOUNTS), refresh tokens from
// tools/spotify_auth.py; whoever talks uses their own, else the admin's. Development Mode
// (since 2026-03): no recommendations, related artists, artist top tracks, audio features or
// Spotify's own playlists.
//
// Tools (all slow, TLS to Spotify): play_music, music_control, music_speakers, music_playlist,
// music_library, music_insights. The caller also tells the service when a voice conversation is
// open (Duck) so the music in the assistant's room is turned down while it talks.
//
// Secrets: SPOTIFY_CLIENT_ID, SPOTIFY_CLIENT_SECRET, SPOTIFY_REFRESH_TOKEN (strings),
// SPOTIFY_ROOM_SPEAKERS (comma separated string, or a JSON array of strings) and SPOTIFY_ACCOUNTS
// (see parseAccounts: the M5's macro is a C++ brace list that tools/make_secrets.py cannot turn
// into JSON, so the Go side accepts the brace list as text as well as real JSON).
package spotify

import (
	"log"
	"net/http"
	"sync"
	"time"

	"echo-assistant/internal/config"
	"echo-assistant/internal/netx"
	"echo-assistant/internal/people"
	"echo-assistant/internal/prompts"
	"echo-assistant/internal/store"
)

const (
	apiBase  = "https://api.spotify.com"
	tokenURL = "https://accounts.spotify.com/api/token"

	// Access tokens (not refresh tokens) survive a restart here; keyed by a hash of the refresh
	// token. The M5 keeps them in RAM only; on the Echo a restart would cost a refresh per account.
	tokenFile = "spotify_tokens.json"

	// notVisible is appended to errors and lists that name the speakers Spotify can see.
	notVisible = " Phones and computers appear only while the Spotify app is open on them; an Echo of the household " +
		"that is missing can start Spotify itself with alexa (command, e.g. 'spiele ... von Spotify'). A speaker " +
		"the Spotify app finds only in the local WiFi (e.g. a friend's while visiting) is not known to the account " +
		"until the user picks it once in the app; after that it can be controlled here."
)

// Service is the Spotify module. Create it with New; all methods are safe for concurrent use
// (tools run in several goroutines).
type Service struct {
	// LocalSpeaker: The assistant's own Spotify Connect name (the Echo with librespot): where music
	// goes when nothing plays and no speaker is named, and not ducked over the Web API (the
	// device turns it down itself, at once).
	LocalSpeaker string
	// LocalVolume: the device's real volume 0..100 (Spotify only knows what librespot last
	// reported); the base for "lauter"/"leiser" on it.
	LocalVolume func() int
	// LocalWake starts the assistant's own player anew: Spotify stops listing it once playback moved
	// away; nil = none.
	LocalWake func()
	// LocalPlaying: The assistant's own player delivers audio (Spotify's word for it is not enough).
	LocalPlaying func() bool
	// PhoneHint: on the phone, why its Spotify app may not play (e.g. the battery saver cuts it
	// off from the network in the background); "" when nothing is known.
	PhoneHint func() string

	clientID, clientSecret string
	admin                  string    // the admin's refresh token
	others                 []account // other people's accounts
	roomSpeakers           []string  // lower case names, SPOTIFY_ROOM_SPEAKERS
	people                 *people.Register

	timeout       time.Duration
	settle        time.Duration // after next/previous/play before the state shows the new song
	duckVolume    int           // music in the assistant's room lower than this while it talks; 0 = off
	listTracks    int           // songs of a list the model gives
	playlistPages int           // of 50 playlists each, searched by name

	http  *http.Client
	api   string // base URLs, fields so tests can point them elsewhere
	token string

	// tokenMu guards tokens (and is held while one is fetched, as on the M5).
	tokenMu sync.Mutex
	tokens  map[string]*accessToken
	loaded  bool

	// Ducking (duck.go).
	duckMu    sync.Mutex
	ducked    []duckedDevice
	open      bool
	duckOnce  sync.Once
	duckQueue chan bool
}

// New reads the Spotify secrets. people resolves names to accounts (the register of the
// household). The service is inert until Configured.
func New(secrets config.Secrets, register *people.Register) *Service {
	s := &Service{
		clientID:      secrets.String("SPOTIFY_CLIENT_ID"),
		clientSecret:  secrets.String("SPOTIFY_CLIENT_SECRET"),
		admin:         secrets.String("SPOTIFY_REFRESH_TOKEN"),
		others:        parseAccounts(secrets["SPOTIFY_ACCOUNTS"]),
		roomSpeakers:  parseRoomSpeakers(secrets["SPOTIFY_ROOM_SPEAKERS"]),
		people:        register,
		timeout:       time.Duration(prompts.Int("SPOTIFY_TIMEOUT_MS", 15000)) * time.Millisecond,
		settle:        time.Duration(prompts.Int("SPOTIFY_SETTLE_MS", 700)) * time.Millisecond,
		duckVolume:    prompts.Int("SPOTIFY_DUCK_VOLUME", 20),
		listTracks:    prompts.Int("SPOTIFY_LIST_TRACKS", 15),
		playlistPages: prompts.Int("SPOTIFY_PLAYLIST_PAGES", 4),
		api:           apiBase,
		token:         tokenURL,
		tokens:        map[string]*accessToken{},
	}
	s.http = netx.Client(s.timeout)
	if s.Configured() {
		log.Printf("[Spotify] configured, %d more account(s)", len(s.others))
	}
	return s
}

// Configured: client id, client secret and the admin's refresh token are set.
func (s *Service) Configured() bool {
	return s != nil && s.clientID != "" && s.clientSecret != "" && s.admin != ""
}

// --- Accounts ---------------------------------------------------------------------------

// Session: whose account a call uses: the refresh token, the person (people id) and how results
// name it.
type session struct {
	refresh, person, label string // label "" for the admin's own account
}

// accountOf gives the refresh token of a people id, "" if none.
func (s *Service) accountOf(person string) string {
	if person == s.people.Admin() {
		return s.admin
	}
	for _, a := range s.others {
		id := s.people.Admin()
		if a.person != "" {
			id = s.people.ByName(a.person)
		}
		if id == person && a.refresh != "" {
			return a.refresh
		}
	}
	return ""
}

// session picks the account for a call: `wanted` (a person's name) if given, else the caller's
// own, else the admin's. Another person's account only for them and the admin (at home,
// unknown = trusted).
func (s *Service) session(caller, wanted string) (session, string) {
	admin := s.people.Admin()
	person := admin
	if caller != "" && s.accountOf(caller) != "" {
		person = caller
	}
	if wanted != "" {
		person = s.people.ByName(wanted)
		if person == "" {
			return session{}, "Error: nobody called \"" + wanted + "\" in the household."
		}
		if caller != "" && caller != person && caller != admin {
			return session{}, "Error: only " + s.people.Name(person) + " or the admin may use " + s.people.Name(person) + "'s Spotify."
		}
	}
	out := session{refresh: s.accountOf(person), person: person}
	if person != admin {
		out.label = s.people.Name(person)
	}
	if out.refresh == "" {
		return session{}, "Error: " + s.people.Name(person) + " has no Spotify account linked yet (tools/spotify_auth.py --other, " +
			"then SPOTIFY_ACCOUNTS in secrets.h)."
	}
	return out, ""
}

// allSessions: every linked account (for ducking).
func (s *Service) allSessions() []session {
	var out []session
	if s.admin != "" {
		out = append(out, session{refresh: s.admin, person: s.people.Admin()})
	}
	for _, a := range s.others {
		if a.refresh != "" {
			out = append(out, session{refresh: a.refresh, label: a.person})
		}
	}
	return out
}

// suffix names a non-admin account in results.
func (sess session) suffix() string {
	if sess.label != "" {
		return " (" + sess.label + "'s Spotify)"
	}
	return ""
}

// persisted is what tokenFile holds per account.
type persisted struct {
	Access string `json:"access"`
	User   string `json:"user"`
	Until  int64  `json:"until"` // unix seconds
}

// loadTokensLocked reads the saved access tokens once, at the first use (the clock is set by
// then; a token the server no longer knows costs one 401 and a refresh).
func (s *Service) loadTokensLocked() {
	if s.loaded {
		return
	}
	s.loaded = true
	var saved map[string]persisted
	if !store.LoadJSON(tokenFile, &saved) {
		return
	}
	for _, refresh := range append([]string{s.admin}, accountTokens(s.others)...) {
		if p, ok := saved[hashOf(refresh)]; ok && refresh != "" {
			s.tokens[refresh] = &accessToken{access: p.Access, user: p.User, until: time.Unix(p.Until, 0)}
		}
	}
}

func (s *Service) saveTokensLocked() {
	saved := map[string]persisted{}
	for refresh, t := range s.tokens {
		saved[hashOf(refresh)] = persisted{Access: t.access, User: t.user, Until: t.until.Unix()}
	}
	if err := store.SaveJSON(tokenFile, saved); err != nil {
		log.Printf("[Spotify] saving tokens failed: %v", err)
	}
}
