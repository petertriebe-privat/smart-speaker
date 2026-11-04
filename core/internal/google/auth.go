// Package google: the user's Google account over its REST APIs (Calendar, People, Drive, Tasks,
// Gmail), as devices/atoms3r/src/services/google.cpp. OAuth 2.0 with a refresh token
// (GOOGLE_CLIENT_ID / GOOGLE_CLIENT_SECRET / GOOGLE_REFRESH_TOKEN from secrets.h, or a newer one
// from a renewed login in state_dir/google.json); the access token is kept until shortly before
// it expires.
package google

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"echo-assistant/internal/netx"
	"echo-assistant/internal/store"
)

const tokenURL = "https://oauth2.googleapis.com/token"

// Scopes as SCOPES in tools/google_auth.py.
var Scopes = []string{
	"https://www.googleapis.com/auth/calendar.events", "https://www.googleapis.com/auth/calendar.readonly",
	"https://www.googleapis.com/auth/contacts", "https://www.googleapis.com/auth/drive.file",
	"https://www.googleapis.com/auth/tasks", "https://www.googleapis.com/auth/gmail.modify",
}

// ErrExpired: the login has to be renewed by the admin.
var ErrExpired = errors.New("Error: the Google login has expired or was revoked; the admin has to log in again " +
	"(tools/google_auth.py, then deploy the secrets to the Echo).")

type Credentials struct {
	ClientID, ClientSecret       string // installed-app client (tools/google_auth.py)
	WebClientID, WebClientSecret string // web client (login renewal through the relay)
	RefreshToken                 string
}

type token struct {
	access string
	until  time.Time
}

type Account struct {
	creds  Credentials
	web    bool // the refresh token came from the web client
	http   *http.Client
	mu     sync.Mutex
	tokens map[string]*token // per refresh token: the account's and other mailboxes'
	// LoginNeeded is set when the account's refresh token was refused.
	LoginNeeded bool

	// Hooks the caller sets before AddTools / Run (see hooks.go).
	Hooks Hooks

	cal      calendarState
	dir      directoryState
	login    loginState
	drv      *Drive
	drvOnce  sync.Once
	warmedUp atomic.Bool // searchContacts needs one warm-up request
}

// APIError: a Google API answered with a non-2xx status. Error() starts with "Error: " like
// every text for the model.
type APIError struct {
	Status int
	Text   string
}

func (e *APIError) Error() string { return "Error: " + e.Text }

// StatusOf: the HTTP status of an API error, 0 for other errors.
func StatusOf(err error) int {
	var api *APIError
	if errors.As(err, &api) {
		return api.Status
	}
	return 0
}

type saved struct {
	RefreshToken string `json:"refresh_token"`
	Web          bool   `json:"web"`
}

const savedFile = "google.json"

func New(c Credentials) *Account {
	a := &Account{creds: c, http: netx.Client(60 * time.Second), tokens: map[string]*token{}}
	var s saved
	if store.LoadJSON(savedFile, &s) && s.RefreshToken != "" {
		a.creds.RefreshToken, a.web = s.RefreshToken, s.Web
	}
	return a
}

func (a *Account) Configured() bool {
	return a.creds.ClientID != "" && a.creds.ClientSecret != "" && a.creds.RefreshToken != ""
}

// SetRefreshToken keeps a renewed login (from the web client) for later starts.
func (a *Account) SetRefreshToken(refresh string, web bool) error {
	a.mu.Lock()
	a.creds.RefreshToken, a.web, a.LoginNeeded = refresh, web, false
	a.tokens = map[string]*token{}
	a.mu.Unlock()
	return store.SaveJSON(savedFile, saved{refresh, web})
}

func (a *Account) accessToken(ctx context.Context, refresh string) (string, error) {
	own := refresh == ""
	a.mu.Lock()
	if own {
		refresh = a.creds.RefreshToken
	}
	web := own && a.web
	if t := a.tokens[refresh]; t != nil && time.Now().Before(t.until) {
		a.mu.Unlock()
		return t.access, nil
	}
	a.mu.Unlock()
	if refresh == "" { // web client only, never logged in
		if own {
			a.mu.Lock()
			a.LoginNeeded = true
			a.mu.Unlock()
		}
		return "", a.expiredError()
	}
	id, secret := a.creds.ClientID, a.creds.ClientSecret
	if web {
		id, secret = a.creds.WebClientID, a.creds.WebClientSecret
	}
	form := url.Values{"client_id": {id}, "client_secret": {secret}, "refresh_token": {refresh},
		"grant_type": {"refresh_token"}}
	req, _ := http.NewRequestWithContext(ctx, "POST", tokenURL, strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	resp, err := a.http.Do(req)
	if err != nil {
		return "", fmt.Errorf("Error: Google login failed (%v)", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	var out struct {
		Access    string `json:"access_token"`
		ExpiresIn int    `json:"expires_in"`
	}
	if resp.StatusCode != 200 || json.Unmarshal(body, &out) != nil || out.Access == "" {
		if resp.StatusCode == 400 && bytes.Contains(body, []byte("invalid_grant")) {
			if own {
				a.mu.Lock()
				a.LoginNeeded = true
				a.mu.Unlock()
				return "", a.expiredError()
			}
			return "", errors.New("Error: this mailbox's Google login has expired; its owner has to log in again " +
				"(tools/google_auth.py --gmail-only).")
		}
		return "", fmt.Errorf("Error: Google login failed (%s)", errorText(resp.StatusCode, body))
	}
	life := time.Duration(max(out.ExpiresIn, 600)-300) * time.Second
	a.mu.Lock()
	a.tokens[refresh] = &token{out.Access, time.Now().Add(life)}
	a.mu.Unlock()
	return out.Access, nil
}

func errorText(status int, body []byte) string {
	var e struct {
		Error any    `json:"error"`
		Desc  string `json:"error_description"`
	}
	if json.Unmarshal(body, &e) == nil {
		switch v := e.Error.(type) {
		case map[string]any:
			if m, ok := v["message"].(string); ok && m != "" {
				return fmt.Sprintf("HTTP %d: %s", status, m)
			}
		case string:
			if e.Desc != "" {
				return fmt.Sprintf("HTTP %d: %s", status, e.Desc)
			}
			return fmt.Sprintf("HTTP %d: %s", status, v)
		}
	}
	return fmt.Sprintf("HTTP %d", status)
}

// Request: one API call with the account's access token, or with that of another mailbox's
// refreshToken. A 401 fetches a new token once. Returns the body of a 2xx answer.
func (a *Account) Request(ctx context.Context, method, rawURL, contentType string, body []byte, refreshToken string) ([]byte, error) {
	for attempt := 0; attempt < 2; attempt++ {
		access, err := a.accessToken(ctx, refreshToken)
		if err != nil {
			return nil, err
		}
		var reader io.Reader
		if body != nil {
			reader = bytes.NewReader(body)
		}
		req, err := http.NewRequestWithContext(ctx, method, rawURL, reader)
		if err != nil {
			return nil, err
		}
		req.Header.Set("Authorization", "Bearer "+access)
		if contentType != "" {
			req.Header.Set("Content-Type", contentType)
		}
		resp, err := a.http.Do(req)
		if err != nil {
			return nil, fmt.Errorf("Error: Google not reachable (%v)", err)
		}
		data, _ := io.ReadAll(io.LimitReader(resp.Body, 32<<20))
		resp.Body.Close()
		if resp.StatusCode == 401 && attempt == 0 {
			a.mu.Lock()
			key := refreshToken
			if key == "" {
				key = a.creds.RefreshToken
			}
			delete(a.tokens, key)
			a.mu.Unlock()
			continue
		}
		if resp.StatusCode < 200 || resp.StatusCode >= 300 {
			return data, &APIError{resp.StatusCode, errorText(resp.StatusCode, data)}
		}
		return data, nil
	}
	return nil, errors.New("Error: Google refused the login")
}

// JSON: a request with a JSON body (nil = none) whose JSON answer goes into out (nil = ignored).
func (a *Account) JSON(ctx context.Context, method, rawURL string, in, out any) error {
	var body []byte
	ct := ""
	if in != nil {
		var err error
		if body, err = json.Marshal(in); err != nil {
			return err
		}
		ct = "application/json"
	}
	data, err := a.Request(ctx, method, rawURL, ct, body, "")
	if err != nil {
		return err
	}
	if out == nil || len(data) == 0 {
		return nil
	}
	return json.Unmarshal(data, out)
}
