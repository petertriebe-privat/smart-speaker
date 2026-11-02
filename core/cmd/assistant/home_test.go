package main

import "testing"

func TestHomeCandidate(t *testing.T) {
	for text, want := range map[string]bool{
		"Licht in der Küche aus.":             true,
		"Hey Jarvis, mach bitte das Licht an": true,
		"Rollos im Wohnzimmer runter":         true,
		"Stell die Heizung auf 21 Grad":       true,
		"Dimm die Lampe im Flur":              true,
		"Ist das Licht in der Küche an?":      false, // a question: GPT Live
		"Fernseher aus":                       false, // fire_tv, not HA's sentences
		"Wann ist es dunkel heute":            false,
		"Pause":                               false,
		"Jarvis, Küche aus":                   true, // a bare room: its light
		"Wohnzimmer an.":                      true,
		"Musik aus":                           false, // the music commands'
		"Alles aus":                           false,
		"Erzähl mir, wie man das Licht in einem Bild so setzt, dass es warm und weich wirkt": false, // long
	} {
		if got := homeCandidate(text); got != want {
			t.Errorf("homeCandidate(%q) = %v, want %v", text, got, want)
		}
	}
}

func TestHomeWords(t *testing.T) {
	for text, want := range map[string]string{
		"Hey Jarvis, Licht in der Küche aus.":    "Licht in der Küche aus",
		"Ja, Sir. Rollos runter!":                "Rollos runter",
		"Mach das Licht an":                      "Mach das Licht an",
		"Schalt dir das Licht in der Küche aus.": "Schalt das Licht in der Küche aus",
	} {
		if got := homeWords(text); got != want {
			t.Errorf("homeWords(%q) = %q, want %q", text, got, want)
		}
	}
}

func TestWithRoom(t *testing.T) {
	for words, want := range map[string]string{
		"Schalte das Licht ein": "Schalte das Licht im Wohnzimmer ein", // HA: "… ein im Wohnzimmer" failed
		"Licht aus":             "Licht im Wohnzimmer aus",
		"Rollos runter":         "Rollos im Wohnzimmer runter",
		"Dimm die Lampe":        "Dimm die Lampe im Wohnzimmer",
	} {
		if got := withRoom(words, "Wohnzimmer"); got != want {
			t.Errorf("withRoom(%q) = %q, want %q", words, got, want)
		}
	}
	if !bareRoom("Küche aus") || bareRoom("Licht aus") || bareRoom("Fernseher aus") || bareRoom("Licht in der Küche aus") {
		t.Error("bareRoom")
	}
}

// TestBareRoomNotMusic: "Küche aus" must reach the home path, not a music command.
func TestBareRoomNotMusic(t *testing.T) {
	if cmd := parseMusicCommand("Küche aus"); cmd.action != "" {
		t.Errorf("parseMusicCommand(Küche aus) = %+v", cmd)
	}
}

func TestNamesPlace(t *testing.T) {
	for text, want := range map[string]bool{
		"Licht in der Küche aus":    true,
		"Rollos im Wohnzimmer hoch": true,
		"Licht aus":                 false, // this speaker's area goes first
		"Licht auf hundert Prozent": false,
		"Schreibtischlampe an":      false,
	} {
		if got := namesPlace(text); got != want {
			t.Errorf("namesPlace(%q) = %v, want %v", text, got, want)
		}
	}
}

func TestHomeIntent(t *testing.T) {
	if got := commandOf("home", 0.8); got.action != "home" {
		t.Errorf("commandOf(home) = %+v", got)
	}
	if got := commandOf("home", 0.2); got.action != "" {
		t.Errorf("commandOf(home, unsure) = %+v", got)
	}
}
