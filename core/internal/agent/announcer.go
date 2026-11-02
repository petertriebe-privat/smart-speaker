package agent

import (
	"context"
	"encoding/binary"
	"log"
	"sync"
	"time"

	"echo-assistant/internal/openai"
	"echo-assistant/internal/prompts"
	"echo-assistant/internal/tools"
)

// Announcer: announcements of the agent (its speak tool), as devices/atoms3r/src/voice/announcer.cpp:
// played between conversations; a conversation that starts meanwhile cuts it off.
type Announcer struct {
	Play  func(pcm []int16) // queues 24 kHz audio on the speaker
	Flush func()            // drops what the speaker still has queued
	Tone  func()            // the short beep before an announcement
	Busy  func() bool       // a conversation is open
	// Route: speak with a room: another Echo says it (several Echos); false = it means this one.
	Route    func(room, text string) (string, bool)
	mu       sync.Mutex
	queue    [][]int16
	playing  bool
	stopping bool
}

// Speak: text to speech in the assistant's voice, played once no conversation is open.
func (a *Announcer) Speak(text string) string {
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	raw, err := openai.Speech(ctx, text, "pcm")
	if err != nil {
		return "Error: " + err.Error()
	}
	pcm := make([]int16, len(raw)/2)
	for i := range pcm {
		pcm[i] = int16(binary.LittleEndian.Uint16(raw[i*2:]))
	}
	if limit := prompts.Int("SPEECH_MAX_SECONDS", 90) * 24000; len(pcm) > limit {
		pcm = pcm[:limit]
	}
	a.mu.Lock()
	a.queue = append(a.queue, pcm)
	start := !a.playing
	a.playing = true
	a.mu.Unlock()
	log.Printf("[Agent] announcement %.1f s", float64(len(pcm))/24000)
	if start {
		go a.run()
	}
	return "Being announced on the speaker now."
}

// SpeakIn: Speak on the assistant speaker in room (” = this one; 'all' = every one).
func (a *Announcer) SpeakIn(room, text string) string {
	if room != "" && a.Route != nil {
		if out, routed := a.Route(room, text); routed {
			return out
		}
	}
	return a.Speak(text)
}

func (a *Announcer) run() {
	for {
		a.mu.Lock()
		if len(a.queue) == 0 || a.stopping {
			a.queue, a.playing, a.stopping = nil, false, false
			a.mu.Unlock()
			return
		}
		a.mu.Unlock()
		if a.Busy() {
			time.Sleep(time.Second)
			continue
		}
		a.mu.Lock()
		pcm := a.queue[0]
		a.queue = a.queue[1:]
		a.mu.Unlock()
		a.Tone()
		// Fed in slices, so a conversation that starts can cut it off.
		for i := 0; i < len(pcm); i += 24000 / 5 {
			if a.Busy() || a.Stopped() {
				a.Flush()
				break
			}
			a.Play(pcm[i:min(i+24000/5, len(pcm))])
		}
	}
}

func (a *Announcer) Stopped() bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.stopping
}

// Active: an announcement is waiting or playing.
func (a *Announcer) Active() bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.playing
}

// Stop: the button during an announcement: silence at once, the waiting ones dropped too.
func (a *Announcer) Stop() {
	a.mu.Lock()
	waiting := len(a.queue)
	if a.playing {
		a.stopping = true
	}
	a.mu.Unlock()
	a.Flush()
	log.Printf("[Announcer] stopped by the user (%d more dropped)", waiting)
}

// AddTools registers speak (agent runs only: nobody is at the speaker in a conversation's
// backend).
func (a *Announcer) AddTools(r *tools.Registry) {
	r.Add(tools.Tool{
		Name: "speak",
		// The M5 offered it to agent runs only (its one conversation was at the speaker). On the
		// Echo a phone call runs next to the speaker: from there it is the way to say something
		// at home (16.10.2026 Home Assistant's broadcast reported success and nothing was heard).
		Description: "Says a text out loud on the speaker at home, the one the assistant itself runs on (announcements, reading out mails or answers; " +
			"from a phone call: 'erzähl das über den Lautsprecher'). Write it as natural spoken German, without " +
			"formatting. Only when the user asked for it or your instructions allow it: you do not know which room they are in, so results and " +
			"reminders nobody asked to hear go to their phone as a message. Not for Echos of Amazon or other speakers " +
			"(alexa) and not while you talk at the speaker yourself. " +
			"room: another of the assistant's own speakers by its room or name ('in der Küche'), 'all' for every one of them " +
			"('Durchsage überall'), '' for this one.",
		Parameters: `{"type": "object", "additionalProperties": false,
            "properties": {"text": {"type": "string"},
                           "room": {"type": "string", "description": "optional: room or name of another assistant speaker, 'all', '' = here"}},
            "required": ["text"]}`,
		Channels: tools.Agent | tools.Voice | tools.Console, // console: ctl tool speak (tests)
		Run: func(args map[string]any, ctx tools.Context) string {
			text, room := tools.String(args, "text"), tools.String(args, "room")
			if room != "" && a.Route != nil {
				if out, routed := a.Route(room, text); routed {
					return out
				}
			}
			if ctx.Conversation != nil && ctx.Conversation.Label() == "speaker" {
				return "Error: you are talking at the speaker yourself; just say it."
			}
			return a.Speak(text) // Route said: this one
		},
	})
}
