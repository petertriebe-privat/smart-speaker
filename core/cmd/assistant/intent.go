package main

import (
	"context"
	"log"
	"strings"
	"time"
	"unicode"

	"echo-assistant/internal/openai"
	"echo-assistant/internal/prompts"
)

// The decision tree after the wake word or the button (capture.go): every first utterance the
// rules do not know ("Pause sollst du machen", "Setze die YouTube-Wiedergabe fort") goes to
// OpenAI's Decisions API (#59): one choice question with fixed answers, no text generated, and
// after the wake word in the same call whether the words were meant for the assistant at all. Only what
// is neither a command nor meant for someone else goes to GPT Live.
//
// Measured from the PC on 18.10.2026: 150-400 ms per answer (the first call ~1.9 s with the
// connection), against 0.9-1.7 s for a small model over the Responses API, which ran into 3 s
// on the Echo. The answer carries a confidence: below intentSure the device does not act.

const (
	intentTimeout = 2500 * time.Millisecond
	intentModel   = "gpt-6-luna"
	intentSure    = 0.35
)

// musicCues: word beginnings that make a short utterance worth asking about.
var musicCues = []string{"paus", "stop", "musik", "lied", "song", "titel", "track", "laut", "leis", "weiter",
	"nächst", "zurück", "spiel", "skip", "überspring", "playlist", "spotify", "video", "youtube", "fernseh", "lieblings"}

// fulfil is the speaker session's Unfulfilled hook: GPT Live announced an action and did not
// delegate it. A music, video or home command in the user's words is run here and GPT Live told so.
func (d *device) fulfil(user, recent, said string) string {
	cmd := parseMusicCommand(user)
	if cmd.action == "" && (hasMusicCue(recent) || hasMusicCue(said)) {
		// any length, with the conversation before and what GPT Live announced: "Hast du es
		// vergessen?" or "Nein, läuft nicht" alone was no command (18.10.2026)
		cmd = decide("Conversation:\n"+strings.TrimSpace(recent)+"\n\nThe user's last words: "+strings.TrimSpace(user)+
			"\nThe assistant's announcement: "+strings.TrimSpace(said),
			prompts.TextOr("QUICK_ANNOUNCED_INSTRUCTIONS", announcedQuestion))
		cmd.where = placeIn(user)
	}
	if cmd.action == "" || cmd.action == "time" || cmd.action == "date" || cmd.action == "home" && !homeCandidate(user) {
		// HA acts on whatever matches: "Nein, es ist immer noch an im Arbeitszimmer" went to it
		// as a command (21.10.2026). Only the user's words that are one go there.
		return ""
	}
	out := d.doMedia(cmd, user)
	if strings.HasPrefix(out, homeUnknown) {
		return "" // HA did nothing: the usual reminder to delegate
	}
	log.Printf("[Nudge] the device did it itself: %s -> %.160s", cmd.action, out)
	if strings.HasPrefix(out, "Error") {
		return "System note, not from the user: the device tried what you announced and it failed: " + out +
			" Tell the user in one short sentence. Do not delegate it."
	}
	return "System note, not from the user: the device itself has just done what you announced (" + out +
		"). Do not delegate it again. Confirm it to the user in a few words."
}

// hasMusicCue: a music, video or home word in the text.
func hasMusicCue(text string) bool {
	for _, w := range strings.FieldsFunc(strings.ToLower(foldAccents(text)), func(r rune) bool { return !unicode.IsLetter(r) && !unicode.IsDigit(r) }) {
		if hasPrefix(w, musicCues) || hasPrefix(w, homeCues) {
			return true
		}
	}
	return false
}

// placeIn: the room or speaker at the end of the text ("… im Wohnzimmer"), as the rules read it.
func placeIn(text string) string {
	words := strings.FieldsFunc(strings.ToLower(foldAccents(text)), func(r rune) bool { return !unicode.IsLetter(r) && !unicode.IsDigit(r) })
	var kept []string
	for _, w := range words {
		if !isFiller(w) {
			kept = append(kept, w)
		}
	}
	for j := len(kept) - 2; j >= 1; j-- {
		if kept[j] == "im" || kept[j] == "in" || kept[j] == "auf" {
			if where := strings.Join(withoutVerbs(kept[j+1:]), " "); where != "spotify" && where != "youtube" {
				return where
			}
			return ""
		}
	}
	return ""
}

// trailingVerbs: verbs at the end of a sentence, not part of the place after "im" ("… auf dem Echo
// im Arbeitszimmer fortsetzen" named a speaker "arbeitszimmer fortsetzen", 18.10.2026).
var trailingVerbs = map[string]bool{"fortsetzen": true, "weiterspielen": true, "spielen": true, "abspielen": true,
	"laufen": true, "wiedergeben": true, "weiter": true, "übertragen": true, "starten": true, "hören": true,
	"pausieren": true, "anhalten": true}

func withoutVerbs(words []string) []string {
	for len(words) > 1 && trailingVerbs[words[len(words)-1]] {
		words = words[:len(words)-1]
	}
	return words
}

// The answers: the device's commands and what each means for the model.
var intentChoices = []struct{ value, description string }{
	{"none", "Anything else: a question, a conversation, more than one command, or unsure."},
	{"pause", "Pause or stop the music."},
	{"resume", "Go on with the paused music (not a video)."},
	{"continue", "Play music without naming what: 'my music', 'my Spotify music', 'some music'."},
	{"liked", "Play their favourite songs (Lieblingsmusik, Lieblingslieder, Favoriten)."},
	{"play_named", "Play a named artist, album, playlist, song or genre."},
	{"next", "Next song."},
	{"previous", "Previous song."},
	{"louder", "Louder."},
	{"quieter", "Quieter."},
	{"video_pause", "Pause the video: any mention of video, YouTube, film or TV (Fernseher)."},
	{"video_play", "Go on with the video: any mention of video, YouTube, film or TV (Fernseher)."},
	{"move", "Move the music that plays to another speaker or Echo the user names ('auf dem Echo im Arbeitszimmer weiterspielen', 'übertrag die Musik ins Wohnzimmer')."},
	{"move_here", "Bring the music that plays elsewhere back to this speaker ('hol die Musik zu dir zurück', 'spiel hier weiter')."},
	// the device hands the user's words to Home Assistant's own sentence matching (home.go)
	{"home", "Switch or set one thing in the house: lights, lamps, plugs, blinds or shutters, fans, heating, a scene ('Licht in der Küche aus', 'Rollos runter'). Not the TV, not music."},
	// the device answers these itself (clock.go)
	{"time", "Ask only for the current time ('Wie spät ist es?', 'Wie viel Uhr haben wir?')."},
	{"date", "Ask only for today's date or weekday ('Welcher Tag ist heute?', 'Den Wievielten haben wir?')."},
}

const refereeQuestion = "Does the user's latest request need the voice assistant to act with a tool: control a " +
	"device, music or a video, send or print something, look up current or personal data (weather, calendar, " +
	"mail, the web), plan, remember or call? Small talk, thanks, opinions and questions answerable from general " +
	"knowledge or from what the assistant always knows (the current time, date and weekday) are no."

const announcedQuestion = "A voice assistant told the user it would do something and then did not do it. From the " +
	"conversation, the user's last words and the assistant's announcement (a German speech transcript), which of " +
	"these commands did the assistant announce? 'Starting it again' after the user said the video does not " +
	"play is video_play."

// intentTriggers: GPT Live's own Decisions trigger on the user's transcript (live.Hooks.Triggers):
// the referee, whether GPT Live talks without delegating while the user wanted something done.
// The command question it also asked until 28.10.2026 is gone: the device decides the first words
// before GPT Live connects (capture.go).
func intentTriggers() []any {
	return []any{map[string]any{
		"type": "decisions", "event_type": "session.input_transcript.delta",
		"decisions": map[string]any{"questions": []map[string]any{
			{"type": "predicate", "name": "needs_action", "instructions": prompts.TextOr("REFEREE_QUESTION", refereeQuestion)},
		}},
	}}
}

// After the wake word the Decisions call also asks whether the words were meant for the assistant
// at all (QUICK_ADDRESSED_QUESTION in shared/prompts/prompts.h, with how its wording was chosen):
// most false starts heard the TV or people in the room, and their words reach the transcription
// too ("Jetzt kommt rein, setzt euch hin!", "Firetruck.", 26.10.2026).
// Below FIRST_WORDS_ADDRESSED_NOT (config.h, both devices) a start by the wake word ends silently.

// classifyIntent asks about a first utterance; action "" = not a command (or no answer in time).
// addressed: the probability that the words were meant for the assistant (-1: not asked or no
// answer).
func classifyIntent(text string, askAddressed bool) (cmd musicCommand, addressed float64) {
	extra := ""
	if askAddressed {
		extra = prompts.Text("QUICK_ADDRESSED_QUESTION")
	}
	cmd, addressed = decideWith(text, prompts.Text("QUICK_INTENT_INSTRUCTIONS"), extra)
	cmd.where = placeIn(text)
	return cmd, addressed
}

// decide asks the Decisions API which command the text is.
func decide(text, question string) musicCommand {
	cmd, _ := decideWith(text, question, "")
	return cmd
}

// decideWith: decide, and with addressed != "" also that predicate in the same call (its
// probability, -1 without it).
func decideWith(text, question, addressed string) (musicCommand, float64) {
	ctx, cancel := context.WithTimeout(context.Background(), intentTimeout)
	defer cancel()
	choices := make([]map[string]string, len(intentChoices))
	for i, c := range intentChoices {
		choices[i] = map[string]string{"value": c.value, "description": c.description}
	}
	started := time.Now()
	var resp struct {
		Answers []struct {
			Type        string   `json:"type"`
			Name        string   `json:"name"`
			Choice      string   `json:"choice"`
			Confidence  float64  `json:"confidence"`
			Probability *float64 `json:"probability"`
		} `json:"answers"`
	}
	questions := []map[string]any{{"type": "choice", "name": "command", "instructions": question, "choices": choices}}
	if addressed != "" {
		questions = append(questions, map[string]any{"type": "predicate", "name": "addressed", "instructions": addressed})
	}
	err := openai.Post(ctx, "/v1/decisions", map[string]any{
		"model":     prompts.TextOr("QUICK_INTENT_MODEL", intentModel),
		"input":     text,
		"questions": questions,
	}, &resp)
	ms := time.Since(started).Milliseconds()
	if err != nil || len(resp.Answers) == 0 {
		log.Printf("[Quick] intent of %q: no answer (%v, %d ms)", text, err, ms)
		return musicCommand{}, -1
	}
	cmd, prob := musicCommand{}, -1.0
	for _, a := range resp.Answers {
		switch {
		case a.Type == "choice":
			log.Printf("[Quick] intent of %q: %s (%.2f, %d ms)", text, a.Choice, a.Confidence, ms)
			cmd = commandOf(a.Choice, a.Confidence)
		case a.Type == "predicate" && a.Name == "addressed" && a.Probability != nil:
			prob = *a.Probability
			log.Printf("[Quick] meant for the assistant: %.2f", prob)
		}
	}
	return cmd, prob
}

// commandOf: the device's command for a Decisions answer (sure -1: no confidence given).
func commandOf(choice string, sure float64) musicCommand {
	if sure < 0 {
		sure = 1 // GPT Live's trigger results carry none
	}
	if choice == "play_named" && sure < 0.5 {
		// "Hierlaen Musik" was a mishearing of "Spiel meine Musik", not a title (18.10.2026):
		// unsure what to play means go on with what played last
		return musicCommand{action: "continue"}
	}
	if sure < intentSure {
		return musicCommand{}
	}
	switch choice {
	case "pause", "resume", "continue", "liked", "next", "previous", "video_pause", "video_play", "move", "move_here", "home",
		"time", "date":
		return musicCommand{action: choice}
	case "play_named":
		return musicCommand{action: "play"} // the backend searches it in the user's words
	case "louder":
		return musicCommand{action: "volume", step: 10}
	case "quieter":
		return musicCommand{action: "volume", step: -10}
	}
	return musicCommand{}
}
