package google

import (
	"context"
	"errors"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// Hooks: what the services need from the rest of the assistant. Set them before AddTools / Run; a nil
// hook is skipped.
type Hooks struct {
	// RefreshAgenda: a calendar event or a to-do is about to change; the agenda of the next
	// conversation has to be read again (agenda::refreshSoon on the M5).
	RefreshAgenda func()
	// Busy: a conversation is open; background work (address book, login messages) waits
	// (live::open on the M5).
	Busy func() bool
	// TellAdmin sends the admin a message: html for Telegram, plain for WhatsApp (Telegram if it
	// is set up, else WhatsApp). Returns "sent" or "Error: ...".
	TellAdmin func(html, plain string) string
	// RelayURL: the WhatsApp relay's base URL (WHATSAPP_RELAY_URL). Its /google page receives
	// Google's answer to a login link and passes code and state to LoginCallback.
	RelayURL string
	// LoggedIn: the login was renewed; what waited for Google (restores) may go on at once.
	LoggedIn func()
}

func (a *Account) refreshAgenda() {
	if a.Hooks.RefreshAgenda != nil {
		a.Hooks.RefreshAgenda()
	}
}

func (a *Account) busy() bool { return a.Hooks.Busy != nil && a.Hooks.Busy() }

// renewable: whether the admin can renew the login himself: web client and a way to reach him
// and the relay's /google page.
func (a *Account) renewable() bool {
	return a.creds.WebClientID != "" && a.creds.WebClientSecret != "" && a.Hooks.TellAdmin != nil &&
		a.Hooks.RelayURL != ""
}

// Enabled: the account is set up (desktop client with a refresh token) or can be set up by a
// login link (google::configured on the M5). The tools are only added then.
func (a *Account) Enabled() bool { return a.Configured() || a.renewable() }

// expiredError: the text for the model when the account's login is gone.
func (a *Account) expiredError() error {
	if a.renewable() {
		return errors.New("Error: the Google login has expired. The assistant sends the admin a link to log in again " +
			"(Telegram); Google works again once it has used it. Tell the user so; there is nothing else to do.")
	}
	return ErrExpired
}

func (a *Account) loginNeeded() bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.LoginNeeded
}

// callContext: one tool call's budget (Request has no deadline of its own beyond the client's).
func callContext() (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.Background(), 90*time.Second)
}

// enc encodes a query value or path segment (everything but unreserved characters, spaces as %20).
func enc(text string) string { return strings.ReplaceAll(url.QueryEscape(text), "+", "%20") }

// synced: the clock was set (the Echo starts in 1970).
func synced() bool { return time.Now().Year() >= 2024 }

const noClock = "Error: the clock is not set yet."

var errNotReadable = errors.New("Error: Google's answer is not readable")

// get fetches a JSON answer into out. Errors start with "Error: ".
func (a *Account) get(ctx context.Context, rawURL string, out any) error {
	if err := a.JSON(ctx, "GET", rawURL, nil, out); err != nil {
		if StatusOf(err) != 0 || strings.HasPrefix(err.Error(), "Error:") {
			return err
		}
		return errNotReadable
	}
	return nil
}

// --- Loose JSON access (Google's answers are read as maps, like ArduinoJson on the M5) -----------

// dig follows a path of map keys (string) and array indexes (int); nil if anything is missing.
func dig(v any, path ...any) any {
	for _, step := range path {
		switch s := step.(type) {
		case string:
			m, _ := v.(map[string]any)
			v = m[s]
		case int:
			l, _ := v.([]any)
			if s < 0 || s >= len(l) {
				return nil
			}
			v = l[s]
		}
	}
	return v
}

func digS(v any, path ...any) string {
	s, _ := dig(v, path...).(string)
	return s
}

func digN(v any, path ...any) int {
	n, _ := dig(v, path...).(float64)
	return int(n)
}

func digB(v any, path ...any) bool {
	b, _ := dig(v, path...).(bool)
	return b
}

func digL(v any, path ...any) []any {
	l, _ := dig(v, path...).([]any)
	return l
}

// sub is Arduino's String::substring: clamped, never panics.
func sub(s string, from, to int) string {
	if from > len(s) {
		from = len(s)
	}
	if to > len(s) {
		to = len(s)
	}
	if to < from {
		return ""
	}
	return s[from:to]
}

// --- Dates (time.Local; the caller sets Europe/Berlin = CALENDAR_TIMEZONE) --------------------------

var weekdays = [...]string{"So", "Mo", "Di", "Mi", "Do", "Fr", "Sa"}

// scanInts reads "a<sep>b<sep>c" like sscanf("%4d-%2d-%2d"): fields of at most the given widths.
func scanInts(s string, sep byte, widths ...int) ([]int, bool) {
	out := make([]int, 0, len(widths))
	i := 0
	for n, width := range widths {
		if n > 0 {
			if i >= len(s) || s[i] != sep {
				return nil, false
			}
			i++
		}
		start, v := i, 0
		for i < len(s) && i-start < width && s[i] >= '0' && s[i] <= '9' {
			v = v*10 + int(s[i]-'0')
			i++
		}
		if i == start {
			return nil, false
		}
		out = append(out, v)
	}
	return out, true
}

// parseLocal: "YYYY-MM-DD" and "HH:MM" ("" = midnight) as local time. False if malformed or after
// 2037 (the M5's 32-bit time_t; kept so the texts stay the same).
func parseLocal(date, clock string) (time.Time, bool) {
	d, ok := scanInts(date, '-', 4, 2, 2)
	if !ok {
		return time.Time{}, false
	}
	hour, minute := 0, 0
	if clock != "" {
		c, ok := scanInts(clock, ':', 2, 2)
		if !ok {
			return time.Time{}, false
		}
		hour, minute = c[0], c[1]
	}
	year, month, day := d[0], d[1], d[2]
	if year < 1970 || year > 2037 || hour > 23 || minute > 59 {
		return time.Time{}, false
	}
	t := time.Date(year, time.Month(month), day, hour, minute, 0, 0, time.Local)
	if t.Day() != day || int(t.Month()) != month { // no 31.02.
		return time.Time{}, false
	}
	return t, true
}

func addDays(t time.Time, days int) time.Time { return t.AddDate(0, 0, days) }

func dateStamp(t time.Time) string  { return t.Format("2006-01-02") }
func localStamp(t time.Time) string { return t.Format("2006-01-02T15:04:05") }
func utcStamp(t time.Time) string   { return t.UTC().Format("2006-01-02T15:04:05Z") }
func today() string                 { return dateStamp(time.Now()) }

// dayName: "2026-09-28" -> "Mo 28.09.2026"
func dayName(date string) string {
	t, ok := parseLocal(date, "12:00")
	if !ok {
		return date
	}
	return weekdays[t.Weekday()] + t.Format(" 02.01.2006")
}

// cellText: a spreadsheet cell or JSON value as text.
func cellText(v any) string {
	switch x := v.(type) {
	case string:
		return x
	case float64:
		return strconv.FormatFloat(x, 'f', -1, 64)
	case nil:
		return ""
	case bool:
		return strconv.FormatBool(x)
	}
	return ""
}

// argString: a string argument if it is given (even empty), like arguments[key].is<const char*>().
func argString(args map[string]any, key string) (string, bool) {
	s, ok := args[key].(string)
	return s, ok
}
