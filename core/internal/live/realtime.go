package live

import (
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"log"
	"strings"
	"time"

	"github.com/coder/websocket"

	"echo-assistant/internal/prompts"
)

// Realtime (#108): a session may talk to OpenAI's Realtime API instead of GPT Live
// (Options.Realtime = the model, e.g. gpt-realtime-2.1 or gpt-realtime-mini). Made for push to
// talk (the Fire TV's remote): billed by tokens, not by the open minute, so silence costs nothing
// and the session may stay open for follow-up questions; turn based, the user's turn ends when
// the key is let go (PushToTalk), no server voice detection.
//
// The rest of the session keeps speaking GPT Live: this file translates both ways. GPT Live's
// delegation is done here: the realtime model has one tool, delegate, whose task runs the
// backend (Hooks.Delegate: the backend instructions and every tool, the thread kept over the
// session).

const realtimeURL = "wss://api.openai.com/v1/realtime"

var (
	// realtimeIdle: the session closes after this long without speech (the next press of the key
	// opens a new one); silence is free, the context of the conversation is not.
	realtimeIdle = ms("REALTIME_IDLE_MS", 180000)
)

func (s *Session) realtime() bool { return s.opts.Realtime != "" }

// realtimeNote tells the realtime model how it reaches its tools. Every turn of the user goes to
// the backend (rtCommit forces the call): gpt-realtime-mini announced a lookup on 29.10.2026 and
// never made it, and the backend also decides what the screen shows.
const realtimeNote = "\n\n# Your backend\nEach turn of the user goes to the function delegate first: give it " +
	"the complete task in German with everything the user said that matters (for small talk too: it " +
	"knows the user and shows things on the screen). Then speak its result in your words, short and " +
	"natural. Never answer facts yourself, never invent anything (weather, times, prices, news).\n"

func (s *Session) rtStart(req Request) {
	session := map[string]any{
		"type":              "realtime",
		"instructions":      s.hooks.Instructions(req) + realtimeNote,
		"output_modalities": []string{"audio"},
		"audio": map[string]any{
			"input": map[string]any{
				"format":         map[string]any{"type": "audio/pcm", "rate": 24000},
				"turn_detection": nil, // push to talk: the key ends the turn
				"transcription": map[string]any{"model": prompts.TextOr("REALTIME_TRANSCRIBE_MODEL", "gpt-4o-mini-transcribe"),
					"language": "de"},
			},
			"output": map[string]any{
				"format": map[string]any{"type": "audio/pcm", "rate": 24000},
				"voice":  prompts.TextOr("REALTIME_VOICE", "cedar"),
			},
		},
		"tools": []any{map[string]any{
			"type": "function", "name": "delegate",
			"description": "Hands a task to the assistant's backend, which has every tool (home, music, TV and screen, " +
				"calendar, mail, messages, people, memory, weather, maps, web search, timers). The result comes " +
				"back as text.",
			"parameters": map[string]any{"type": "object", "additionalProperties": false,
				"properties": map[string]any{"task": map[string]any{"type": "string",
					"description": "the complete task in German"}},
				"required": []string{"task"}},
		}},
		"tool_choice": "auto",
	}
	s.sendRT(map[string]any{"type": "session.update", "session": session})
}

// sendRT sends a Realtime event as it is (send would translate it).
func (s *Session) sendRT(event map[string]any) bool {
	data, err := json.Marshal(event)
	if err != nil {
		return false
	}
	return s.sendRaw(data)
}

// rtOut translates what the session sends in GPT Live's protocol; false = not handled here.
func (s *Session) rtOut(event any) bool {
	m, ok := event.(map[string]any)
	if !ok {
		return false
	}
	switch m["type"] {
	case "session.commentary.append":
		text, _ := m["content"].(string)
		s.rtSystem(text)
	case "session.close":
		s.rtClose()
	default:
		log.Printf("[Realtime] not translated: %v", m["type"])
	}
	return true
}

// rtSystem: a note for the model (a notice, a nudge); it answers at once unless the user is
// talking.
func (s *Session) rtSystem(text string) {
	if strings.TrimSpace(text) == "" {
		return
	}
	s.sendRT(map[string]any{"type": "conversation.item.create", "item": map[string]any{
		"type": "message", "role": "system",
		"content": []any{map[string]any{"type": "input_text", "text": text}}}})
	s.mu.Lock()
	talking := s.rtTurnOpen
	s.mu.Unlock()
	if !talking {
		s.sendRT(map[string]any{"type": "response.create"})
	}
}

// rtClose: no goodbye in the Realtime protocol: the connection closes, the session ends as after
// GPT Live's session.closed.
func (s *Session) rtClose() {
	s.mu.Lock()
	conn := s.conn
	s.mu.Unlock()
	if conn != nil {
		conn.Close(websocket.StatusNormalClosure, "")
	}
	go s.onEvent([]byte(`{"type":"session.closed","reason":"close_requested"}`))
}

// rtAudio: microphone audio (24 kHz) of the user's turn. Exact silence is the Fire TV app's
// filler while the remote's key is not held: it costs tokens and is dropped.
func (s *Session) rtAudio(pcm []int16) {
	silent := true
	for _, v := range pcm {
		if v != 0 {
			silent = false
			break
		}
	}
	if silent {
		return
	}
	raw := make([]byte, len(pcm)*2)
	for i, v := range pcm {
		binary.LittleEndian.PutUint16(raw[i*2:], uint16(v))
	}
	if s.sendRT(map[string]any{"type": "input_audio_buffer.append", "audio": base64.StdEncoding.EncodeToString(raw)}) {
		s.mu.Lock()
		s.rtHeard = true
		s.rtSamples += len(pcm)
		s.mu.Unlock()
	}
}

// rtCommit ends the user's turn: the audio sent so far is the question, the assistant answers.
func (s *Session) rtCommit() {
	s.mu.Lock()
	heard, samples := s.rtHeard, s.rtSamples
	s.rtSamples = 0
	s.rtHeard = false
	s.mu.Unlock()
	if !heard {
		return
	}
	log.Printf("[Realtime] turn ends: %.1f s of the user's audio", float64(samples)/24000)
	s.sendRT(map[string]any{"type": "input_audio_buffer.commit"})
	s.sendRT(map[string]any{"type": "response.create", "response": map[string]any{
		"tool_choice": map[string]any{"type": "function", "name": "delegate"}}})
}

// PushToTalk: the Fire TV remote's microphone key (cmd/assistant/tvboard.go). Pressed while
// The assistant speaks interrupts it; let go ends the user's turn once the audio before it has gone out.
func (s *Session) PushToTalk(held bool) {
	if !s.realtime() {
		return
	}
	s.mu.Lock()
	s.rtTurnOpen = held
	live := s.state == Live
	gen := s.micGen
	s.mu.Unlock()
	if held {
		if s.out().Speaking(0) {
			s.out().Flush()
			s.sendRT(map[string]any{"type": "response.cancel"})
		}
		return
	}
	_ = gen
	if live {
		s.mu.Lock()
		s.rtCommitPending = true // after the audio still waiting in pending (feedLocked)
		s.mu.Unlock()
	}
}

// rtFirstTurn: the first words went to the session (commit); if the key is already let go the
// turn is complete once they are queued. The marker went out before the preloaded audio on
// 29.10.2026: the turn was empty and the assistant greeted instead of answering.
func (s *Session) rtFirstTurnLocked() {
	if s.realtime() && !s.rtTurnOpen {
		s.rtCommitPending = true
	}
}

// rtCommitNowLocked (feedLocked): the pending audio is queued, the turn may end behind it.
func (s *Session) rtCommitNowLocked() bool {
	if !s.rtCommitPending || s.state != Live || len(s.pending) >= micChunk {
		return false
	}
	s.rtCommitPending = false
	return true
}

// rtEvent translates a Realtime event into GPT Live's events for onEvent.
func (s *Session) rtEvent(data []byte) {
	var ev struct {
		Type      string `json:"type"`
		Delta     string `json:"delta"`
		EventID   string `json:"event_id"`
		Name      string `json:"name"`
		CallID    string `json:"call_id"`
		Arguments string `json:"arguments"`
		Text      string `json:"text"`
		Session   struct {
			ID string `json:"id"`
		} `json:"session"`
		Error struct {
			Message string `json:"message"`
		} `json:"error"`
		Response struct {
			Usage struct {
				InputTokens  int `json:"input_tokens"`
				OutputTokens int `json:"output_tokens"`
			} `json:"usage"`
		} `json:"response"`
	}
	if json.Unmarshal(data, &ev) != nil {
		return
	}
	live := func(m map[string]any) {
		out, _ := json.Marshal(m)
		s.onEvent(out)
	}
	switch ev.Type {
	case "session.updated":
		s.mu.Lock()
		first := !s.rtStarted
		s.rtStarted = true
		s.mu.Unlock()
		if first {
			live(map[string]any{"type": "session.started", "session": map[string]any{"id": ev.Session.ID}})
			s.mu.Lock()
			s.greetAt = time.Time{} // no greeting: the user holds the key and talks
			s.rtFirstTurnLocked()
			s.mu.Unlock()
		}
	case "response.output_audio.delta":
		live(map[string]any{"type": "session.output_audio.delta", "delta": ev.Delta})
	case "response.output_audio_transcript.delta":
		live(map[string]any{"type": "session.output_transcript.delta", "delta": ev.Delta})
	case "conversation.item.input_audio_transcription.delta":
		live(map[string]any{"type": "session.input_transcript.delta", "delta": ev.Delta, "event_id": ev.EventID})
	case "response.function_call_arguments.done":
		if ev.Name == "delegate" {
			s.rtDelegate(ev.CallID, ev.Arguments)
		}
	case "response.output_text.done":
		// gpt-realtime-mini, made to call delegate, wrote the call as text on 29.10.2026:
		// {"task": "…"}. Taken as the call.
		var call struct {
			Task string `json:"task"`
		}
		if json.Unmarshal([]byte(strings.TrimSpace(ev.Text)), &call) == nil && call.Task != "" {
			s.rtDelegate("", ev.Text)
		}
	case "response.output_text.delta":
	case "response.created":
		s.rtResponding.Store(true)
	case "response.done":
		s.rtResponding.Store(false)
		if u := ev.Response.Usage; u.InputTokens+u.OutputTokens > 0 {
			log.Printf("[Realtime] tokens in %d, out %d", u.InputTokens, u.OutputTokens)
		}
	case "error":
		live(map[string]any{"type": "error", "error": map[string]any{"message": ev.Error.Message}})
	case "session.created", "input_audio_buffer.committed", "conversation.item.added", "conversation.item.done",
		"response.output_item.added", "response.output_item.done", "response.content_part.added",
		"response.content_part.done", "response.output_audio.done", "response.output_audio_transcript.done",
		"conversation.item.input_audio_transcription.completed", "response.function_call_arguments.delta",
		"rate_limits.updated", "conversation.item.created", "input_audio_buffer.cleared":
	default:
		if _, seen := seenTypes.LoadOrStore("rt:"+ev.Type, true); !seen {
			log.Printf("[Realtime] %s: %.300s", ev.Type, data)
		}
	}
}

// rtDelegate runs the backend for the realtime model's task and hands the result back.
func (s *Session) rtDelegate(callID, arguments string) {
	var args struct {
		Task string `json:"task"`
	}
	json.Unmarshal([]byte(arguments), &args)
	s.onEvent([]byte(`{"type":"session.delegation.created","delegation":{"target":"backend"}}`))
	req := s.Request()
	s.mu.Lock()
	conn := s.conn
	s.mu.Unlock()
	done := make(chan struct{})
	go s.rtFiller(done)
	go func() {
		defer close(done)
		started := time.Now()
		out := "Error: no backend"
		if s.hooks.Delegate != nil {
			s.mu.Lock()
			thread := s.rtThread
			s.mu.Unlock()
			out = s.hooks.Delegate(req, args.Task, &thread)
			s.mu.Lock()
			s.rtThread = thread
			s.mu.Unlock()
		}
		log.Printf("[Delegation] %.80q: %.1f s, %d chars", args.Task, time.Since(started).Seconds(), len(out))
		if strings.HasPrefix(out, "Error") { // 30.10.2026: three failed in a DNS outage, unlogged
			log.Printf("[Delegation] %.300s", out)
		}
		s.mu.Lock()
		same := s.conn == conn
		if same {
			s.lastActivity = time.Now()
			s.working = false // set by session.delegation.created
		}
		s.mu.Unlock()
		if !same {
			// The session it belonged to has ended: a search of 137 s answered an old question in
			// the next conversation (phone, 01.11.2026).
			log.Printf("[Delegation] its session has ended: result dropped")
			return
		}
		item := map[string]any{"type": "function_call_output", "call_id": callID, "output": out}
		if callID == "" { // the call came as text: the result as a note
			item = map[string]any{"type": "message", "role": "system", "content": []any{map[string]any{
				"type": "input_text", "text": "The backend's result (delegate), to be spoken now:\n" + out}}}
		}
		s.sendRT(map[string]any{"type": "conversation.item.create", "item": item})
		s.rtWaitIdle() // the filler may still be running: one response at a time
		s.sendRT(map[string]any{"type": "response.create", "response": map[string]any{"tool_choice": "none"}})
	}()
}

// rtFillerAfter: a backend that takes longer gets a short spoken filler first. The model is made to
// call delegate before it says anything, and a web search took 20 s of silence on the phone
// (01.11.2026): the user thought the assistant had stopped.
const rtFillerAfter = 1500 * time.Millisecond

const rtFillerNote = "Say only a very short German filler that you are on it, at most six words and in " +
	"your own words (like „Einen Moment, ich schaue nach.“). No facts, no question, nothing else."

func (s *Session) rtFiller(done <-chan struct{}) {
	select {
	case <-done:
		return
	case <-time.After(rtFillerAfter):
	}
	s.rtWaitIdle() // the response that called delegate ends first
	select {
	case <-done:
		return
	default:
	}
	if s.State() != Live {
		return
	}
	s.sendRT(map[string]any{"type": "response.create", "response": map[string]any{
		"tool_choice": "none", "instructions": rtFillerNote}})
}

// rtWaitIdle waits until no response runs (response.created to response.done), at most 5 s.
func (s *Session) rtWaitIdle() {
	for end := time.Now().Add(5 * time.Second); time.Now().Before(end); time.Sleep(50 * time.Millisecond) {
		if !s.rtResponding.Load() {
			return
		}
	}
}

// endpoint: GPT Live, or the Realtime API with the model.
func (s *Session) endpoint() string {
	if s.realtime() {
		return fmt.Sprintf("%s?model=%s", realtimeURL, s.opts.Realtime)
	}
	return liveURL
}

// SetRealtime chooses GPT Live ("") or a Realtime model for the next session (Home Assistant's
// select on the Fire TV, for comparing).
func (s *Session) SetRealtime(model string) {
	s.mu.Lock()
	s.realtimeNext, s.realtimeSet = model, true
	s.mu.Unlock()
}

// applyRealtimeLocked at Start: the choice takes effect between sessions only.
func (s *Session) applyRealtimeLocked() {
	if s.realtimeSet {
		s.opts.Realtime, s.realtimeSet = s.realtimeNext, false
		log.Printf("[Live] protocol: %s", map[bool]string{true: "Realtime " + s.opts.Realtime, false: "GPT Live"}[s.opts.Realtime != ""])
	}
	// rtTurnOpen stays: it is the remote's key (PushToTalk), held while the first session connects.
	// Reset here, the session took the key as let go once it was up and ended the user's turn
	// after its first few hundred ms: the model made something up from the scrap, and the user
	// had to say it again (29.10.2026, every first conversation).
	s.rtHeard, s.rtStarted, s.rtCommitPending, s.rtThread, s.rtSamples = false, false, false, "", 0
}
