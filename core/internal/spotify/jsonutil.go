package spotify

import (
	"fmt"
	"strconv"
	"strings"
	"time"
)

// The Web API answers are decoded into any (maps, slices, float64, string, bool, nil). These
// helpers read them the way the M5's ArduinoJson code does: a missing or mistyped value is the
// zero value, never an error.

// get follows a path of object keys; nil if any step is missing.
func get(v any, path ...string) any {
	for _, key := range path {
		m, ok := v.(map[string]any)
		if !ok {
			return nil
		}
		v = m[key]
	}
	return v
}

func str(v any, path ...string) string {
	s, _ := get(v, path...).(string)
	return s
}

// num reads a JSON number; false if it is not one.
func num(v any, path ...string) (float64, bool) {
	f, ok := get(v, path...).(float64)
	return f, ok
}

// intOr reads a JSON number as int, def if it is missing.
func intOr(v any, def int, path ...string) int {
	if f, ok := num(v, path...); ok {
		return int(f)
	}
	return def
}

func flag(v any, path ...string) bool {
	b, _ := get(v, path...).(bool)
	return b
}

func list(v any, path ...string) []any {
	l, _ := get(v, path...).([]any)
	return l
}

// isNull: missing or JSON null.
func isNull(v any, path ...string) bool { return get(v, path...) == nil }

// lower trims and lower-cases for comparing names.
func lower(text string) string { return strings.ToLower(strings.TrimSpace(text)) }

// urlEncode percent-encodes everything but unreserved characters (RFC 3986), as the M5 does.
func urlEncode(text string) string {
	const hex = "0123456789ABCDEF"
	var b strings.Builder
	for i := 0; i < len(text); i++ {
		c := text[i]
		if c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '-' || c == '_' || c == '.' || c == '~' {
			b.WriteByte(c)
		} else {
			b.WriteByte('%')
			b.WriteByte(hex[c>>4])
			b.WriteByte(hex[c&15])
		}
	}
	return b.String()
}

// clock formats milliseconds as m:ss.
func clock(ms int64) string {
	seconds := int(ms / 1000)
	return fmt.Sprintf("%d:%02d", seconds/60, seconds%60)
}

// names joins the "name" of every entry of a list (artists, authors).
func names(entries []any) string {
	var out []string
	for _, entry := range entries {
		out = append(out, str(entry, "name"))
	}
	return strings.Join(out, ", ")
}

// describe gives one line for a track, episode, album, artist, playlist, show or audiobook.
func describe(item any) string {
	name := str(item, "name")
	switch str(item, "type") {
	case "track":
		return "'" + name + "' by " + names(list(item, "artists"))
	case "episode":
		return "episode '" + name + "' of " + orDefault(str(item, "show", "name"), "a podcast")
	case "album":
		return "album '" + name + "' by " + names(list(item, "artists"))
	case "artist":
		return "artist " + name
	case "playlist":
		return "playlist '" + name + "' (by " + orDefault(str(item, "owner", "display_name"), "?") + ")"
	case "show":
		return "podcast '" + name + "'"
	case "audiobook":
		return "audiobook '" + name + "' by " + names(list(item, "authors"))
	}
	return "'" + name + "'"
}

// orDefault: ArduinoJson's `value | "default"` also replaces an empty string only when the value
// is missing; Spotify never sends empty names for these fields, so empty counts as missing.
func orDefault(s, def string) string {
	if s == "" {
		return def
	}
	return s
}

// ago says how long ago an ISO 8601 UTC stamp was ("5 min ago", "3 h ago", "4 days ago"); ""
// if the stamp does not parse or lies in the future.
func ago(stamp string, now time.Time) string {
	then, err := time.Parse(time.RFC3339, stamp)
	if err != nil || then.Unix() <= 0 || now.Before(then) {
		return ""
	}
	minutes := int(now.Sub(then) / time.Minute)
	switch {
	case minutes < 60:
		return strconv.Itoa(minutes) + " min ago"
	case minutes < 48*60:
		return strconv.Itoa(minutes/60) + " h ago"
	}
	return strconv.Itoa(minutes/1440) + " days ago"
}

// year is the first four characters of a release date.
func year(date string) string {
	if len(date) > 4 {
		return date[:4]
	}
	return date
}

func clamp(v, lo, hi int) int { return min(max(v, lo), hi) }
