package google

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"io"
	"log"
	"math/big"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"echo-assistant/internal/prompts"
	"echo-assistant/internal/store"
	"echo-assistant/internal/tools"
)

// Renewing the login: a link for the admin, answered through the relay. Google's device flow does
// not allow calendar or Gmail scopes, so the admin gets an ordinary authorization link (web
// client, PKCE) by Telegram or WhatsApp. Google sends the browser to the relay's /google page,
// which queues code and state like a WhatsApp event (the caller passes them to LoginCallback);
// Run exchanges the code (useless without the client secret and the verifier, both only here) and
// keeps the refresh token through SetRefreshToken. State and verifier are kept in
// state_dir/google_login.json too, so a link survives a restart. As loginWork, sendLink,
// exchangeCode and loginCallback in devices/atoms3r/src/services/google.cpp.

const pendingFile = "google_login.json"

type loginState struct {
	mu       sync.Mutex
	state    string // the pending link
	verifier string
	since    time.Time
	code     string    // from the relay, exchanged by loginWork
	linkSent time.Time // zero = not yet (loginWork only)
	loaded   bool
}

type pending struct {
	State    string `json:"state"`
	Verifier string `json:"verifier"`
	Since    int64  `json:"since"`
}

func randomText(length int) string {
	const alphabet = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789"
	out := make([]byte, length)
	for i := range out {
		n, err := rand.Int(rand.Reader, big.NewInt(int64(len(alphabet))))
		if err != nil {
			panic("google: no randomness: " + err.Error())
		}
		out[i] = alphabet[n.Int64()]
	}
	return string(out)
}

func (a *Account) redirectURI() string {
	return strings.TrimRight(a.Hooks.RelayURL, "/") + "/google"
}

func (a *Account) loadPendingLocked() {
	l := &a.login
	if l.loaded || l.state != "" {
		return
	}
	l.loaded = true
	var p pending
	if store.LoadJSON(pendingFile, &p) {
		l.state, l.verifier, l.since = p.State, p.Verifier, time.Unix(p.Since, 0)
	}
}

// codeChallenge: base64url without padding of the verifier's SHA-256 (PKCE, S256).
func codeChallenge(verifier string) string {
	hash := sha256.Sum256([]byte(verifier))
	return base64.RawURLEncoding.EncodeToString(hash[:])
}

// loginURLLocked: the link for the pending state (a new one after GOOGLE_LOGIN_LINK_S).
func (a *Account) loginURLLocked() string {
	l := &a.login
	a.loadPendingLocked()
	now := time.Now()
	linkAge := time.Duration(prompts.Int("GOOGLE_LOGIN_LINK_S", 604800)) * time.Second
	if l.state == "" || (synced() && now.Sub(l.since) > linkAge) {
		l.state, l.verifier, l.since = randomText(32), randomText(64), now
		if err := store.SaveJSON(pendingFile, pending{l.state, l.verifier, now.Unix()}); err != nil {
			log.Printf("[Google] login state not saved: %v", err)
		}
	}
	return "https://accounts.google.com/o/oauth2/v2/auth?client_id=" + enc(a.creds.WebClientID) +
		"&redirect_uri=" + enc(a.redirectURI()) + "&response_type=code&access_type=offline&prompt=consent" +
		"&scope=" + enc(strings.Join(Scopes, " ")) + "&state=" + l.state + "&code_challenge=" + codeChallenge(l.verifier) +
		"&code_challenge_method=S256"
}

// SendLink sends the admin the link to log in again. Returns "sent" or an error text.
func (a *Account) SendLink(expired bool) string {
	if !a.renewable() {
		return "Error: renewing the login needs GOOGLE_WEB_CLIENT_ID/SECRET and the WhatsApp relay."
	}
	a.login.mu.Lock()
	link := a.loginURLLocked()
	a.login.mu.Unlock()
	intro := "Hier können Sie meine Google-Anmeldung erneuern. "
	if expired {
		intro = "Meine Google-Anmeldung ist abgelaufen, ich komme nicht an Kalender, Kontakte und Drive. "
	}
	ask := "Bitte melden Sie sich an und setzen Sie alle Häkchen:"
	href := strings.ReplaceAll(link, "&", "&amp;")
	return a.Hooks.TellAdmin(intro+ask+"\n<a href=\""+href+"\">Bei Google anmelden</a>", intro+ask+"\n"+link)
}

func (a *Account) tellAdmin(text string) {
	if a.Hooks.TellAdmin != nil {
		a.Hooks.TellAdmin(text, text)
	}
}

func (a *Account) exchangeCode(code string) {
	a.login.mu.Lock()
	verifier := a.login.verifier
	a.login.mu.Unlock()
	form := url.Values{"code": {code}, "client_id": {a.creds.WebClientID}, "client_secret": {a.creds.WebClientSecret},
		"redirect_uri": {a.redirectURI()}, "grant_type": {"authorization_code"}, "code_verifier": {verifier}}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	status, body, err := a.postForm(ctx, form)
	var parsed struct {
		Refresh string `json:"refresh_token"`
		Scope   string `json:"scope"`
	}
	if err != nil || status != 200 || json.Unmarshal(body, &parsed) != nil || parsed.Refresh == "" {
		why := "no refresh token"
		if err != nil {
			why = err.Error()
		} else if status != 200 {
			why = errorText(status, body)
		}
		log.Printf("[Google] login link failed (%s)", why)
		a.tellAdmin("Die Google-Anmeldung hat nicht geklappt (" + why + "). Bitte öffnen Sie den Link noch einmal.")
		return
	}
	var missing []string
	for _, scope := range Scopes {
		if !strings.Contains(parsed.Scope, scope) {
			missing = append(missing, scope[strings.LastIndexByte(scope, '/')+1:])
		}
	}
	if len(missing) > 0 {
		a.tellAdmin("Es fehlen Berechtigungen (" + strings.Join(missing, ", ") + "). Bitte öffnen Sie den Link noch " +
			"einmal und setzen Sie alle Häkchen.")
		return
	}
	if err := a.SetRefreshToken(parsed.Refresh, true); err != nil {
		log.Printf("[Google] renewed login not saved: %v", err)
	}
	store.Remove(pendingFile)
	a.login.mu.Lock()
	a.login.state, a.login.verifier, a.login.loaded = "", "", true
	a.login.linkSent = time.Time{}
	a.login.mu.Unlock()
	log.Print("[Google] login renewed by link")
	if a.Hooks.LoggedIn != nil {
		go a.Hooks.LoggedIn()
	}
	a.tellAdmin("Danke, meine Google-Anmeldung ist erneuert. Kalender, Kontakte und Drive gehen wieder.")
}

// postForm posts a form to the token endpoint.
func (a *Account) postForm(ctx context.Context, form url.Values) (int, []byte, error) {
	req, err := http.NewRequestWithContext(ctx, "POST", tokenURL, strings.NewReader(form.Encode()))
	if err != nil {
		return 0, nil, err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	resp, err := a.http.Do(req)
	if err != nil {
		return 0, nil, err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	return resp.StatusCode, body, nil
}

// LoginCallback: the relay's answer to a login link (google_login, sent by itself when the login
// expired): queues the code for Run if state is the pending link's. From any goroutine, does not
// block.
func (a *Account) LoginCallback(code, state string) {
	if code == "" || len(code) > 512 || !a.renewable() {
		return
	}
	a.login.mu.Lock()
	a.loadPendingLocked()
	ours := a.login.state != "" && state == a.login.state
	if ours {
		a.login.code = code
	}
	a.login.mu.Unlock()
	if ours {
		log.Print("[Google] login code received")
	} else {
		log.Print("[Google] login answer with an unknown state ignored")
	}
}

// loginWork: exchanges a received code, or sends the link when the login expired (again after
// GOOGLE_LOGIN_REMIND_MS). Not during a conversation.
func (a *Account) loginWork() {
	if a.busy() {
		return
	}
	a.login.mu.Lock()
	code := a.login.code
	a.login.code = ""
	a.login.mu.Unlock()
	if code != "" {
		a.exchangeCode(code)
		return
	}
	remind := time.Duration(prompts.Int("GOOGLE_LOGIN_REMIND_MS", 43200000)) * time.Millisecond
	if !a.loginNeeded() || (!a.login.linkSent.IsZero() && time.Since(a.login.linkSent) < remind) {
		return
	}
	a.login.linkSent = time.Now()
	result := a.SendLink(true)
	log.Printf("[Google] login expired, link to the admin: %s", result)
}

// addLoginTool registers google_login when the login can be renewed by a link.
func (a *Account) addLoginTool(r *tools.Registry) {
	if !a.renewable() {
		return
	}
	r.Add(tools.Tool{
		Name: "google_login",
		Description: "Sends the admin a link (Telegram) to renew the assistant's Google login. Only when the admin asks for it; when " +
			"the login has expired the link goes out by itself.",
		Parameters: `{"type": "object", "additionalProperties": false, "properties": {}}`,
		Private:    true, Channels: tools.Everywhere,
		Run: func(map[string]any, tools.Context) string {
			result := a.SendLink(a.loginNeeded())
			if result == "sent" {
				return "Link sent to the admin."
			}
			return result
		}})
}
