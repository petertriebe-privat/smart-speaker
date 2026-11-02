// Package textutil: small text helpers shared by the modules, as devices/atoms3r/src/system/
// {text_util,phone_number,utf8}.h.
package textutil

import (
	"strings"
	"unicode"
	"unicode/utf8"
)

// Slug: "Anna-Lena Müller" -> "anna-lena-mueller" (ids, topic keys), at most maxLength bytes.
func Slug(text string, maxLength int) string {
	var b strings.Builder
	for _, r := range text {
		if b.Len() >= maxLength {
			break
		}
		switch {
		case r < 0x80 && (unicode.IsLetter(r) || unicode.IsDigit(r)):
			b.WriteRune(unicode.ToLower(r))
		case r == 'ä' || r == 'Ä':
			b.WriteString("ae")
		case r == 'ö' || r == 'Ö':
			b.WriteString("oe")
		case r == 'ü' || r == 'Ü':
			b.WriteString("ue")
		case r == 'ß':
			b.WriteString("ss")
		case r >= 0x80:
			// other non-ASCII letters are dropped, as on the M5
		default:
			if s := b.String(); s != "" && !strings.HasSuffix(s, "-") {
				b.WriteByte('-')
			}
		}
	}
	return strings.TrimRight(b.String(), "-")
}

// PhoneNormalized: numbers as the FritzBox writes them: +49171... / 0049171... / 0171... all
// as 0171... ("**610": the FritzBox's own phones).
func PhoneNormalized(number string) string {
	var b strings.Builder
	for _, r := range number {
		if r >= '0' && r <= '9' || r == '*' || r == '+' && b.Len() == 0 {
			b.WriteRune(r)
		}
	}
	digits := b.String()
	if strings.HasPrefix(digits, "+") {
		digits = "00" + digits[1:]
	}
	if strings.HasPrefix(digits, "0049") {
		digits = "0" + digits[4:]
	}
	return digits
}

// Prefix cuts s to at most n bytes without splitting a UTF-8 character.
func Prefix(s string, n int) string {
	if len(s) <= n {
		return s
	}
	for n > 0 && !utf8.RuneStart(s[n]) {
		n--
	}
	return s[:n]
}

// Repair replaces invalid UTF-8 (mail texts): the API rejects events with broken bytes.
func Repair(s string) string { return strings.ToValidUTF8(s, "?") }

// HTML escapes text for Telegram's HTML mode and mails.
func HTML(text string) string {
	return strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;").Replace(text)
}

// Folded: lower case for searching.
func Folded(text string) string { return strings.ToLower(strings.TrimSpace(text)) }
