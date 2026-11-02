// Package live runs a GPT Live voice conversation over WebSocket, following
// devices/atoms3r/src/voice/live_session.cpp (protocol, timers and prompts).
package live

import (
	"context"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/coder/websocket"

	"echo-assistant/internal/audio"
	"echo-assistant/internal/netx"
	"echo-assistant/internal/prompts"
	"echo-assistant/internal/textutil"
	"echo-assistant/internal/tools"
	"echo-assistant/internal/transcript"
)

type State int

const (
	Idle State = iota
	Connecting
	Starting
	Live
	Closing
)

func (s State) String() string {
	return [...]string{"idle", "connecting", "starting", "live", "closing"}[s]
}

const (
	liveURL    = "wss://api.openai.com/v1/live/sessions"
	micChunk   = audio.SpeakerRate / 25 // 40 ms per append event
	maxPreroll = 15 * audio.SpeakerRate // recorded while connecting
	// of a preroll without voice: the last 2 s (0.5 s lost a quiet "spiel meine Musik", 20.10.2026)
	keepSilent = 2 * audio.SpeakerRate
)

// Settings from the M5's config.h (milliseconds).
var (
	greetingDelay      = ms("GREETING_DELAY_MS", 1500)
	phoneGreetingDelay = ms("PHONE_GREETING_DELAY_MS", 300)
	conferenceIdle     = ms("CONFERENCE_IDLE_MS", 30000) // The assistant leaves a call it was brought into
)

// conferenceGreeting: LIVE_CONFERENCE_GREETING's default (prompts.h has the text).
const conferenceGreeting = "System note, not from the user: you were brought into a phone call that runs on the " +
	"speaker at home. Greet the user at home and {names} on the phone in one short sentence and say you are " +
	"listening; then wait until someone addresses you."

var (
	phoneIdleTimeout   = ms("PHONE_IDLE_TIMEOUT_MS", 20000)
	greetingAfterVoice = ms("GREETING_AFTER_SPEECH_MS", 5000)
	wakeSilentEnd      = ms("WAKE_SILENT_END_MS", 4000) // after the wake word nobody spoke: ends silently (#78)
	ackWait            = ms("ACK_WAIT_MS", 500)
	quickSilence       = ms("QUICK_COMMAND_SILENCE_MS", 300) // the transcript rested
	quickWindow        = 8 * time.Second
	delegationEffort   = prompts.TextOr("DELEGATION_EFFORT", "low") // config.h: reasoning of the backend model in a conversation
	quickSettled       = 1200 * time.Millisecond                    // no new words: the sentence is over
	holdMax            = ms("QUICK_HOLD_MAX_MS", 12000)             // the hold ends at the latest (#59)
	idleTimeout        = ms("IDLE_TIMEOUT_MS", 10000)
	answerTimeout      = ms("ANSWER_TIMEOUT_MS", 15000)
	endCheckQuiet      = ms("END_CHECK_QUIET_MS", 1500)
	thinkingTimeout    = ms("THINKING_TIMEOUT_MS", 180000)
	maxSession         = ms("MAX_SESSION_MS", 20*60*1000)
	connectTimeout     = ms("CONNECT_TIMEOUT_MS", 15000)
	speakerLag         = ms("SPEAKER_LAG_MS", 500)
	echoTail           = ms("ECHO_TAIL_MS", 300)
	noticeQuiet        = ms("NOTICE_QUIET_MS", 1000)
	noticeMaxWait      = ms("NOTICE_MAX_WAIT_MS", 45000)
	speakerRepeat      = ms("SPEAKER_REPEAT_MS", 5000)
	speakerInsist      = ms("SPEAKER_INSIST_MS", 600000)
	silencePeak        = prompts.Int("SILENCE_PEAK", 300)
	voicePeak          = prompts.Int("VOICE_PEAK", 1000)
	closeTimeout       = 5 * time.Second
)

// notUnderstoodVoiceMs: this much voice without a word in either transcript is "not understood".
const notUnderstoodVoiceMs = 400

// SetVoicePeak: the microphone peak that counts as the user's voice. The M5's VOICE_PEAK (1000)
// is far above a voice through the Echo's DSP (a few hundred): speech while connecting was then
// dropped as silence, "Ja, Sir?" played into the user's sentence and the greeting talked over
// their request (16.10.2026).
func SetVoicePeak(peak int) {
	if peak > 0 {
		voicePeak = peak
	}
}

func ms(name string, def int) time.Duration {
	return time.Duration(prompts.Int(name, def)) * time.Millisecond
}

// Output: where the assistant's voice goes: the Echo's speaker (audio.Speaker) or a phone line.
type Output interface {
	Queue(pcm []int16) error // 24 kHz
	Queued() time.Duration
	Speaking(tail time.Duration) bool
	Flush()
	Tone(freq float64, ms int, amp float64)
}

// Request: why a conversation starts. Empty Reason at the speaker: the user pressed the button.
type Request struct {
	Reason string // The assistant speaks up / calls itself: the occasion or task
	Person string // who he talks with for sure, "" = unknown
	Insist bool   // address them again until they answer (speaker only)
	Again  bool   // the device just asked "Wie bitte?": no acknowledgement clip
	// Wake: the wake word started it. No "Ja, Sir?" then: the user goes on talking after "Jarvis",
	// and the clip in their breath covered the command and muted it for GPT Live and the quick
	// path alike (18.10.2026 at once, 20.10.2026 still after 0.6 s: "Hey Jarvis … Licht an" was
	// lost). No cue either since 28.10.2026, only the ring (cmd/assistant/silentwake.go); if
	// nobody speaks it ends silently (endsSilently).
	Wake   bool
	Repeat time.Duration // silence before he tries again (insist)
	// Decided: the device took the first words itself and found no quick command
	// (cmd/assistant/capture.go): no hold, no acknowledgement. Audio: what was said since the
	// wake word or the button (16 kHz), GPT Live's first input. Heard: the device's transcript of
	// it, "" when it has none.
	Decided bool
	Audio   []int16
	Heard   string
	// Warm: connected ahead while the device still decides (Decided must be set too): GPT Live
	// gets no audio and nothing speaks up or ends it until Commit; Close drops it.
	Warm bool

	// A phone call (package phone): the audio runs over the line; the caller's audio comes in
	// with FeedLine.
	Phone      bool
	Line       Output
	Caller     string // "Name (number)" or the number
	CallerName string // from the register or the address book: the greeting uses it
	Stranger   bool   // someone outside the household: no memory, only the call's tools
	// Callback: a number the assistant called for the user calls back (phone/followup.go): the task and
	// the earlier calls with it. "" = an ordinary call.
	Callback string
	// Conference: The assistant joins a call on the speaker at home (#54): the line is the room and the
	// far end at once. The participants other than the user, for the instructions and the
	// greeting; "" = no conference.
	Conference string
}

// Channel: where the conversation's tools may come from.
func (r Request) Channel() tools.Channel {
	if r.Stranger {
		return tools.Stranger
	}
	return tools.Voice
}

// Origin: how this conversation came in, for sender lines of messages passed on.
func (r Request) Origin() string {
	switch {
	case !r.Phone:
		return "Lautsprecher"
	case r.Reason != "":
		return "Anruf von " + prompts.Name()
	}
	return "Anruf"
}

// Ended: what the device keeps of a conversation.
type Ended struct {
	Request    Request
	Started    time.Time
	Transcript string
	Seconds    int // billed
	HeardUser  bool
	Silent     bool // woken by the wake word, nobody said a word: ended without the greeting
	// Tools: the tools that ran, one per line with arguments and result (shortened), so the
	// memory records what Jarvis did, not what it said it did (#71). "" = none.
	Tools string
	// ClosedBy: why GPT Live closed the session itself ("content": its moderation stopped an
	// answer and closed it, 25.10.2026 mid-sentence on a harmless shopping talk); "" = not by it.
	ClosedBy string
}

// Hooks: what the device does around the conversation (instructions, memory).
type Hooks struct {
	OnState func(State)
	OnWords func(user bool, line string) // the current line as it grows (captions on a TV)
	// Delegate runs the backend for a Realtime session (GPT Live delegates itself): the task with
	// the backend instructions and every tool; thread keeps the backend's conversation.
	Delegate     func(req Request, task string, thread *string) string
	Instructions func(Request) string           // persona + instructions + context for GPT Live
	Backend      func(Request) string           // instructions of the delegation backend
	OnEnd        func(Ended)                    // after the session (memory), in its own goroutine
	OnOver       func(Ended)                    // after every session, also one without a word (wake word clips)
	Closing      func(transcript string) string // agenda items not mentioned yet ("" = none)
	OnError      func(message string)           // the conversation failed (the ring shows it)
	// Acknowledge: after the button, if the user is still quiet: plays "Ja, Sir?" and returns its
	// length (0 = no clip, the session beeps instead). nil = only the beep.
	Acknowledge func() time.Duration
	// QuickCommand: the user's first utterance, once they stopped talking. true = handled on the
	// device (a music command like "Pause"): the conversation ends at once, without GPT Live
	// answering or delegating. nil = never.
	// settled: nothing new for quickSettled (GPT Live's transcript often has no final full stop).
	// decision: GPT Live's own Decisions trigger on the words so far (Triggers), zero when none.
	QuickCommand func(text string, settled bool, decision Decision) bool
	// Triggers: session.triggers for GPT Live (Decisions on the user's transcript), nil = none.
	Triggers []any
	// Unfulfilled: GPT Live announced an action ("ich pausiere es") and did not delegate it. The
	// device may do it itself from the user's last words; it returns a system note for GPT Live
	// ("" = not something it can do: the usual reminder goes). In its own goroutine.
	// recent: the end of the conversation's transcript ("Nein, läuft nicht" alone does not say
	// that the video was meant).
	Unfulfilled func(userTurn, recent, said string) string
	// NotUnderstood: the hold's deadline passed, the user spoke, but neither GPT Live's
	// transcript nor the device has a word of it. true = the device takes over (asks again);
	// the held answer - GPT Live's guess - is never played. false = released as usual.
	NotUnderstood func() bool
	// HoldEnded: the hold on GPT Live's voice and calls is over (Release or its deadline), the
	// conversation goes on as usual. Not called when the conversation ends while held.
	HoldEnded func()
}

type Options struct {
	APIKey   string
	EchoGate bool   // at start: mute the microphone while the assistant is audible (SetEchoGate changes it)
	Label    string // "speaker" or "phone": several sessions run side by side
	Room     string // the speaker's room (tools.Context.Room); "" for the phone
	// Realtime: a Realtime model instead of GPT Live (realtime.go), for push to talk; "" = GPT Live.
	// SetRealtime changes it for the next session.
	Realtime string
}

// EndSoon: end_call: hang up once the assistant is quiet (GPT Live decided it is over).
func (s *Session) EndSoon() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.state != Idle {
		s.endRequested = time.Now()
	}
}

// Elapsed: how long the conversation has been open (0 when idle).
func (s *Session) Elapsed() time.Duration {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.state == Idle {
		return 0
	}
	return time.Since(s.sessionSince)
}

// Label: which of the assistant's conversations this is.
func (s *Session) Label() string { return s.opts.Label }

// EndCallTool: end_call for all sessions; it ends the one it was called from (ctx.Conversation).
func EndCallTool() tools.Tool {
	return tools.Tool{
		Name:        "end_call",
		Description: "Ends the conversation (hangs up a phone call) once it is over. Say goodbye first.",
		Parameters:  `{"type": "object", "additionalProperties": false, "properties": {}, "required": []}`,
		Channels:    tools.Voice | tools.Stranger,
		Run: func(_ map[string]any, ctx tools.Context) string {
			if ctx.Conversation == nil {
				return "Error: no conversation is running."
			}
			ctx.Conversation.EndSoon()
			return "Ended."
		},
	}
}

type notice struct {
	text   string
	urgent bool
	at     time.Time
}

type Session struct {
	opts    Options
	hooks   Hooks
	tools   *tools.Registry
	speaker *audio.Speaker
	mic     *audio.Mic
	words   *transcript.Transcript
	ended   *endCheck

	mu           sync.Mutex
	state        State
	stateSince   time.Time
	sessionSince time.Time
	startedAt    time.Time
	liveAt       time.Time // session.started: from here on GPT Live has the microphone (zero before)
	req          Request
	conn         *websocket.Conn
	cancel       context.CancelFunc
	up           audio.Upsampler
	pending      []int16 // 24 kHz microphone audio not sent yet
	voiceMs      int     // voice heard since the start
	lastVoice    time.Time
	lastActivity time.Time
	greetAt      time.Time
	ackAt        time.Time // acknowledge the button then, unless the user talks already
	ackUntil     time.Time // the clip plays: the microphone hears the assistant, not the user
	// When the clip was queued, and the first and last loud microphone frame after it: does the
	// microphone still hear the clip after ackUntil (19.10.2026, "Ja" transcribed after "Ja, Sir?")?
	ackQueued, ackLoudFirst, ackLoudLast time.Time
	// what the assistant said since the user last spoke, and whether it delegated: an announced action
	// without a delegation gets a reminder (nudgeIfClaimed)
	assistantSaid    strings.Builder
	assistantDelta   time.Time
	delegatedTurn    bool
	toolTurn         bool     // a tool ran since the user last spoke (nudgeIfClaimed)
	toolLog          []string // the tools that ran with their results, for Ended.Tools
	nudgedTurn       bool
	micQ             chan micPacket  // to micSender
	micGen           int             // conversation number: queued audio of an earlier one is dropped
	turnUser         strings.Builder // what the user said since the assistant last spoke
	turnDone         bool            // The assistant spoke since: the next user words start a new turn
	turnStartAt      int             // the transcript part before this turn (decisions.go)
	turnAction       float64         // the referee: probability that this turn needs an action, -1 unknown
	userText         strings.Builder
	userFirst        time.Time      // first piece of the user's transcript
	userDelta        time.Time      // last piece of the user's transcript
	quickChecked     bool           // QuickCommand is done with this conversation
	quickLen         int            // how much of userText it saw
	quickSettledSeen bool           // it saw the settled transcript
	deltaOrder       map[string]int // the user's transcript events by id: their number
	deltaCount       int
	decision         Decision // the trigger's latest result for the first utterance
	decisionAt       int      // the transcript part it covers
	decisionNew      bool     // QuickCommand has not seen it yet
	// The hold (#59): after the button or the wake word GPT Live hears the user, but what it says
	// and the tools it calls wait on the device until the device decided whether the first
	// utterance is a quick command. Released -> played and run; the conversation ends -> dropped.
	held      bool
	holdUntil time.Time
	heldAudio [][]int16
	heldCalls []heldCall
	// Discard: the rest of the dropped answer, still streaming, is dropped too (dropRest)
	skipQuiet      bool // Request.Audio given: the quiet while connecting is left out (feedLocked)
	warm           bool // Request.Warm until Commit: no audio, no timers
	dropping       bool
	dropSince      time.Time
	dropQuietMs    int
	dropMs         int
	outMu          sync.Mutex // keeps GPT Live's audio in order while the held audio is queued
	working        bool
	workingSince   time.Time
	slowCalls      int
	endRequested   time.Time
	heardUser      bool
	silentEnd      bool   // ended after the wake word without a word (greetIfSilent)
	closedBy       string // GPT Live closed the session itself: its reason (Ended.ClosedBy)
	closingOffered bool
	insisting      bool
	attempts       int
	usage          int
	// Realtime (realtime.go): the user holds the key; audio of this turn went up; the session is
	// set up; the backend's conversation.
	rtTurnOpen, rtHeard, rtStarted, rtCommitPending bool
	rtSamples                                       int // audio sent in this turn (24 kHz)
	rtThread                                        string
	rtResponding                                    atomic.Bool // a response runs (response.created to response.done)
	realtimeNext                                    string      // SetRealtime: for the next session
	realtimeSet                                     bool
	notices                                         []notice
	checked                                         time.Time // lastActivity the end check last ran for
	checkedAt                                       time.Time
	checks                                          int
	sendMu                                          sync.Mutex
	echoGate                                        atomic.Bool
	bargeUntil                                      atomic.Int64 // unix ms: The assistant was just interrupted (bargein.go)
	paused                                          atomic.Bool
	current                                         atomic.Pointer[Request] // the conversation's request, read without s.mu
}

func New(opts Options, hooks Hooks, registry *tools.Registry, speaker *audio.Speaker, mic *audio.Mic) *Session {
	s := &Session{opts: opts, hooks: hooks, tools: registry, speaker: speaker, mic: mic,
		words: transcript.New(), ended: &endCheck{}}
	s.echoGate.Store(opts.EchoGate)
	if hooks.OnWords != nil {
		s.words.OnLine = func(sp transcript.Speaker, text string) { hooks.OnWords(sp == transcript.User, text) }
	}
	s.micQ = make(chan micPacket, 1024) // 40 s of audio
	go s.micSender()
	go s.loop()
	return s
}

// SetEchoGate: true mutes the microphone while the assistant is audible (it cannot be interrupted);
// false relies on the DSP's echo cancellation.
func (s *Session) SetEchoGate(on bool) { s.echoGate.Store(on) }
func (s *Session) EchoGate() bool      { return s.echoGate.Load() }

// SetPaused holds the conversation without ending it: the model hears nothing more (FeedLine
// drops the audio), says nothing more (what he had begun is dropped) and none of the timers that
// end a conversation run - the silence is wanted. Used while someone at home takes part in a
// phone call themselves (package phone): the model keeps the conversation and its transcript and
// is back with Commentary when they ask for him.
func (s *Session) SetPaused(on bool) {
	if s.paused.Swap(on) == on {
		return
	}
	// Pausing is a phone thing: drop what he had begun to say on the line.
	if r := s.current.Load(); on && r != nil && r.Phone && r.Line != nil {
		r.Line.Flush()
	}
	s.mu.Lock()
	s.lastActivity = time.Now() // the timers start from here, not from the pause
	s.mu.Unlock()
	log.Printf("[Live] %s: paused %v", s.opts.Label, on)
}

// Paused: the conversation is held (SetPaused).
func (s *Session) Paused() bool { return s.paused.Load() }

func (s *Session) State() State {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.state
}

func (s *Session) Open() bool { return s.State() != Idle }

// Working: delegated work running (thinking animation).
func (s *Session) Working() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.working
}

// Person: who Jarvis talks with in the running conversation ("" = unknown or none).
func (s *Session) Person() string { return s.Request().Person }

// Request of the running (or last) conversation.
func (s *Session) Request() Request {
	if r := s.current.Load(); r != nil {
		return *r
	}
	return Request{}
}

// OnPhone: the open conversation is a phone call.
func (s *Session) OnPhone() bool { return s.Open() && s.Request().Phone }

// out: where the voice goes (lock-free: called with and without s.mu).
func (s *Session) out() Output {
	if r := s.current.Load(); r != nil && r.Phone && r.Line != nil {
		return r.Line
	}
	return s.speaker
}

// FeedLine takes the caller's audio of a phone call (24 kHz). While the conversation is paused
// the model hears nothing: someone at home is talking on the line themselves.
func (s *Session) FeedLine(pcm []int16) {
	if s.Request().Phone && !s.paused.Load() {
		s.feed24(pcm)
	}
}

func (s *Session) setStateLocked(next State) {
	s.state, s.stateSince = next, time.Now()
	if s.hooks.OnState != nil {
		go s.hooks.OnState(next)
	}
}

// preloadLocked: what was said before the session (16 kHz) is GPT Live's first input; the quiet
// while connecting is left out from then on (skipQuiet). Called with s.mu held.
func (s *Session) preloadLocked(pcm []int16) {
	s.skipQuiet = len(pcm) > 0
	if len(pcm) == 0 {
		return
	}
	s.pending = append(s.pending, s.up.Process(pcm, nil)...)
	for i := 0; i+audio.MicRate/50 <= len(pcm); i += audio.MicRate / 50 {
		if peakOf(pcm[i:i+audio.MicRate/50]) >= voicePeak {
			s.voiceMs += 20
		}
	}
}

// Warm: connected ahead, not committed yet (Request.Warm).
func (s *Session) Warm() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.warm && s.state != Idle
}

// Commit: the device decided that GPT Live answers; a warm session gets what was said (pcm, 16
// kHz) and the device's transcript of it and runs from now on as any other. False when the
// session is not warm (closed meanwhile).
func (s *Session) Commit(pcm []int16, heard string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.warm || s.state == Idle || s.state == Closing {
		return false
	}
	s.warm = false
	req := s.req
	req.Warm, req.Heard = false, heard
	s.req = req
	s.current.Store(&req)
	s.preloadLocked(pcm)
	s.rtFirstTurnLocked() // Realtime: the key already let go ends the turn
	s.lastActivity = time.Now()
	if s.state == Live && !s.realtime() { // push to talk: no greeting, the user has spoken
		s.greetAt = s.lastActivity.Add(greetingDelay)
	}
	log.Printf("[Live] committed: %.1f s of audio", float64(len(pcm))/audio.MicRate)
	return true
}

// Toggle: the action button.
func (s *Session) Toggle() {
	if s.Open() {
		s.Close()
	} else {
		s.Start(Request{})
	}
}

// Start connects; the microphone records from now on (what is said while connecting is kept).
// Returns false if a conversation is already running.
func (s *Session) Start(req Request) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.state != Idle {
		return false
	}
	s.applyRealtimeLocked() // GPT Live or a Realtime model (realtime.go)
	if !netx.TimeSynced {
		log.Print("[Live] clock not set yet, TLS would fail")
	}
	if req.Repeat <= 0 {
		req.Repeat = speakerRepeat
	}
	s.req = req
	s.current.Store(&req)
	s.paused.Store(false) // a conversation never starts held (SetPaused)
	s.pending, s.voiceMs, s.up = s.pending[:0], 0, audio.Upsampler{}
	s.working, s.slowCalls, s.heardUser, s.silentEnd, s.closingOffered = false, 0, false, false, false
	s.insisting, s.attempts = req.Reason != "" && req.Insist, 0
	s.endRequested, s.greetAt, s.usage, s.notices, s.toolLog = time.Time{}, time.Time{}, 0, nil, nil
	s.checked, s.checkedAt, s.checks = time.Time{}, time.Time{}, 0
	s.ackAt, s.ackUntil = time.Time{}, time.Time{}
	s.ackQueued, s.ackLoudFirst, s.ackLoudLast = time.Time{}, time.Time{}, time.Time{}
	s.userText.Reset()
	// what the assistant said in the last conversation is no announcement in this one: a dropped held
	// "Ich starte Ihre Lieblingslieder" was nudged in the next conversation (18.10.2026)
	s.assistantSaid.Reset()
	s.assistantDelta, s.delegatedTurn, s.nudgedTurn, s.toolTurn = time.Time{}, false, false, false
	s.turnUser.Reset()
	s.turnDone, s.turnStartAt, s.turnAction = false, 0, -1
	s.micGen++
	s.userFirst, s.userDelta, s.quickLen, s.quickSettledSeen = time.Time{}, time.Time{}, 0, false
	s.deltaOrder, s.deltaCount, s.decision, s.decisionAt, s.decisionNew = nil, 0, Decision{}, 0, false
	s.quickChecked = req.Reason != "" || req.Phone || req.Decided // only after the button / wake word
	s.held = !s.quickChecked && s.hooks.QuickCommand != nil
	s.holdUntil, s.heldAudio, s.heldCalls = time.Now().Add(holdMax), nil, nil
	s.dropping = false
	if req.Reason == "" && !req.Phone && !req.Again && !req.Wake && !req.Decided { // the button
		s.ackAt = time.Now().Add(ackWait)
	}
	s.warm = req.Warm
	s.preloadLocked(req.Audio)
	if req.Stranger {
		s.words.SetUserName("Gegenüber")
	} else {
		s.words.SetUserName("Du")
	}
	s.words.Clear()
	s.ended.reset()
	s.startedAt, s.liveAt = time.Now(), time.Time{}
	s.setStateLocked(Connecting)
	s.sessionSince = s.stateSince
	ctx, cancel := context.WithCancel(context.Background())
	s.cancel = cancel
	if req.Reason != "" {
		log.Printf("[Connecting] the assistant speaks up: %s", req.Reason)
	} else {
		log.Print("[Connecting]")
	}
	go s.connect(ctx)
	return true
}

func (s *Session) connect(ctx context.Context) {
	dialCtx, cancel := context.WithTimeout(ctx, connectTimeout)
	defer cancel()
	header := http.Header{}
	header.Set("Authorization", "Bearer "+s.opts.APIKey)
	started := time.Now()
	conn, resp, err := websocket.Dial(dialCtx, s.endpoint(), &websocket.DialOptions{
		HTTPClient: netx.Client(0), HTTPHeader: header, CompressionMode: websocket.CompressionDisabled})
	if err != nil {
		detail := ""
		if resp != nil {
			detail = fmt.Sprintf(" (HTTP %d)", resp.StatusCode)
		}
		if ctx.Err() != nil {
			return // closed while connecting (the button again): no failure
		}
		s.fail("Verbindung fehlgeschlagen: " + err.Error() + detail)
		return
	}
	conn.SetReadLimit(16 << 20) // session.started echoes the whole configuration
	log.Printf("[Live] connected in %d ms", time.Since(started).Milliseconds())
	s.mu.Lock()
	if s.state != Connecting {
		s.mu.Unlock()
		conn.Close(websocket.StatusNormalClosure, "")
		return
	}
	s.conn = conn
	s.setStateLocked(Starting)
	req := s.req
	s.mu.Unlock()
	if s.realtime() {
		s.rtStart(req)
	} else {
		s.sendSessionStart(req)
	}
	go s.read(ctx, conn)
}

func (s *Session) sendSessionStart(req Request) {
	session := map[string]any{
		"model":        prompts.Text("LIVE_MODEL"),
		"instructions": s.hooks.Instructions(req),
		"audio": map[string]any{
			"format": map[string]any{"type": "audio/pcm", "rate": audio.SpeakerRate},
			"output": map[string]any{"voice": prompts.Text("LIVE_VOICE")},
		},
		"delegation": map[string]any{
			"type": "responses",
			"responses": map[string]any{
				"model":               prompts.Text("DELEGATION_MODEL"),
				"instructions":        s.hooks.Backend(req),
				"tools":               s.tools.Describe(tools.Context{Channel: req.Channel(), Person: req.Person, Origin: req.Origin()}),
				"parallel_tool_calls": false, // each result goes back with response.create at once
				// Without these the backend thought for 8-9 s per response: "Welches Lied läuft
				// gerade?" took ~20 s (two responses around one tool call, 16.10.2026). GPT Live
				// phrases the result itself, so a short one is enough.
				"reasoning": map[string]any{"effort": delegationEffort},
				"text":      map[string]any{"verbosity": "low"},
			},
		},
	}
	if s.hooks.Triggers != nil && !req.Phone && prompts.Bool("LIVE_DECISION_TRIGGERS", true) {
		session["triggers"] = s.hooks.Triggers
	}
	s.send(map[string]any{"type": "session.start", "session": session})
}

func (s *Session) send(event any) bool {
	if s.realtime() && s.rtOut(event) {
		return true
	}
	data, err := json.Marshal(event)
	if err != nil {
		log.Printf("[Live] event not encodable: %v", err)
		return false
	}
	return s.sendRaw(data)
}

func (s *Session) sendRaw(data []byte) bool {
	s.mu.Lock()
	conn := s.conn
	s.mu.Unlock()
	if conn == nil {
		return false
	}
	s.sendMu.Lock()
	defer s.sendMu.Unlock()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := conn.Write(ctx, websocket.MessageText, data); err != nil {
		log.Printf("[Live] send failed: %v", err)
		return false
	}
	return true
}

// Commentary: a system note for the model (greeting, notices).
func (s *Session) Commentary(text string) {
	if s.State() != Live {
		return
	}
	s.send(map[string]any{"type": "session.commentary.append", "delegation_id": nil, "content": text})
}

// Notice queues news for the user; it goes to the model at the next pause. False if no
// conversation is running.
func (s *Session) Notice(text string, urgent bool) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.state != Live || text == "" {
		return false
	}
	// GPT Live takes at most 500 tokens per context append: a longer notice failed with an API
	// error and was lost (25.10.2026, a research result). Callers send long texts by message too.
	if limit := prompts.Int("NOTICE_MAX_CHARS", 1400); len(text) > limit {
		text = textutil.Prefix(text, limit) + " […]"
	}
	s.notices = append(s.notices, notice{text, urgent, time.Now()})
	return true
}

// Feed takes 16 kHz microphone samples (about every 20 ms).
func (s *Session) Feed(samples []int16) {
	if s.Request().Phone {
		return // a call's audio comes from the line (FeedLine)
	}
	s.mu.Lock()
	if s.state == Idle || s.state == Closing || s.warm {
		s.mu.Unlock()
		return
	}
	pcm := s.up.Process(samples, nil)
	s.feedLocked(pcm) // unlocks
}

func (s *Session) feed24(pcm []int16) {
	s.mu.Lock()
	if s.state == Idle || s.state == Closing {
		s.mu.Unlock()
		return
	}
	s.feedLocked(pcm)
}

// measureAck (s.mu held): logs once, 4 s after the clip was queued, when the microphone heard
// something loud relative to it and to the mute window.
func (s *Session) measureAck(peak int) {
	if s.ackQueued.IsZero() {
		return
	}
	since := time.Since(s.ackQueued)
	if since < 4*time.Second {
		if peak >= voicePeak {
			if s.ackLoudFirst.IsZero() {
				s.ackLoudFirst = time.Now()
			}
			s.ackLoudLast = time.Now()
		}
		return
	}
	if s.ackLoudFirst.IsZero() {
		log.Printf("[Ack] microphone: nothing loud within 4 s of the clip (muted %d ms)", s.ackUntil.Sub(s.ackQueued).Milliseconds())
	} else {
		log.Printf("[Ack] microphone loud from %d to %d ms after the clip was queued, muted until %d ms",
			s.ackLoudFirst.Sub(s.ackQueued).Milliseconds(), s.ackLoudLast.Sub(s.ackQueued).Milliseconds(),
			s.ackUntil.Sub(s.ackQueued).Milliseconds())
	}
	s.ackQueued = time.Time{}
}

// feedLocked queues 24 kHz audio and sends whole chunks while live. Called with s.mu held;
// releases it.
func (s *Session) feedLocked(pcm []int16) {
	peak := 0
	for _, v := range pcm {
		peak = max(peak, int(v), -int(v))
	}
	// A decided start (Request.Audio ends with a second of quiet): the quiet while connecting
	// would only delay the answer; from the first voice on everything goes out.
	if s.skipQuiet && s.state != Live {
		if peak < voicePeak {
			s.mu.Unlock()
			return
		}
		s.skipQuiet = false
	}
	acking := time.Now().Before(s.ackUntil)
	s.measureAck(peak)
	if peak >= voicePeak && !acking {
		s.voiceMs += len(pcm) * 1000 / audio.SpeakerRate
		s.lastVoice = time.Now()
	}
	// The clip mutes the microphone for GPT Live too: the DSP does not cancel it fully, GPT Live
	// heard "Yeah, sir" and answered its own "Ja, Sir?" with "Was kann ich für Sie tun?" before
	// the user's question (18.10.2026). It only plays when the user is quiet anyway.
	gated := acking || s.echoGate.Load() && s.out().Speaking(echoTail)
	start := len(s.pending)
	// The clip's frames are left out, not sent as zeros: the clip now plays while the session
	// connects, and its 2-3 s of digital silence at the start made GPT Live transcribe "Ja" from
	// nobody and answer it (19.10.2026; the microphone heard the clip only inside the window).
	if !acking {
		s.pending = append(s.pending, pcm...)
	}
	if gated && !acking {
		for i := start; i < len(s.pending); i++ {
			s.pending[i] = 0
		}
	}
	if len(s.pending) > maxPreroll {
		s.pending = append(s.pending[:0], s.pending[len(s.pending)-maxPreroll:]...)
	}
	var chunks [][]int16
	if s.state == Live {
		for len(s.pending) >= micChunk {
			chunks = append(chunks, append([]int16(nil), s.pending[:micChunk]...))
			s.pending = s.pending[micChunk:]
		}
		s.pending = append([]int16(nil), s.pending...)
	}
	gen := s.micGen
	commit := s.rtCommitNowLocked() // Realtime: the turn ends behind this audio (realtime.go)
	s.mu.Unlock()
	for _, c := range chunks {
		select {
		case s.micQ <- micPacket{gen: gen, pcm: c}:
		default:
			log.Print("[Live] microphone queue full, audio dropped")
		}
	}
	if commit {
		s.micQ <- micPacket{gen: gen, commit: true}
	}
}

type micPacket struct {
	gen    int
	pcm    []int16
	commit bool // Realtime: the user's turn ends here (realtime.go)
}

// micSender sends the microphone audio as GPT Live takes it: "no more than 1.2x real-time speed
// with bursts of at most 5 seconds", else it drops the excess. What was said before the session
// (the first words, which the device decided about itself, plus the connection) goes out as a
// burst of up to micBurst, the rest at 1.1x; live audio, once caught up, goes out at once. In
// one burst GPT Live heard only "Hey" of "Hey Jarvis, spiel was von Deichkind" (16.10.2026); at
// 1.5x the 9 s of a decided question were dropped in part and the answer came after 12 s
// (28.10.2026).
const micBurst = 3500 * time.Millisecond // credit: 3.85 s of audio at once

func (s *Session) micSender() {
	const chunkTime = time.Second * micChunk / audio.SpeakerRate
	const step = chunkTime * 10 / 11 // 1.1x real time
	var next time.Time
	for p := range s.micQ {
		s.mu.Lock()
		current := p.gen == s.micGen && s.state == Live
		s.mu.Unlock()
		if !current {
			continue
		}
		if p.commit {
			s.rtCommit() // Realtime: after the audio of the turn
			continue
		}
		now := time.Now()
		if next.Before(now.Add(-micBurst)) {
			next = now.Add(-micBurst) // the burst's credit, never more
		}
		if wait := next.Sub(now); wait > 0 {
			time.Sleep(wait)
		}
		next = next.Add(step)
		s.sendMic(p.pcm)
	}
}

func (s *Session) sendMic(pcm []int16) {
	if s.realtime() {
		s.rtAudio(pcm)
		return
	}
	raw := make([]byte, len(pcm)*2)
	for i, v := range pcm {
		binary.LittleEndian.PutUint16(raw[i*2:], uint16(v))
	}
	const prefix = `{"type":"session.input_audio.append","audio":"`
	msg := make([]byte, 0, len(prefix)+base64.StdEncoding.EncodedLen(len(raw))+2)
	msg = append(msg, prefix...)
	msg = base64.StdEncoding.AppendEncode(msg, raw)
	msg = append(msg, '"', '}')
	s.sendRaw(msg)
}

// Close ends the conversation politely (session.close, then wait for session.closed).
func (s *Session) Close() {
	s.mu.Lock()
	state := s.state
	if state == Connecting {
		s.mu.Unlock()
		s.finish()
		return
	}
	if state != Starting && state != Live {
		s.mu.Unlock()
		return
	}
	s.setStateLocked(Closing)
	s.mu.Unlock()
	s.send(map[string]any{"type": "session.close"})
}

// Abandon: the conversation ends at once, without the server's goodbye: another Echo heard the
// wake word louder and answers (peers.go); this one has not said anything yet.
func (s *Session) Abandon() {
	log.Print("[Live] abandoned: another device answers")
	s.out().Flush()
	s.finish()
}

func (s *Session) fail(message string) {
	log.Printf("[Error] %s", message)
	if s.hooks.OnError != nil {
		go s.hooks.OnError(message)
	}
	if !s.Open() {
		return
	}
	s.out().Flush()
	if !s.Request().Phone { // beeps at home only: on the phone the caller hears the line
		s.speaker.Tone(220, 300, 0.2)
	}
	s.finish()
}

func (s *Session) finish() {
	s.mu.Lock()
	if s.state == Idle {
		s.mu.Unlock()
		return
	}
	s.words.Flush()
	conn, cancel := s.conn, s.cancel
	s.conn, s.cancel = nil, nil
	if s.held {
		log.Printf("[Hold] conversation over while held: %d audio packets, %d calls dropped", len(s.heldAudio), len(s.heldCalls))
	}
	s.held, s.heldAudio, s.heldCalls = false, nil, nil
	end := Ended{Request: s.req, Started: s.startedAt, Seconds: s.usage, HeardUser: s.heardUser,
		Silent: s.silentEnd, Tools: strings.Join(s.toolLog, "\n"), ClosedBy: s.closedBy}
	s.toolLog, s.closedBy = nil, ""
	dropped := s.notices
	s.notices = nil
	s.setStateLocked(Idle)
	s.mu.Unlock()
	if conn != nil {
		conn.Close(websocket.StatusNormalClosure, "")
	}
	if cancel != nil {
		cancel()
	}
	for _, n := range dropped {
		log.Printf("[Notice] conversation over, not told: %s", n.text)
	}
	end.Transcript = s.words.Full()
	log.Printf("[Session ended] %d s billed, speaker underruns so far %d", end.Seconds, s.speaker.Underruns())
	if s.hooks.OnEnd != nil && (end.Transcript != "" || end.Request.Phone) {
		go s.hooks.OnEnd(end)
	}
	if s.hooks.OnOver != nil {
		go s.hooks.OnOver(end)
	}
}
