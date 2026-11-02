package main

import (
	"context"
	"echo-assistant/internal/prompts"
	"fmt"
	"log"
	"strings"
	"time"
	"unicode"

	"echo-assistant/internal/homeassistant"
)

// Quick home commands (#75): "Jarvis, Licht in der Küche aus" should take a moment, as the music
// commands do. The rules for them are Home Assistant's own sentence matching (Assist, in the
// LAN), which knows the house's areas, names and aliases: the device only decides that a short
// first utterance is meant for the house (a home word, or the intent model's "home") and hands
// the user's words to HA. What HA does not understand goes on to GPT Live, which has HA's tools
// and can look the device up.

const homeTimeout = 3 * time.Second // HA answers in the LAN within ~0.3 s

// homeUnknown starts doMedia's result when HA did not understand: not an error to explain, the
// request goes on to GPT Live.
const homeUnknown = "Error: Home Assistant did not understand: "

// homeCues: word beginnings for what HA switches. Not the TV ("Fernseher aus" is fire_tv's) nor
// "hell"/"dunkel" ("Wann ist es dunkel heute?").
var homeCues = []string{"licht", "lamp", "leucht", "beleucht", "rollo", "rolllad", "rollad", "jalousi",
	"markis", "heizung", "thermostat", "steckdos", "ventilat", "lüfter", "szene", "dimm", "hellig"}

// homeParticles: the separable part of German switching verbs; HA wants a room before it
// ("Schalte das Licht im Wohnzimmer aus"), "… aus im Wohnzimmer" was no_valid_targets (22.10.2026).
var homeParticles = map[string]bool{"an": true, "aus": true, "ein": true, "ab": true, "auf": true, "zu": true,
	"hoch": true, "runter": true}

// notRooms: what "<word> an/aus" means that is not a room's light (the TV is fire_tv's, music
// the music commands').
var notRooms = map[string]bool{"fernseher": true, "fernsehen": true, "tv": true, "musik": true, "radio": true,
	"video": true, "youtube": true, "netflix": true, "alexa": true, "alles": true}

// bareRoom: "Küche aus" - a room and nothing but on/off. HA has no sentence for it ("Küche
// existiert nicht"), "Licht Küche aus" works (22.10.2026).
func bareRoom(words string) bool {
	w := strings.Fields(strings.ToLower(strings.Trim(words, ".,!? ")))
	if len(w) != 2 || notRooms[w[0]] || isFiller(w[0]) || hasPrefix(w[0], homeCues) {
		return false
	}
	return w[1] == "an" || w[1] == "aus" || w[1] == "ein"
}

// withRoom puts this speaker's room into the words, before a closing particle.
func withRoom(words, area string) string {
	w := strings.Fields(words)
	if n := len(w); n > 1 && homeParticles[strings.ToLower(w[n-1])] {
		return strings.Join(w[:n-1], " ") + " im " + area + " " + w[n-1]
	}
	return words + " im " + area
}

// homeCandidate: short, no question, with a home word - or a bare room ("Küche aus").
func homeCandidate(text string) bool {
	t := strings.TrimSpace(text)
	if t == "" || strings.HasSuffix(t, "?") {
		return false
	}
	if bareRoom(homeWords(t)) {
		return true
	}
	kept := 0
	cue := false
	for _, w := range strings.FieldsFunc(strings.ToLower(foldAccents(t)), func(r rune) bool { return !unicode.IsLetter(r) && !unicode.IsDigit(r) }) {
		if isFiller(w) {
			continue
		}
		kept++
		cue = cue || hasPrefix(w, homeCues)
	}
	return cue && kept <= 10
}

func hasPrefix(word string, prefixes []string) bool {
	for _, p := range prefixes {
		if strings.HasPrefix(word, p) {
			return true
		}
	}
	return false
}

// homeWords: the user's words for HA, without the wake word and the assistant's own "Ja, Sir" (HA's
// sentences do not allow them) and without "dir"/"mir" ("Schalt dir das Licht in der Küche aus"
// came from the transcription and HA failed on it, 22.10.2026).
func homeWords(text string) string {
	words := strings.Fields(text)
	for len(words) > 0 {
		w := strings.ToLower(strings.Trim(words[0], ".,!?;:"))
		if w != "hey" && !prompts.IsName(w) && w != "ja" && w != "sir" && w != "okay" && w != "ok" {
			break
		}
		words = words[1:]
	}
	kept := words[:0]
	for _, w := range words {
		if l := strings.ToLower(strings.Trim(w, ".,!?;:")); l != "dir" && l != "mir" {
			kept = append(kept, w)
		}
	}
	return strings.TrimRight(strings.Join(kept, " "), ".!")
}

// namesPlace: the words name a room ("… in der Küche", "im Flur", "ins Bad").
func namesPlace(text string) bool {
	for _, w := range strings.FieldsFunc(strings.ToLower(text), func(r rune) bool { return !unicode.IsLetter(r) }) {
		if w == "im" || w == "in" || w == "ins" {
			return true
		}
	}
	return false
}

// homeCommand hands a home command to HA and returns the result for doMedia. HA takes the room
// of the satellite that asks when none is named; asked over its API there is none, and "Licht
// aus" came back as no_valid_targets (21.10.2026). So without a room named this speaker's own
// area goes first (withRoom), then the words alone (a device named in another room:
// "Schreibtischlampe an"). A bare room ("Küche aus") goes as that room's light.
func (d *device) homeCommand(text string) string {
	ha := d.svc.ha
	if ha == nil {
		return homeUnknown + "no Home Assistant configured"
	}
	ctx, cancel := context.WithTimeout(context.Background(), homeTimeout)
	defer cancel()
	words := homeWords(text)
	tries := []string{words}
	area := d.cfg.Settings.HAArea
	if area == "" {
		area = d.cfg.Settings.Room
	}
	switch {
	case bareRoom(words):
		tries = []string{"Licht " + words}
	case area != "" && !namesPlace(words):
		tries = []string{withRoom(words, area), words}
	}
	var r homeassistant.AssistResult
	for _, words = range tries {
		started := time.Now()
		var err error
		r, err = ha.Assist(ctx, words)
		ms := time.Since(started).Milliseconds()
		if err != nil {
			log.Printf("[Quick] home %q: %v (%d ms)", words, err, ms)
			return homeUnknown + err.Error()
		}
		log.Printf("[Quick] home %q: %s %s %q, %d done, %d failed (%d ms)", words, r.Type, r.Code, r.Speech, r.Done, r.Failed, ms)
		if r.Type != "error" {
			break
		}
	}
	switch {
	case r.Type == "action_done" && r.Failed > 0:
		return fmt.Sprintf("Error: Home Assistant: %s (%d of %d devices failed)", r.Speech, r.Failed, r.Failed+r.Done)
	case r.Type == "action_done":
		return "Home Assistant: " + r.Speech
	}
	// an error (no match, no such device) or a question's answer: GPT Live takes it
	return homeUnknown + r.Type + " " + r.Code
}
