package chatcore

import (
	"strings"
	"testing"
)

func TestSplit(t *testing.T) {
	text := strings.Repeat("a", 60) + "\n" + strings.Repeat("b", 60) + "\n\n" + strings.Repeat("c", 20)
	parts := Split(text, 100)
	if len(parts) != 2 || parts[0] != strings.Repeat("a", 60) || !strings.HasPrefix(parts[1], "b") {
		t.Errorf("Split: %q", parts)
	}
	if got := Split("kurz", 100); len(got) != 1 || got[0] != "kurz" {
		t.Errorf("short: %q", got)
	}
	// Never inside a multi-byte character.
	for _, p := range Split(strings.Repeat("ä", 100), 51) {
		if strings.ContainsRune(p, 0xFFFD) || len(p) > 51 {
			t.Errorf("bad part %q", p)
		}
	}
}

func TestReadable(t *testing.T) {
	for _, c := range []struct {
		mime, name string
		want       bool
	}{
		{"application/pdf", "a", true}, {"", "A.PDF", true}, {"text/plain", "x", true}, {"image/png", "x", true},
		{"application/octet-stream", "data.csv", true}, {"application/zip", "a.zip", false}, {"application/json", "x", true},
	} {
		if got := Readable(c.mime, c.name); got != c.want {
			t.Errorf("Readable(%q, %q) = %v", c.mime, c.name, got)
		}
	}
}

func TestStripSourcesAndAddresses(t *testing.T) {
	if got := StripSources("Zehn Uhr ([a.de](https://a.de)) fertig"); got != "Zehn Uhr  fertig" {
		t.Errorf("StripSources: %q", got)
	}
	if got := StripSources("offen ([a"); got != "offen ([a" {
		t.Errorf("StripSources open: %q", got)
	}
	if got := StripAddresses("siehe https://a.de/x und http://b.de."); got != "siehe  und " {
		t.Errorf("StripAddresses: %q", got)
	}
}

func TestHow(t *testing.T) {
	if got := How("Telegram", true, "", nil); got != "per Telegram, Sprachnachricht" {
		t.Error(got)
	}
	if got := How("WhatsApp", false, "data:", nil); got != "per WhatsApp, mit Foto" {
		t.Error(got)
	}
	f := &Attachment{Name: "a.pdf", Data: make([]byte, 2048)}
	if got := How("Telegram", false, "", f); got != "per Telegram, mit Datei a.pdf (2 KB)" {
		t.Error(got)
	}
	if got := How("Telegram", false, "", nil); got != "per Telegram" {
		t.Error(got)
	}
}
