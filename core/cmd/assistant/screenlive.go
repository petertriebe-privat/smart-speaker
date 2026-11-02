package main

import (
	"echo-assistant/internal/prompts"
	"strings"
	"sync"
	"time"

	"echo-assistant/internal/assistant"
	"echo-assistant/internal/board"
	"echo-assistant/internal/live"
)

// The assistant on the TV in its room (#108): the speaker conversation's state and words go to the
// screen there as a status card – the hologram listens, thinks, speaks, the captions below. The
// TV app decides where it appears (the console, the corner, not over a film unless the
// conversation is held through that TV). On the Fire TV's own assistant the room's TV is itself.

// backendInstructions: the backend's instructions, and while the TV in this room is on (or this is
// the Fire TV itself) the rule to show what the conversation is about: the user sees it there.
func (d *device) backendInstructions(req live.Request) string {
	text := assistant.BackendInstructions(req)
	if req.Phone || !(board.FireTV || d.svc.screens.On(d.cfg.Settings.Room)) {
		return text
	}
	// The phone's app reports its screen on only while the assistant's console is open in front (phone/).
	where := "# The TV in this room\nThe user sees the TV here: it is on."
	if board.Phone {
		where = "# The phone's screen\nThe user has the assistant's app open on the phone and sees its screen."
	}
	return text + "\n\n" + where + " Decide yourself what is worth " +
		"seeing and show it with show_on_screen (room '', mode auto), without being asked: a card with the " +
		"kind and blocks that fit (the weather as a forecast, appointments as a timeline, lists, recipe " +
		"steps, a route, a comparison as a table, values as stats or a chart, choices to answer by number), " +
		"a picture of what you talk about (a person, a place, an animal, a thing: an image link from the web " +
		"search, image block or image_url), a text to read (a poem, a message, a summary), a web page when " +
		"the user wants to read or look at it (url), a video file (kind video). Several things one after " +
		"the other as the conversation goes on. Keep the spoken answer short: the screen holds the " +
		"details. Pick the template of show_on_screen that fits. " + screenHow() + "\n"
}

// screenHow: on the TV itself every answer with content is shown, it is what the user looks at;
// at an Echo with the TV on only what is worth seeing.
func screenHow() string {
	if board.Phone {
		return "Here on the phone show every answer that has content (appointments, weather, lists, routes, " +
			"facts with numbers), graphically where you can; a new card replaces the last one. Only greetings " +
			"and small talk go without a card."
	}
	if board.FireTV {
		return "Here on the TV show every answer that has content, graphically where you can (a forecast, " +
			"a chart, a table, stats, a picture rather than a plain text); only greetings and small talk " +
			"go without a card."
	}
	return "Not for small talk or a single short fact."
}

// screenHint: a message the assistant sends a person (people.Message: reminders, results, hints) also
// as a small card on every TV that is on – in the corner, above a film, gone after a while. None
// is woken, none in night mode. Long texts are cut: the whole text is on the phone.
func (d *device) screenHint(name, text string) {
	if d.svc.screens == nil || d.night.Load() {
		return
	}
	text = strings.TrimSpace(text)
	if r := []rune(text); len(r) > 280 {
		text = strings.TrimSpace(string(r[:280])) + " … (ganz auf dem Handy)"
	}
	d.svc.screens.Live("", map[string]any{"mode": "overlay", "kind": "message", "notify": true,
		"title": prompts.Name(), "eyebrow": "für " + name, "text": text, "seconds": 20})
}

// screenCaptionEvery: captions follow the words at most this often (one MQTT message each).
const screenCaptionEvery = 300 * time.Millisecond

type screenLive struct {
	mu        sync.Mutex
	state     string
	user      string
	assistant string
	sent      time.Time
	pending   *time.Timer
	thinking  *time.Timer
	watching  bool // screenSpoken runs
}

func (d *device) screenCard(seconds int) map[string]any {
	l := &d.screenLive
	card := map[string]any{"kind": "status", "name": prompts.Name(), "state": l.state, "user": l.user, "assistant": l.assistant,
		"here": board.Android}
	if seconds > 0 {
		card["seconds"] = seconds
	}
	return card
}

// screenSend: The assistant's state and words to the TV in its room; on the phone straight to its app
// (phoneboard.go): over the broker in the data centre the captions lagged behind the words.
func (d *device) screenSend(card map[string]any) {
	if board.Phone && d.phoneCard(card) {
		return
	}
	d.svc.screens.Live(d.cfg.Settings.Room, card)
}

// captionEvery: captions follow the words at most this often; the phone's app is local.
func captionEvery() time.Duration {
	if board.Phone {
		return 100 * time.Millisecond
	}
	return screenCaptionEvery
}

// screenPublishLocked sends now, or once the caption interval has passed.
func (d *device) screenPublishLocked(seconds int) {
	l := &d.screenLive
	if d.svc.screens == nil {
		return
	}
	if l.pending != nil {
		l.pending.Stop()
		l.pending = nil
	}
	wait := captionEvery() - time.Since(l.sent)
	if wait <= 0 || seconds > 0 {
		l.sent = time.Now()
		d.screenSend(d.screenCard(seconds))
		return
	}
	l.pending = time.AfterFunc(wait, func() {
		l.mu.Lock()
		defer l.mu.Unlock()
		l.pending = nil
		l.sent = time.Now()
		d.screenSend(d.screenCard(0))
	})
}

// screenState: the speaker session's state (onSessionState).
func (d *device) screenState(label string, st live.State) {
	if label != "speaker" {
		return
	}
	l := &d.screenLive
	l.mu.Lock()
	defer l.mu.Unlock()
	switch st {
	case live.Connecting, live.Starting:
		l.state, l.user, l.assistant = "listening", "", ""
		d.screenPublishLocked(0)
	case live.Idle:
		if l.thinking != nil {
			l.thinking.Stop()
		}
		l.state = "idle"
		d.screenPublishLocked(4) // the last words stay a moment, then the corner clears
	}
}

// screenKey: the Fire TV remote's microphone key (tvboard.go). Pressed: The assistant listens at once,
// a new turn (the last words give way); let go: he thinks until his answer comes.
func (d *device) screenKey(held bool) {
	l := &d.screenLive
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.thinking != nil {
		l.thinking.Stop()
	}
	if held {
		l.state, l.user, l.assistant = "listening", "", ""
	} else {
		l.state = "thinking"
	}
	// now, not after the caption interval
	if l.pending != nil {
		l.pending.Stop()
		l.pending = nil
	}
	l.sent = time.Now()
	if d.svc.screens != nil {
		d.screenSend(d.screenCard(0))
	}
}

// screenWords: a line of the conversation as it grows (live.Hooks.OnWords).
func (d *device) screenWords(label string, user bool, line string) {
	if label != "speaker" {
		return
	}
	l := &d.screenLive
	l.mu.Lock()
	defer l.mu.Unlock()
	line = strings.TrimSpace(line)
	if user {
		if l.state == "thinking" {
			// the transcript of a push-to-talk turn comes after the key: he thinks already
			l.user = line
			d.screenPublishLocked(0)
			return
		}
		if l.state != "listening" {
			l.assistant = "" // a new turn: the last answer gives way
		}
		l.state, l.user = "listening", line
		// No answer soon after the user's words: The assistant is thinking (tools, the backend).
		if l.thinking != nil {
			l.thinking.Stop()
		}
		l.thinking = time.AfterFunc(1200*time.Millisecond, func() {
			l.mu.Lock()
			defer l.mu.Unlock()
			if l.state == "listening" {
				l.state = "thinking"
				d.screenPublishLocked(0)
			}
		})
	} else {
		if l.thinking != nil {
			l.thinking.Stop()
		}
		l.state, l.assistant = "speaking", line
		if !l.watching {
			l.watching = true
			go d.screenSpoken()
		}
	}
	d.screenPublishLocked(0)
}

// screenSpoken: once the assistant has said its answer, it is ready again (the session stays open for the
// next press of the key; without this the screen kept "speaking" or "listening" for minutes).
func (d *device) screenSpoken() {
	l := &d.screenLive
	quiet := 0
	for range time.Tick(250 * time.Millisecond) {
		speaking := d.speaker.Speaking(0)
		working := d.session.Working() // asked before l.mu: the session's lock never inside it
		l.mu.Lock()
		if l.state != "speaking" {
			l.watching = false
			l.mu.Unlock()
			return
		}
		if speaking {
			quiet = 0
		} else if quiet++; quiet >= 4 { // a second of silence: the answer is over
			// … or only its first words ("Einen Moment, ich schaue nach") while the backend
			// still works: then he thinks. The phone showed "Bereit" for two minutes of a
			// search, as if the assistant were dead (01.11.2026).
			l.state, l.watching = "idle", false
			if working {
				l.state = "thinking"
			}
			d.screenPublishLocked(0)
			l.mu.Unlock()
			return
		}
		l.mu.Unlock()
	}
}
