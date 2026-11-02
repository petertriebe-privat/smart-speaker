package main

import (
	"context"
	"encoding/binary"
	"log"
	"strings"
	"sync"
	"time"

	"echo-assistant/internal/agent"
	"echo-assistant/internal/audio"
	"echo-assistant/internal/chatcore"
	"echo-assistant/internal/live"
	"echo-assistant/internal/openai"
	"echo-assistant/internal/prompts"
	"echo-assistant/internal/tools"
)

// The Fire TV's voice turns (voice model "text", the TV's default, #108): push to talk needs no
// live voice model. The key's audio is transcribed, the backend answers with every tool (its
// thread kept for follow-up questions), the answer is spoken (text to speech) and shown with the
// question under the screen; what is worth seeing the backend shows itself (show_on_screen). A
// live voice model only repeated the backend's result (realtime: ~10 000 tokens per answer, the
// beginning of the question lost to the "Ja?" clip, 29.10.2026). After a pause the conversation
// goes to the memory as a speaker conversation does.

const (
	tvTurnModel   = "text"          // the voice model setting for this mode
	tvTurnMinimum = 400             // ms: shorter is a slip of the key
	tvTurnAnswer  = 1200            // characters spoken at most
	tvTurnIdle    = 3 * time.Minute // then the conversation is over (memory, new thread)
)

type tvTurns struct {
	mu      sync.Mutex
	pcm     []int16 // the held key's audio
	thread  string  // the backend's conversation (previous_response_id)
	started time.Time
	words   strings.Builder // "Du: …\nJarvis: …" for the memory
	gen     int             // the current turn; an older one's answer is dropped
	idle    *time.Timer
}

// textTurns: this device talks in turns (the Fire TV with voice model "text").
func (d *device) textTurns() bool { return d.realtimeModel() == tvTurnModel }

// turnKey: the remote's key in text mode (pushToTalk). Pressed: a new turn, the assistant stops talking;
// let go: the turn runs.
func (d *device) turnKey(held bool) {
	t := &d.tvTurn
	t.mu.Lock()
	defer t.mu.Unlock()
	if held {
		t.pcm = t.pcm[:0]
		t.gen++
		d.speaker.Stop() // pressed while he speaks: the user has the word
		return
	}
	pcm := append([]int16(nil), t.pcm...)
	gen := t.gen
	go d.runTurn(pcm, gen)
}

// turnHear: the held key's audio (pushToTalk, under hearMu).
func (d *device) turnHear(pcm []int16) {
	t := &d.tvTurn
	t.mu.Lock()
	t.pcm = append(t.pcm, pcm...)
	t.mu.Unlock()
}

func (d *device) runTurn(pcm []int16, gen int) {
	t := &d.tvTurn
	current := func() bool {
		t.mu.Lock()
		defer t.mu.Unlock()
		return t.gen == gen
	}
	if len(pcm)*1000/audio.MicRate < tvTurnMinimum {
		d.screenState("speaker", live.Idle)
		return
	}
	started := time.Now()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	question, err := openai.TranscribeHint(ctx, wav(pcm, audio.MicRate), "turn.wav", prompts.Text("QUICK_TRANSCRIBE_HINT"))
	cancel()
	question = strings.TrimSpace(question)
	if err != nil || question == "" {
		log.Printf("[Turn] nothing understood (%v)", err)
		d.sayTurn(gen, "Verzeihung, das habe ich nicht verstanden.")
		return
	}
	log.Printf("[Du] %s (%.1f s audio, %d ms)", question, float64(len(pcm))/audio.MicRate, time.Since(started).Milliseconds())
	d.screenWords("speaker", true, question)
	if !current() {
		return
	}
	t.mu.Lock()
	if t.words.Len() == 0 {
		t.started = time.Now()
	}
	thread := t.thread
	t.mu.Unlock()
	req := live.Request{}
	answer := d.svc.agent.Run(question, agent.Options{
		Instructions: d.backendInstructions(req) + prompts.Text("TV_TURN_INSTRUCTIONS"),
		Thread:       &thread, Origin: "Fernseher", Effort: "low", Channel: tools.Voice})
	answer = chatcore.Spoken(chatcore.StripSources(answer), tvTurnAnswer)
	log.Printf("[Assistant] %s (%.1f s)", answer, time.Since(started).Seconds())
	t.mu.Lock()
	t.thread = thread
	t.words.WriteString("Du: " + question + "\n" + prompts.Name() + ": " + answer + "\n")
	if t.idle != nil {
		t.idle.Stop()
	}
	t.idle = time.AfterFunc(tvTurnIdle, d.endTurns)
	t.mu.Unlock()
	if current() {
		d.sayTurn(gen, answer)
	}
}

// sayTurn speaks and shows the assistant's words, unless the user pressed the key again meanwhile.
func (d *device) sayTurn(gen int, text string) {
	d.screenWords("speaker", false, text)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	data, err := openai.Speech(ctx, text, "pcm")
	cancel()
	t := &d.tvTurn
	t.mu.Lock()
	stale := t.gen != gen
	t.mu.Unlock()
	if err != nil || stale {
		if err != nil {
			log.Printf("[Turn] speech: %v", err)
		}
		d.screenState("speaker", live.Idle)
		return
	}
	pcm := make([]int16, len(data)/2)
	for i := range pcm {
		pcm[i] = int16(binary.LittleEndian.Uint16(data[2*i:]))
	}
	d.speaker.Queue(pcm)
	// idle once he has finished (or the next press stopped him)
	for d.speaker.Speaking(0) {
		time.Sleep(200 * time.Millisecond)
	}
	t.mu.Lock()
	stale = t.gen != gen
	t.mu.Unlock()
	if !stale {
		d.screenState("speaker", live.Idle)
	}
}

// endTurns: the pause after a conversation: it goes to the memory, the next question starts a
// new thread.
func (d *device) endTurns() {
	t := &d.tvTurn
	t.mu.Lock()
	transcript, started := t.words.String(), t.started
	t.words.Reset()
	t.thread = ""
	t.mu.Unlock()
	if transcript == "" {
		return
	}
	log.Printf("[Turn] conversation over (%d chars): to the memory", len(transcript))
	d.onSessionEnd(live.Ended{Request: live.Request{}, Started: started, Transcript: transcript, HeardUser: true})
}
