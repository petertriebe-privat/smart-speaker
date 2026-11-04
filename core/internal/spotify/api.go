package spotify

import (
	"bytes"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"log"
	"net"
	"net/http"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"echo-assistant/internal/textutil"
)

// --- Secrets ----------------------------------------------------------------------------

// account: another person's Spotify account.
type account struct {
	person  string // name or id in the register, "" = the admin
	refresh string
}

var pairPattern = regexp.MustCompile(`\{\s*("(?:[^"\\]|\\.)*")\s*,\s*("(?:[^"\\]|\\.)*")\s*\}`)

// parseAccounts reads SPOTIFY_ACCOUNTS. The M5's macro expands to a C++ initializer list
// (`{"anna", "token"}, {"ben", "token"}`), which make_secrets.py cannot represent, so the value
// may be any of: that list as a string, a JSON array of [person, token] pairs, a JSON array of
// objects {"person", "refresh"}, or a JSON object {person: token}. Anything else is ignored.
func parseAccounts(v any) []account {
	var out []account
	add := func(person, refresh string) {
		if refresh = strings.TrimSpace(refresh); refresh != "" {
			out = append(out, account{strings.TrimSpace(person), refresh})
		}
	}
	switch t := v.(type) {
	case string:
		var decoded any
		if json.Unmarshal([]byte(t), &decoded) == nil {
			return parseAccounts(decoded)
		}
		for _, m := range pairPattern.FindAllStringSubmatch(t, -1) {
			var person, refresh string
			if json.Unmarshal([]byte(m[1]), &person) == nil && json.Unmarshal([]byte(m[2]), &refresh) == nil {
				add(person, refresh)
			}
		}
	case []any:
		for _, item := range t {
			switch e := item.(type) {
			case []any:
				if len(e) == 2 {
					person, _ := e[0].(string)
					refresh, _ := e[1].(string)
					add(person, refresh)
				}
			case map[string]any:
				add(firstString(e, "person", "name"), firstString(e, "refresh", "refresh_token", "token"))
			}
		}
	case map[string]any:
		keys := make([]string, 0, len(t))
		for k := range t {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			refresh, _ := t[k].(string)
			add(k, refresh)
		}
	}
	return out
}

func firstString(m map[string]any, keys ...string) string {
	for _, k := range keys {
		if s, ok := m[k].(string); ok {
			return s
		}
	}
	return ""
}

func accountTokens(list []account) []string {
	var out []string
	for _, a := range list {
		out = append(out, a.refresh)
	}
	return out
}

// parseRoomSpeakers reads SPOTIFY_ROOM_SPEAKERS (comma separated, or a JSON array of strings)
// into lower case names.
func parseRoomSpeakers(v any) []string {
	var raw []string
	switch t := v.(type) {
	case string:
		raw = strings.Split(t, ",")
	case []any:
		for _, item := range t {
			if s, ok := item.(string); ok {
				raw = append(raw, s)
			}
		}
	}
	var out []string
	for _, name := range raw {
		if name = lower(name); name != "" {
			out = append(out, name)
		}
	}
	return out
}

// hashOf names a token in the saved file without writing the refresh token twice.
func hashOf(refresh string) string {
	sum := sha256.Sum256([]byte(refresh))
	return hex.EncodeToString(sum[:8])
}

// --- HTTP and OAuth ---------------------------------------------------------------------

// Transport failures as status codes below zero.
const (
	statusTimeout = -1
	statusFailed  = -2
)

// send does one request and returns the status and body; status below zero (with err) if there
// was no answer. A connection the server closed meanwhile is opened anew once, as on the M5.
func (s *Service) send(method, target string, header http.Header, body []byte) (int, []byte, error) {
	var lastErr error
	for attempt := 0; attempt < 2; attempt++ {
		req, err := http.NewRequest(method, target, bytes.NewReader(body))
		if err != nil {
			return statusFailed, nil, err
		}
		for k, v := range header {
			req.Header[k] = v
		}
		resp, err := s.http.Do(req)
		if err != nil {
			lastErr = err
			var netErr net.Error
			if errors.As(err, &netErr) && netErr.Timeout() {
				return statusTimeout, nil, err
			}
			continue
		}
		reply, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
		resp.Body.Close()
		if err != nil {
			var netErr net.Error
			if errors.As(err, &netErr) && netErr.Timeout() {
				return statusTimeout, nil, err
			}
			return statusFailed, nil, err
		}
		return resp.StatusCode, reply, nil
	}
	return statusFailed, nil, lastErr
}

// errorText describes a failed call for the model.
func errorText(status int, reply []byte, err error) string {
	switch {
	case status == statusTimeout:
		return "Spotify does not answer"
	case status < 0:
		detail := "unknown"
		if err != nil {
			detail = err.Error()
		}
		return "Spotify not reachable (" + detail + ")"
	case status == 429:
		return "Spotify's rate limit, try again in a minute"
	}
	var parsed any
	var message string
	if len(reply) > 0 && json.Unmarshal(reply, &parsed) == nil {
		message = str(parsed, "error", "message") // API
		if message == "" {
			message = str(parsed, "error_description") // token endpoint
		}
		if message == "" {
			message = str(parsed, "error")
		}
	}
	if status == 403 && strings.Contains(message, "remium") {
		message += " (this needs Spotify Premium)"
	}
	text := "HTTP " + itoa(status)
	if message != "" {
		text += ": " + textutil.Prefix(message, 200)
	}
	return text
}

// accessToken: the access token of one account with the Spotify user id.
type accessToken struct {
	access, user string
	until        time.Time
}

// authorizeLocked makes sure the entry has a valid access token (fetched with the refresh
// token, kept until 5 minutes before it expires). Returns the error text or "".
func (s *Service) authorizeLocked(refresh string, entry *accessToken) string {
	if entry.access != "" && time.Now().Before(entry.until) {
		return ""
	}
	header := http.Header{
		"Content-Type":  {"application/x-www-form-urlencoded"},
		"Authorization": {"Basic " + base64.StdEncoding.EncodeToString([]byte(s.clientID+":"+s.clientSecret))},
	}
	form := "grant_type=refresh_token&refresh_token=" + urlEncode(refresh)
	status, reply, err := s.send("POST", s.token, header, []byte(form))
	var parsed any
	if status != 200 || json.Unmarshal(reply, &parsed) != nil || str(parsed, "access_token") == "" {
		if status == 400 && bytes.Contains(reply, []byte("invalid_grant")) {
			return "Error: the Spotify login expired or was revoked; tools/spotify_auth.py has to run once more."
		}
		return "Error: Spotify login failed (" + errorText(status, reply, err) + ")"
	}
	entry.access = str(parsed, "access_token")
	expires := max(intOr(parsed, 3600, "expires_in"), 600)
	entry.until = time.Now().Add(time.Duration(expires-300) * time.Second)
	s.saveTokensLocked()
	return ""
}

func (s *Service) entryLocked(refresh string) *accessToken {
	s.loadTokensLocked()
	t := s.tokens[refresh]
	if t == nil {
		t = &accessToken{}
		s.tokens[refresh] = t
	}
	return t
}

func (s *Service) authorize(refresh string) (string, string) {
	s.tokenMu.Lock()
	defer s.tokenMu.Unlock()
	entry := s.entryLocked(refresh)
	if errText := s.authorizeLocked(refresh, entry); errText != "" {
		return entry.access, errText
	}
	return entry.access, ""
}

func (s *Service) dropToken(refresh string) {
	s.tokenMu.Lock()
	defer s.tokenMu.Unlock()
	s.entryLocked(refresh).access = ""
}

// call makes one Web API request (`path` from /v1 on, `body` JSON or nil). Returns the HTTP
// status (0 without a token), the reply and, unless the status is 2xx, the error text for the
// model. A 401 fetches a new token once.
func (s *Service) call(sess session, method, path string, body []byte) (int, []byte, string) {
	for attempt := 0; ; attempt++ {
		token, errText := s.authorize(sess.refresh)
		if errText != "" {
			return 0, nil, errText
		}
		header := http.Header{"Authorization": {"Bearer " + token}}
		if len(body) > 0 {
			header.Set("Content-Type", "application/json")
		}
		status, reply, err := s.send(method, s.api+"/v1"+path, header, body)
		if status == 401 && attempt == 0 {
			s.dropToken(sess.refresh)
			continue
		}
		if status < 200 || status >= 300 {
			errText = "Error: " + errorText(status, reply, err)
		}
		return status, reply, errText
	}
}

// callJSON is a call whose answer is JSON. A 204 or an empty answer gives a nil document. The
// error text is "" on success.
func (s *Service) callJSON(sess session, method, path string, body []byte) (doc any, status int, errText string) {
	status, reply, errText := s.call(sess, method, path, body)
	if status < 200 || status >= 300 {
		return nil, status, errText
	}
	if status == 204 || len(reply) == 0 {
		return nil, status, ""
	}
	if err := json.Unmarshal(reply, &doc); err != nil {
		return nil, status, "Error: Spotify sent no valid JSON"
	}
	return doc, status, ""
}

// getJSON: GET and parse.
func (s *Service) getJSON(sess session, path string) (any, string) {
	doc, _, errText := s.callJSON(sess, "GET", path, nil)
	return doc, errText
}

// do is a call without an interesting answer: "" or the error.
func (s *Service) do(sess session, method, path string, body []byte) string {
	_, _, errText := s.call(sess, method, path, body)
	return errText
}

// userID: the account's Spotify user id (kept with its token).
func (s *Service) userID(sess session) (string, string) {
	s.tokenMu.Lock()
	known := s.entryLocked(sess.refresh).user
	s.tokenMu.Unlock()
	if known != "" {
		return known, ""
	}
	me, errText := s.getJSON(sess, "/me")
	if errText != "" {
		return "", errText
	}
	id := str(me, "id")
	s.tokenMu.Lock()
	s.entryLocked(sess.refresh).user = id
	s.saveTokensLocked()
	s.tokenMu.Unlock()
	return id, ""
}

func marshal(v any) []byte {
	data, err := json.Marshal(v)
	if err != nil {
		log.Printf("[Spotify] encoding: %v", err)
	}
	return data
}

func itoa(n int) string { return strconv.Itoa(n) }
