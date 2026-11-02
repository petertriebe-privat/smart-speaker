package main

import "testing"

func TestParseMusicCommand(t *testing.T) {
	cases := map[string]musicCommand{
		"Pause.":                                 {action: "pause"},
		"Hey Jarvis, Pause!":                     {action: "pause"},
		"Stopp":                                  {action: "pause"},
		"Musik aus, bitte.":                      {action: "pause"},
		"Weiter.":                                {action: "weiter"},
		"Spiel weiter":                           {action: "resume"},
		"Nächstes Lied, bitte":                   {action: "next"},
		"Zurück":                                 {action: "previous"},
		"Etwas lauter.":                          {action: "volume", step: 10},
		"Bitte mal ein bisschen leiser!":         {action: "volume", step: -10},
		"Viel leiser":                            {action: "volume", step: -25},
		"lízer":                                  {action: "volume", step: -10},
		"Spiele meine Lieblingsmusik.":           {action: "liked"},
		"Spiel bitte meine Lieblingssongs":       {action: "liked"},
		"Spielen meine Lieblingsmusik.":          {action: "liked"},
		"Servis, spiel was von Deichkind.":       {action: "artist", what: "deichkind"},
		"Spiel das Album Discovery":              {action: "play"},
		"Spiel was von Deichkind im Wohnzimmer.": {action: "artist", what: "deichkind", where: "wohnzimmer"},
		"Pause im Kinderzimmer":                  {action: "pause", where: "kinderzimmer"},
		"Spiel meine Lieblingsmusik auf dem Echo Küche": {action: "liked", where: "echo küche"},
		"Die Musik von Eminem.":                         {action: "artist", what: "eminem"},
		"Spiele 8 Mile von Eminem.":                     {action: "play"},
		// stems, objects and the assistant's own acknowledgement (#59, 18.10.2026)
		"Pausiere die Musik":                           {action: "pause"},
		"Stoppe die Musik.":                            {action: "pause"},
		"Ja, Sir. Pause.":                              {action: "pause"},
		"Lied überspringen":                            {action: "next"},
		"Mach die Musik leiser":                        {action: "volume", step: -10},
		"Ja, Sir Spiel mal bitte meine Lieblingsmusik": {action: "liked"},
		// the video on the TV, "my Spotify music" (18.10.2026)
		"YouTube-Video pausieren":                       {action: "video_pause"},
		"Pausiere das Video im Arbeitszimmer":           {action: "video_pause", where: "arbeitszimmer"},
		"Video weiter":                                  {action: "video_play"},
		"Fernseher aus":                                 {},
		"Spiel meine Spotify-Musik":                     {action: "continue"},
		"Spiel meine Musik":                             {action: "continue"},
		"Spiel mir mal das Spotify-Musik":               {action: "continue"},
		"Spiel jetzt meine Lieblingslieder auf Spotify": {action: "liked"},
		"Einfach meine Lieblingsmusik":                  {action: "liked"},
		// not a command alone: GPT Live answers
		"Pause, und wie wird das Wetter?":       {},
		"Spiel was von Daft Punk":               {action: "artist", what: "daft punk"},
		"Solltest Musik von Deichkind spielen.": {},
		"Spiele Musik von Deichkind":            {action: "artist", what: "deichkind"},
		"Spiel":                                 {},
		"Wie spät ist es?":                      {},
		"Mach das Licht aus":                    {},
		"":                                      {},
	}
	for text, want := range cases {
		if got := parseMusicCommand(text); got != want {
			t.Errorf("%q: got %+v, want %+v", text, got, want)
		}
	}
}

func TestParseMobileCommand(t *testing.T) {
	for text, want := range map[string]string{
		"Hey Jarvis, geh ran.":      "answer",
		"Jarvis, nimm bitte ab!":    "answer",
		"Nimm den Anruf an.":        "answer",
		"Lehne ab.":                 "reject",
		"Jarvis, drück ihn weg":     "reject",
		"Leg auf.":                  "hangup",
		"Verbindung beenden.":       "hangup",
		"Wer ruft an?":              "",
		"Geh ran und sag ihr, dass": "",
	} {
		if got := parseMobileCommand(text); got != want {
			t.Errorf("%q: %q, want %q", text, got, want)
		}
	}
}

func TestComplete(t *testing.T) {
	cases := []struct {
		text    string
		settled bool
		want    bool
	}{
		{"Spiele Musik von Eminem.", false, true},
		{"Spiele Musik von Eminem", false, false},
		{"Spiele Musik von Eminem", true, true}, // GPT Live's transcript without a full stop
		{"spiele Musik von...", true, false},
		{"Hey Jarvis, spiel was von", true, false},
		{"Spiel meine Lieblingsmusik im", true, false},
	}
	for _, c := range cases {
		if got := complete(c.text, c.settled); got != c.want {
			t.Errorf("complete(%q, %v) = %v, want %v", c.text, c.settled, got, c.want)
		}
	}
}

func TestMoveAndPlace(t *testing.T) {
	for text, want := range map[string]musicCommand{
		"Hol die Musik zu dir zurück.": {action: "move_here"},
		"Spiel hier weiter":            {action: "move_here"},
	} {
		if got := parseMusicCommand(text); got != want {
			t.Errorf("%q: got %+v, want %+v", text, got, want)
		}
	}
	// the verb at the end is not part of the place (18.10.2026)
	if got := placeIn("Kannst du die Wiedergabe auf dem Echo im Arbeitszimmer fortsetzen?"); got != "arbeitszimmer" {
		t.Errorf("placeIn: %q", got)
	}
}
