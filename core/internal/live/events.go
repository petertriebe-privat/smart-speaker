package live

import (
	"context"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"log"
	"strings"
	"sync"
	"time"

	"github.com/coder/websocket"

	"echo-assistant/internal/textutil"
	"echo-assistant/internal/tools"
	"echo-assistant/internal/transcript"
)

type event struct {
	Type       string          `json:"type"`
	Delta      string          `json:"delta"`
	Session    json.RawMessage `json:"session"`
	Delegation json.RawMessage `json:"delegation"`
	Event      json.RawMessage `json:"event"`
	Usage      struct {
		Seconds float64 `json:"seconds"`
	} `json:"usage"`
	Reason        string `json:"reason"`
	EventID       string `json:"event_id"`
	SourceEventID string `json:"source_event_id"`
	Error         struct {
		Message string `json:"message"`
	} `json:"error"`
}

var seenTypes sync.Map

func (s *Session) read(ctx context.Context, conn *websocket.Conn) {
	for {
		_, data, err := conn.Read(ctx)
		if err != nil {
			// A reader of an earlier connection must not end the conversation that runs now
			// (16.10.2026: a quick second button press failed the new session).
			s.mu.Lock()
			current := s.conn == conn
			s.mu.Unlock()
			if !current || ctx.Err() != nil {
				return
			}
			switch s.State() {
			case Closing:
				s.finish()
			case Idle:
			default:
				s.fail("Verbindung getrennt: " + err.Error())
			}
			return
		}
		if s.realtime() {
			s.rtEvent(data)
		} else {
			s.onEvent(data)
		}
	}
}

func (s *Session) onEvent(data []byte) {
	var ev event
	if err := json.Unmarshal(data, &ev); err != nil {
		log.Printf("[Warning] invalid JSON (%v, %d bytes): %.120s", err, len(data), data)
		return
	}
	switch ev.Type {
	case "session.output_audio.delta":
		s.onAudio(ev.Delta)
	case "session.output_transcript.delta":
		// Not the end of delegated work: Jarvis often says "Einen Moment" while it runs.
		s.mu.Lock()
		s.lastActivity, s.greetAt = time.Now(), time.Time{}
		s.assistantSaid.WriteString(ev.Delta)
		s.assistantDelta = s.lastActivity
		s.turnDone = true
		s.mu.Unlock()
		s.words.Append(transcript.Assistant, ev.Delta)
	case "session.input_transcript.delta":
		s.mu.Lock()
		s.lastActivity, s.greetAt = time.Now(), time.Time{}
		if s.insisting {
			log.Printf("[Speaker] the user answered after %d attempts", s.attempts+1)
		}
		s.insisting, s.heardUser = false, true
		s.assistantSaid.Reset()
		s.delegatedTurn, s.nudgedTurn, s.toolTurn = false, false, false
		if s.turnDone || s.turnUser.Len() == 0 {
			s.turnUser.Reset()
			s.turnDone = false
			s.turnStartAt, s.turnAction = s.deltaCount, -1 // a new turn: the referee's answer starts over
		}
		s.turnUser.WriteString(ev.Delta)
		s.onInputDelta(ev.EventID)
		if !s.quickChecked {
			s.userText.WriteString(ev.Delta)
			s.userDelta = time.Now()
			if s.userFirst.IsZero() {
				s.userFirst = s.userDelta
			}
		}
		s.mu.Unlock()
		s.words.Append(transcript.User, ev.Delta)
	case "session.started":
		s.onStarted(ev, len(data))
	case "session.delegation.created":
		var d struct {
			ID     string `json:"id"`
			Target string `json:"target"`
		}
		json.Unmarshal(ev.Delegation, &d)
		log.Printf("[Delegation] %s", d.Target)
		s.mu.Lock()
		s.delegatedTurn = true
		s.mu.Unlock()
		s.setWorking()
	case "response.event":
		s.onBackend(ev.Event)
	case "decision.event":
		s.onDecision(ev.SourceEventID, ev.Event)
	case "session.usage.updated":
		s.mu.Lock()
		s.usage = max(s.usage, int(ev.Usage.Seconds))
		s.mu.Unlock()
	case "session.closed":
		s.mu.Lock()
		s.usage = max(s.usage, int(ev.Usage.Seconds))
		s.closedBy = ev.Reason
		s.mu.Unlock()
		log.Printf("[Closed] %s", ev.Reason)
		s.finish()
	case "error":
		if s.State() == Starting {
			s.fail(ev.Error.Message)
			return
		}
		log.Printf("[API error] %s", ev.Error.Message)
	default:
		// Protocol discovery: each unknown event type once, with its start.
		if _, seen := seenTypes.LoadOrStore(ev.Type, true); !seen {
			log.Printf("[Event] %s: %.300s", ev.Type, data)
		}
	}
}

func (s *Session) onStarted(ev event, size int) {
	var info struct {
		ID string `json:"id"`
	}
	json.Unmarshal(ev.Session, &info)
	s.mu.Lock()
	log.Printf("[Session] %s (%d bytes, %d ms after the start)", info.ID, size, time.Since(s.sessionSince).Milliseconds())
	s.setStateLocked(Live)
	s.lastActivity = time.Now()
	s.liveAt = s.lastActivity // what was recorded while connecting goes out now
	// On the phone the assistant answers right away, like anyone picking up; when it called itself it
	// speaks once the user picks up (the phone package sends the commentary).
	switch {
	case s.req.Phone && s.req.Reason != "":
		s.greetAt = time.Time{}
	case s.req.Phone:
		s.greetAt = s.lastActivity.Add(phoneGreetingDelay)
	default:
		s.greetAt = s.lastActivity.Add(greetingDelay)
	}
	// Nobody spoke while connecting: seconds of silence would only take time to send.
	if s.voiceMs < 100 && len(s.pending) > keepSilent {
		s.pending = append(s.pending[:0], s.pending[len(s.pending)-keepSilent:]...)
	}
	s.mu.Unlock()
}

func (s *Session) onAudio(b64 string) {
	raw, err := base64.StdEncoding.DecodeString(b64)
	if err != nil {
		log.Print("[Warning] audio delta not decodable")
		return
	}
	pcm := make([]int16, len(raw)/2)
	for i := range pcm {
		pcm[i] = int16(binary.LittleEndian.Uint16(raw[i*2:]))
	}
	if st := s.State(); st != Live && st != Closing {
		return
	}
	s.outMu.Lock()
	defer s.outMu.Unlock()
	if !s.holdAudio(pcm) {
		s.play(pcm)
	}
}

// play queues GPT Live's audio. Called with s.outMu held.
func (s *Session) play(pcm []int16) {
	if s.barging() {
		return // interrupted: what GPT Live still sends of the old answer (bargein.go)
	}
	// GPT Live paces audio in real time and streams silence too: while the backlog is too long,
	// silent packets are dropped so the voice catches up in the next pause.
	if s.out().Queued() > speakerLag && peakOf(pcm) < silencePeak {
		return
	}
	// In the reader goroutine, so the packets stay in order; blocks only while PulseAudio's
	// buffer is full, and GPT Live sends in real time.
	if err := s.out().Queue(pcm); err != nil {
		log.Printf("[Warning] speaker: %v", err)
	}
}

func (s *Session) setWorking() {
	s.mu.Lock()
	s.working, s.workingSince, s.lastActivity, s.greetAt = true, time.Now(), time.Now(), time.Time{}
	s.mu.Unlock()
}

func (s *Session) onBackend(raw json.RawMessage) {
	var ev struct {
		Type string `json:"type"`
		Item struct {
			Type      string `json:"type"`
			Name      string `json:"name"`
			Arguments string `json:"arguments"`
			CallID    string `json:"call_id"`
		} `json:"item"`
	}
	json.Unmarshal(raw, &ev)
	s.mu.Lock()
	s.lastActivity = time.Now()
	s.mu.Unlock()
	finished := ev.Type == "response.completed" || ev.Type == "response.failed" ||
		ev.Type == "response.incomplete" || ev.Type == "response.cancelled"
	if finished || ev.Type == "response.created" || ev.Type == "response.web_search_call.searching" {
		log.Printf("[Backend] %s", ev.Type)
	}
	if ev.Type == "response.output_item.done" && ev.Item.Type == "function_call" {
		s.onFunctionCall(ev.Item.Name, ev.Item.Arguments, ev.Item.CallID)
	} else if finished {
		// The silence timer starts now, not while a slow tool still runs.
		s.mu.Lock()
		s.working = s.slowCalls > 0
		s.mu.Unlock()
	}
}

func (s *Session) onFunctionCall(name, arguments, callID string) {
	if s.holdCall(name, arguments, callID) {
		return
	}
	s.mu.Lock()
	s.toolTurn = true
	s.mu.Unlock()
	req := s.Request()
	ctx := tools.Context{Channel: req.Channel(), Person: req.Person, Origin: req.Origin(), Conversation: s}
	if !req.Phone {
		ctx.Room = s.opts.Room
	}
	// Every call in its own goroutine: the reader must keep the audio flowing (on the M5 a
	// blocking call held the loop). The session counts it as work until the result is sent.
	s.mu.Lock()
	s.slowCalls++
	s.mu.Unlock()
	s.setWorking()
	go func() {
		started := time.Now()
		out := s.tools.Call(name, arguments, ctx)
		log.Printf("[Tool] %s %.200s -> %.200s (%d ms)", name, arguments, out, time.Since(started).Milliseconds())
		s.mu.Lock()
		if len(s.toolLog) < 30 {
			s.toolLog = append(s.toolLog, "- "+name+" "+textutil.Prefix(arguments, 200)+" -> "+
				strings.ReplaceAll(textutil.Prefix(out, 200), "\n", " "))
		}
		s.slowCalls--
		s.mu.Unlock()
		s.sendResult(callID, out)
	}()
}

// sendResult: the output as a function_call_output item, then response.create lets the
// backend continue.
func (s *Session) sendResult(callID, output string) {
	if st := s.State(); st != Live && st != Closing {
		return
	}
	s.send(map[string]any{"type": "response.item.create",
		"item": map[string]any{"type": "function_call_output", "call_id": callID, "output": output}})
	s.send(map[string]any{"type": "response.create"})
	s.setWorking()
}
