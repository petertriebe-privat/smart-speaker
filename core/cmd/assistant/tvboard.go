package main

import (
	"log"
	"os"
	"os/signal"
	"slices"
	"syscall"
	"time"

	"echo-assistant/internal/agent"
	"echo-assistant/internal/audio"
	"echo-assistant/internal/board"
	"echo-assistant/internal/config"
	"echo-assistant/internal/errorlog"
	"echo-assistant/internal/hamqtt"
	"echo-assistant/internal/input"
	"echo-assistant/internal/live"
	"echo-assistant/internal/tools"
)

// The Fire TV (board.FireTV, devices/android/firetv): the app is microphone and speaker. The
// remote's microphone reaches the assistant only while its key is held, the app streams exact silence
// otherwise, so sound from it is the key: pressed when samples arrive, released after a short
// silence. Pressed is the Echo's action button (onInput), so everything the button does works
// here too (start a conversation, stop a ringing timer, answer a call).

// pttRelease: this long without a sample means the key was let go.
const pttRelease = 400 * time.Millisecond

// guardSeccomp: Android's seccomp filter answers a system call an app may not make (settimeofday,
// pidfd for a child process) with SIGSYS, which ends a Go program built for linux (the
// android target would need cgo). The assistant avoids those calls on the stick; should one slip
// through, it is logged instead of a crash loop.
func guardSeccomp() {
	if !board.Android {
		return
	}
	ch := make(chan os.Signal, 4)
	signal.Notify(ch, syscall.Signal(31)) // SIGSYS on Linux (no constant when vetted on Windows)
	go func() {
		for range ch {
			log.Print("[Assistant] SIGSYS: a system call Android forbids an app (see tvboard.go)")
			errorlog.Add("A system call Android forbids (SIGSYS)")
		}
	}()
}

// delegate: the backend of a Realtime session (live/realtime.go), as GPT Live's own delegation:
// the backend instructions, every voice tool, little reasoning (someone waits), the thread kept
// over the session so follow-ups know what was asked before.
func (d *device) delegate(req live.Request, task string, thread *string) string {
	if d.svc.agent == nil {
		return "Error: no backend"
	}
	return d.svc.agent.Run(task, agent.Options{Instructions: d.backendInstructions(req), Thread: thread,
		Person: req.Person, Origin: req.Origin(), Effort: "low", Channel: tools.Voice})
}

// The protocol of the conversation, chosen in Home Assistant on the Fire TV (for comparing; the
// next conversation takes it): GPT Live, billed by the open minute, or a Realtime model, billed
// by tokens (live/realtime.go).
var realtimeChoices = []string{"Sprache zu Text", "gpt-realtime-mini", "gpt-realtime-2.1", "GPT Live"}

// liveRealtime: the live session's Realtime model for a setting (turns need no live session).
func liveRealtime(model string) string {
	if model == tvTurnModel {
		return ""
	}
	return model
}

// voiceModelName: the select's name of a setting ("text" = speech to text, tvturn.go; "" = GPT Live).
func voiceModelName(model string) string {
	switch model {
	case tvTurnModel:
		return "Sprache zu Text"
	case "":
		return "GPT Live"
	}
	return model
}

func (d *device) realtimeEntity() hamqtt.Entity {
	return hamqtt.Entity{Component: "select", Key: "voice_model",
		State: func() string { return voiceModelName(d.realtimeModel()) },
		Command: func(name string) {
			if !slices.Contains(realtimeChoices, name) {
				return
			}
			model := name
			switch name {
			case "GPT Live":
				model = ""
			case "Sprache zu Text":
				model = tvTurnModel
			}
			d.realtimeChoice.Store(model)
			if model != tvTurnModel {
				d.session.SetRealtime(model) // the live session; turns need none
			}
			d.saveHA(func(v *config.HAValues) { v.RealtimeModel = &model })
			if d.ha != nil {
				d.ha.Changed()
			}
		},
		Config: map[string]any{"name": "Sprachmodell", "icon": "mdi:account-voice", "options": realtimeChoices,
			"entity_category": "config"}}
}

func (d *device) realtimeModel() string {
	if m, ok := d.realtimeChoice.Load().(string); ok {
		return m
	}
	return d.cfg.Settings.RealtimeModel
}

// tvSkipped: the Echo's entities in Home Assistant that the stick has not: no mute switch, no
// ring, no wake word, the TV's own volume, no conversation without the remote's key held, no
// Wi-Fi level (Android hides it from apps).
var tvSkipped = map[string]bool{"muted": true, "ring_brightness": true, "wake_sensitivity": true,
	"volume": true, "night_volume": true, "talk": true, "wifi": true}

// tvEntities drops them on the Fire TV and retires those an earlier version announced.
func tvEntities(id string, list []hamqtt.Entity) (kept []hamqtt.Entity, retired []string) {
	if !board.Android {
		return list, nil
	}
	for _, e := range list {
		if tvSkipped[e.Key] {
			retired = append(retired, e.Component+"/"+id+"/"+e.Key)
			continue
		}
		kept = append(kept, e)
	}
	return kept, retired
}

func (d *device) startTVAudio() {
	if !audio.OnTV() {
		log.Print("[Audio] Android app: JARVIS_AUDIO missing, no microphone or speaker")
		return
	}
	d.mic = audio.TVMic()
}

// pushToTalk runs in hear (under hearMu) on the Fire TV.
func (d *device) pushToTalk(pcm []int16) {
	if !board.FireTV {
		return
	}
	sound := false
	for _, v := range pcm {
		if v != 0 {
			sound = true
			break
		}
	}
	now := time.Now()
	if d.textTurns() { // speech to text, the backend, text to speech (tvturn.go)
		ringing := d.timers != nil && d.timers.Ringing()
		switch {
		case sound && !d.pttHeld:
			d.pttHeld = true
			log.Print("[Remote] microphone key")
			if ringing {
				d.onInput(input.Event{Kind: input.ActionDown}) // the key stops a ringing timer, nothing else
				break
			}
			d.turnKey(true)
			d.screenKey(true)
		case !sound && d.pttHeld && now.Sub(d.pttLast) > pttRelease:
			d.pttHeld = false
			if !ringing {
				d.turnKey(false)
				d.screenKey(false)
			}
		}
		if d.pttHeld {
			d.turnHear(pcm)
		}
		if sound {
			d.pttLast = now
		}
		return
	}
	switch {
	case sound && !d.pttHeld:
		d.pttHeld = true
		log.Print("[Remote] microphone key")
		d.session.PushToTalk(true) // Realtime: the user's turn; stops the assistant if it speaks
		d.screenKey(true)          // the screen: listening at once (screenlive.go)
		// A running conversation goes on (the button would end it); otherwise the key is the
		// action button: a conversation starts, a ringing timer stops.
		ringing := d.timers != nil && d.timers.Ringing()
		d.pttButton = ringing || !(d.listening() || d.session.Open())
		if d.pttButton {
			d.onInput(input.Event{Kind: input.ActionDown})
		}
	case !sound && d.pttHeld && now.Sub(d.pttLast) > pttRelease:
		d.pttHeld = false
		d.session.PushToTalk(false) // Realtime: the turn is over, the assistant answers
		d.screenKey(false)          // the screen: thinking
		if d.pttButton {
			d.onInput(input.Event{Kind: input.ActionUp})
		}
	}
	if sound {
		d.pttLast = now
	}
}
