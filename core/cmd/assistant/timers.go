package main

import (
	"log"
	"strings"
	"time"

	"echo-assistant/internal/prompts"
	"echo-assistant/internal/timers"
)

// Timers and alarms on this Echo (#25, internal/timers): "Timer zehn Minuten" is a quick command
// run on the device; due timers ring here until the action button, the wake word or "Timer aus".

func (d *device) setupTimers() {
	d.timers = timers.New()
	d.timers.Ring = d.ringTimer
	d.timers.AddTools(d.registry)
	go d.timers.Run()
}

// ringTimer plays the alarm until it is stopped or TIMER_RING_MAX_S passed. A label is said once
// after the first beeps ("Der Timer für Nudeln ist abgelaufen."). Music is hushed meanwhile.
func (d *device) ringTimer(t timers.Timer) {
	d.haFire("timer_ringing", map[string]any{"label": t.Label, "kind": t.Kind})
	if d.ringingTimer.Swap(true) {
		return // already ringing: the loop goes on for both
	}
	go func() {
		defer d.ringingTimer.Store(false)
		limit := time.Duration(prompts.Int("TIMER_RING_MAX_S", 300)) * time.Second
		started := time.Now()
		said := false
		for d.timers.Ringing() {
			if time.Since(started) > limit {
				rang := d.timers.Stop()
				log.Printf("[Timer] stopped by itself after %v (%d)", limit, len(rang))
				return
			}
			if d.music != nil {
				d.music.HushFor(2 * time.Second)
			}
			for i := 0; i < 3; i++ {
				d.speaker.Tone(1319, 120, 0.25)
				d.speaker.Tone(0, 80, 0)
			}
			if !said {
				said = true
				if text := ringText(t); text != "" {
					go d.sayLocal(text)
				}
			}
			time.Sleep(1500 * time.Millisecond)
		}
	}()
}

func ringText(t timers.Timer) string {
	switch {
	case t.Label != "":
		return "Der Timer für " + t.Label + " ist abgelaufen."
	case t.Kind == "alarm":
		return "Guten Morgen. Es ist " + t.At.Format("15:04") + " Uhr."
	}
	return ""
}

// stopTimer ends a ringing timer; true if one rang.
func (d *device) stopTimer(by string) bool {
	if d.timers == nil || !d.timers.Ringing() {
		return false
	}
	rang := d.timers.Stop()
	d.speaker.Flush()
	log.Printf("[Timer] stopped by %s (%d)", by, len(rang))
	return true
}

// sayLocal speaks a short German sentence without GPT Live (TTS, ~1 s).
func (d *device) sayLocal(text string) {
	pcm, err := speech24k(text)
	if err != nil {
		log.Printf("[Timer] speech: %v", err)
		d.speaker.Tone(880, 120, 0.15)
		return
	}
	d.speaker.Queue(pcm)
}

// quickTimer: the utterance is a timer command; run at once and the conversation ended.
func (d *device) quickTimer(text string, settled bool) bool {
	if d.timers == nil || !complete(text, settled) {
		return false
	}
	cmd := timers.Parse(text)
	if cmd.Action == "" || !d.claimQuick() {
		return cmd.Action != ""
	}
	d.endQuick(text)
	var answer string
	switch cmd.Action {
	case "cancel":
		stopped := d.stopTimer("voice")
		n := d.timers.Cancel(0)
		switch {
		case n == 1:
			answer = "Timer gelöscht."
		case n > 1:
			answer = "Alle Timer gelöscht."
		case stopped:
			answer = ""
		default:
			answer = "Es läuft kein Timer."
		}
	case "alarm":
		t := d.timers.Add("alarm", 0, cmd.Hour, cmd.Minute, cmd.Label)
		answer = timers.Confirmation(t, time.Now())
	default:
		t := d.timers.Add("timer", cmd.After, 0, 0, cmd.Label)
		answer = timers.Confirmation(t, time.Now())
	}
	log.Printf("[Quick] timer %q: %s", text, strings.TrimSpace(answer))
	if answer != "" {
		go d.sayLocal(answer)
	}
	return true
}
