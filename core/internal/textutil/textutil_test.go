package textutil

import "testing"

func TestSlug(t *testing.T) {
	cases := map[string]string{
		"Anna-Lena":         "anna-lena",
		"Jürgen Groß":       "juergen-gross",
		"  Max  ":           "max",
		"Küche & Bad!":      "kueche-bad",
		"Ölwechsel-Termin.": "oelwechsel-termin",
	}
	for in, want := range cases {
		if got := Slug(in, 40); got != want {
			t.Errorf("Slug(%q) = %q, want %q", in, got, want)
		}
	}
	if got := Slug("abcdefghij", 4); got != "abcd" {
		t.Errorf("cap: %q", got)
	}
}

func TestPhoneNormalized(t *testing.T) {
	cases := map[string]string{
		"+49 171 1234567":  "01711234567",
		"0049-171-1234567": "01711234567",
		"0171/1234567":     "01711234567",
		"**610":            "**610",
		"+43 1 234":        "00431234",
	}
	for in, want := range cases {
		if got := PhoneNormalized(in); got != want {
			t.Errorf("PhoneNormalized(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestPrefix(t *testing.T) {
	if got := Prefix("Grüße", 3); got != "Gr" {
		t.Errorf("Prefix split a character: %q", got)
	}
}
