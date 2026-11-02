package phone

import (
	"log"
	"sync"
	"time"

	"echo-assistant/internal/audio"
	"echo-assistant/internal/prompts"
)

// The call on the speaker and the microphone at home (issue #49, the Echo only: the M5 has no
// working echo cancellation, see devices/atoms3r/docs/architecture.md). Two directions that can be
// switched on separately:
//
//   - ToRoom: everything on the line - the other side and the assistant - is audible at home. The room
//     direction is mixed and paced here: the caller's audio arrives with the RTP packets, the assistant's
//     voice in bursts from GPT Live, so a 20 ms ticker takes one frame of each, adds them and
//     hands it to the speaker (whose own jitter buffer absorbs the rest).
//   - ToLine: the microphone at home goes onto the line (Line.MicIn), so whoever is at home takes
//     part in the call. Only sound on the Echo: the DSP delivers the microphone echo-cancelled,
//     so the far end does not hear itself.
//
// While a conversation at the speaker runs, both directions pause: The assistant's answer and the call
// would otherwise write into the same speaker, and the far end would hear what is said to the assistant.
type bridgeMode struct {
	ToRoom bool
	ToLine bool
}

// The four ways a call and the room are connected. listen/talk is a call the assistant makes (listening
// in, then taking part); monitor/talk is the owner dialling in from outside (hearing the room,
// then the intercom).
var (
	modeOff     = bridgeMode{}
	modeListen  = bridgeMode{ToRoom: true}
	modeTalk    = bridgeMode{ToRoom: true, ToLine: true}
	modeMonitor = bridgeMode{ToLine: true}
)

func (m bridgeMode) on() bool { return m.ToRoom || m.ToLine }

// name: the mode in a log line and in a tool's answer.
func (m bridgeMode) name() string {
	switch m {
	case modeListen:
		return "listen"
	case modeTalk:
		return "talk"
	case modeMonitor:
		return "monitor"
	}
	return "off"
}

// modeByName: the modes a tool may ask for.
func modeByName(name string) (bridgeMode, bool) {
	for _, m := range []bridgeMode{modeOff, modeListen, modeTalk, modeMonitor} {
		if m.name() == name {
			return m, true
		}
	}
	return modeOff, false
}

// Speaker: what a call needs of the speaker at home (*audio.Speaker).
type Speaker interface {
	Queue(pcm []int16) error
	Tone(freq float64, ms int, amp float64)
	Flush()                // drops what is queued: the ringback once the other side picks up
	Queued() time.Duration // still to be played of what was queued (pacat's pipe and buffer)
}

// speakerMax: the speaker holds at most this much of the call; frames beyond it are dropped.
// The room queue alone did not keep the call live: during a conversation with the assistant in the
// middle of a call his answer went into the same pipe, and afterwards the call ran 1.5 s late
// for good - too late for the gate too, which then let the caller's own voice back out
// (17.10.2026).
var speakerMax = ms("PHONE_SPEAKER_MAX_MS", 200)

var (
	bridgePrefill = prompts.Int("PHONE_BRIDGE_PREFILL_MS", 40) * apiRate / 1000 // against RTP jitter
	// The other side queued at most; older audio is dropped. The queue drains exactly as fast as
	// RTP fills it, so whatever piles up once (the announcement, a conversation at the speaker, a
	// ticker that fell behind while the speaker's pipe blocked) would stay as delay for the rest
	// of the call - with the old cap of a second, the room heard the call a second late.
	bridgeLag = max(prompts.Int("PHONE_BRIDGE_LAG_MS", 120)*apiRate/1000, bridgePrefill+apiFrame)
	// The assistant's voice kept at most: GPT Live sends in bursts faster than real time, and in a
	// conference whole answers go this way - as much as the line itself keeps (maxQueued).
	bridgeMax = maxQueued
)

// announceTone: how long one tone of the announcement keeps the speaker busy - audio.Speaker
// appends 2 x its jitter buffer of silence to a tone, so that PulseAudio starts the stream at all.
const announceTone = 700 * time.Millisecond

// The ringback at home while a call the user makes themselves rings at the other end: the German
// cadence, a second of 425 Hz every five, quieter than the assistant's voice.
const (
	ringbackFreq = 425.0
	ringbackMs   = 1000
	ringbackAmp  = 0.10
)

var ringbackEvery = ms("PHONE_RINGBACK_MS", 5000)

// The system notes to the model, English as all of them. Defaults here, not in the M5's
// prompts.h: the M5 cannot do any of this (no echo cancellation at the speaker, issue #3), and a
// shared prompt must not promise a device something it lacks.
const (
	bridgeListening = "System note, not from the user: from now on this call is audible on the speaker at " +
		"home and someone there is listening in. Tell the other side so in one short sentence (§ 201 StGB) as soon " +
		"as a person is on the line, then carry on where you stopped. Not to a mailbox or an announcement: never " +
		"talk into those."
	bridgeJoined = "System note, not from the user: the user at home has joined this call over the speaker - " +
		"a three-way call now. You hear the other side and the user at once, and both hear you. Stay in the call " +
		"with your task, but let the user speak: they may step in, correct or decide something. Answer when " +
		"addressed or when it is your turn; do not repeat what the user said."
	bridgeLeft = "System note, not from the user: the user at home has left the call, you are talking to the " +
		"other side alone again. Go on from where the three of you were, with what the user said or agreed."
)

// bridge: the room direction of a call. The zero value is off; without a speaker it stays off.
type bridge struct {
	speaker Speaker     // where the room direction plays (another Echo's while the room is there, remote.go)
	local   Speaker     // this Echo's speaker
	gone    func()      // the room was another Echo's and is off now: tell that one
	busy    func() bool // a conversation at the speaker runs (then the room belongs to it)

	spec *audio.Spectrum // what the room hears, for the LED ring
	echo roomEcho        // when it was last loud in the room, for the gate on the way out

	mu         sync.Mutex
	mode       bridgeMode
	call       []int16   // the other side, 24 kHz, not played yet
	assistant  []int16   // The assistant's voice, 24 kHz, not played yet
	playing    bool      // the prefill is through
	quietUntil time.Time // the announcement is playing: the call waits, one voice at a time
	lastFrame  time.Time // last frame handed to the speaker (the ring goes dark without them)
	stop       chan struct{}
}

func newBridge(speaker Speaker, busy func() bool) *bridge {
	return &bridge{speaker: speaker, local: speaker, busy: busy, spec: audio.NewSpectrum(apiRate, audio.PhoneBands)}
}

// Spectrum: the bands of what the call plays in the room, bass first, all 0 while it plays
// nothing - the ring shows the voices of the call like the music of Spotify.
func (b *bridge) Spectrum() [audio.SpectrumBands]float64 {
	if b == nil {
		return [audio.SpectrumBands]float64{}
	}
	b.mu.Lock()
	quiet := time.Since(b.lastFrame) > 300*time.Millisecond
	b.mu.Unlock()
	if quiet {
		return [audio.SpectrumBands]float64{}
	}
	return b.spec.Levels()
}

// Mode: how the call and the room are connected right now.
func (b *bridge) Mode() bridgeMode {
	if b == nil {
		return modeOff
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.mode
}

// setMode connects the directions of mode; the room direction gets its ticker while it is on.
// False if there is no speaker to play on.
func (b *bridge) setMode(mode bridgeMode) bool {
	if b == nil || b.speaker == nil {
		return false
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if mode == b.mode {
		return true
	}
	b.mode = mode
	b.call, b.assistant, b.playing = nil, nil, false
	if !mode.on() && b.speaker != b.local { // the room comes home with the end
		b.speaker = b.local
		if gone := b.gone; gone != nil {
			b.gone = nil
			go gone()
		}
	}
	switch {
	case mode.ToRoom && b.stop == nil:
		b.stop = make(chan struct{})
		go b.play(b.stop)
	case !mode.ToRoom && b.stop != nil:
		close(b.stop)
		b.stop = nil
	}
	log.Printf("[Bridge] %s", mode.name())
	return true
}

// announce: a call is on the speaker now, or it is not any more. Two tones up for the start, one
// down for the end - without a GPT Live session there is no voice to say it, and the LED ring
// alone is nothing for a room nobody looks at. The call waits as long as the tones play: the
// speaker takes one source at a time (the room hears the call from the moment the tones end,
// not delayed - what piles up meanwhile is dropped).
func (b *bridge) announce(on bool) {
	if b == nil || b.speaker == nil {
		return
	}
	type tone struct {
		freq float64
		ms   int
	}
	tones := []tone{{660, 120}, {880, 160}}
	if !on {
		tones = []tone{{440, 180}}
	}
	b.mu.Lock()
	b.quietUntil = time.Now().Add(time.Duration(len(tones)) * announceTone)
	speaker := b.speaker
	b.mu.Unlock()
	for _, t := range tones {
		speaker.Tone(t.freq, t.ms, 0.15)
	}
}

// Call takes the other side's audio (24 kHz, from the line's receiving goroutine).
func (b *bridge) Call(pcm []int16) {
	if b == nil {
		return
	}
	b.keep(&b.call, pcm, bridgeLag)
}

// The assistant takes its voice for the room (24 kHz, from Line.Queue: bursts of a few hundred ms).
func (b *bridge) Assistant(pcm []int16) {
	if b == nil {
		return
	}
	b.keep(&b.assistant, pcm, bridgeMax)
}

// keep appends to a queue of the room direction while it is on. A queue that runs over limit
// keeps the newest samples: the room hears the call live, late audio is worth nothing.
func (b *bridge) keep(queue *[]int16, pcm []int16, limit int) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if !b.mode.ToRoom {
		return
	}
	*queue = append(*queue, pcm...)
	if over := len(*queue) - limit; over > 0 {
		*queue = (*queue)[:copy(*queue, (*queue)[over:])]
	}
}

// --- What the call and the room do with each other --------------------------------------------

// Bridge: how the running call and the room at home are connected ("off", "listen", "talk",
// "monitor").
func (p *Phone) Bridge() string { return p.bridge.Mode().name() }

// BridgeActive: a call is on the speaker or the microphone at home. The wake word then listens
// although the speaker plays (the DSP takes the call out of the microphone again), and the ring
// shows the call.
func (p *Phone) BridgeActive() bool { return p.bridge.Mode().on() }

// Dialling: a call the user makes themselves rings at the other end. Nothing of the call is
// audible yet (RTP starts with the pick-up), so the ring and the ringback are all they have.
func (p *Phone) Dialling() bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.call.direct && p.call.stage == stageDialing || p.mobileDiallingLocked()
}

// Spectrum: the bands of the call in the room, for the LED ring (all 0 when nothing plays).
func (p *Phone) Spectrum() [audio.SpectrumBands]float64 { return p.bridge.Spectrum() }

// ringback: while a call the user makes themselves rings at the other end, the speaker at home
// rings along - 425 Hz, a second on and four off, as a German exchange. Without it they stand in
// silence and cannot tell whether anything is happening. Not while a conversation at the speaker
// is still running (it would talk into it), and in a goroutine: writing a second of audio into
// the speaker's pipe can block, and the loop must not.
func (p *Phone) ringback(now time.Time) {
	c := &p.call
	if p.deps.Speaker == nil || p.deps.HomeBusy != nil && p.deps.HomeBusy() {
		return
	}
	if !c.ringbackAt.IsZero() && now.Sub(c.ringbackAt) < ringbackEvery {
		return
	}
	c.ringbackAt = now
	speaker := p.deps.Speaker
	go speaker.Tone(ringbackFreq, ringbackMs, ringbackAmp)
}

// Hear takes the microphone at home (16 kHz, every 20 ms) and puts it on the line while someone
// at home takes part in the call. On the way it gets the gain control and the gate (mic.go):
// the DSP's echo cancellation is made for a model listening in the same room, not for a person
// half a second away on a telephone. Nothing to do in every other case.
func (p *Phone) Hear(pcm []int16) {
	if p.bridge.remote() {
		return // the room is another Echo's: its microphone comes in through HearRemote
	}
	p.hear(pcm)
}

func (p *Phone) hear(pcm []int16) {
	if !p.bridge.Mode().ToLine {
		return
	}
	if p.deps.HomeBusy != nil && p.deps.HomeBusy() {
		return // a conversation at the speaker: that one gets the microphone, not the call
	}
	p.confHear(pcm) // The assistant in the call hears the room too (conference.go)
	if p.hearMobile(pcm) {
		return // the mobile phone's call has the room (mobile.go)
	}
	p.mu.Lock()
	line := p.call.line
	p.mu.Unlock()
	if line != nil {
		line.MicIn(p.mic.process(pcm, p.bridge.echo.active()))
	}
}

// SetBridge connects the running call with the room at home, asked for from home (the tool
// speaker_call or the action button): "listen" puts the call on the speaker, "talk" adds the
// microphone, "off" ends it - and ends a call that is nothing but the bridge. The answer is for
// the tool.
func (p *Phone) SetBridge(name string) string {
	mode, ok := modeByName(name)
	if !ok {
		return "Error: mode must be off, listen or talk."
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.setBridgeLocked(mode)
}

// CycleBridge: the action button during a call. It walks off -> listen -> talk -> off, and ends
// a call that is only the bridge. "" if no call is running (then the button does its usual work).
func (p *Phone) CycleBridge() string {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.call.stage != stageTalking && p.call.stage != stageBridge {
		return ""
	}
	next := modeOff
	switch p.bridge.Mode() {
	case modeOff:
		next = modeListen
	case modeListen:
		next = modeTalk
	}
	return p.setBridgeLocked(next)
}

func (p *Phone) setBridgeLocked(mode bridgeMode) string {
	c := &p.call
	// "Ich will mithören" while the assistant's call still rings or waits to be dialled: from the pick-up,
	// as phone_call's listen (19.10.2026: "no call is running", and the assistant claimed it was over).
	ringing := c.stage == stageDialing && c.outgoing && !c.direct
	if (ringing || c.stage == stageNone && p.wanted != nil && !p.wanted.direct) && mode.on() {
		if ringing {
			c.listen = true
		} else {
			p.wanted.listen = true
		}
		return "The call is not picked up yet; it is audible on the speaker at home from the pick-up and the other " +
			"side is told. To talk along: after the pick-up, speaker_call talk or the action button."
	}
	if c.stage != stageTalking && c.stage != stageBridge {
		return "Error: no call is running."
	}
	if p.deps.Speaker == nil && mode.on() {
		return "Error: this device has no speaker for a call."
	}
	before := p.bridge.Mode()
	if mode == before {
		return "The call is already " + mode.name() + "."
	}
	// A call that is only the bridge (the user dialled in from outside): switching it off ends it
	// (clearCall switches the room off and says so).
	if !mode.on() && c.stage == stageBridge {
		p.hangUp("the bridge was ended at home")
		return "Ended."
	}
	if !p.bridge.setMode(mode) {
		return "Error: this device has no speaker for a call."
	}
	switch {
	case mode.on() && !before.on():
		p.bridge.announce(true)
	case !mode.on():
		p.bridge.announce(false)
	}
	// Someone at home is on the line: a three-way call. The assistant stays in it - it hears the room
	// and the other side mixed (the conference's mix, conference.go) and both hear him - so he
	// knows what the user agreed and goes on from there when they leave again. (The first
	// version held him back, deaf, until they handed back; the user wanted to step in, not to
	// take over, 17.10.2026.)
	if p.session.Open() && p.session.OnPhone() && c.stage == stageTalking {
		switch {
		case mode.ToLine && p.conf == nil:
			p.conf = &conference{since: time.Now(), own: true}
			p.session.Commentary(prompts.TextOr("LIVE_BRIDGE_JOINED", bridgeJoined))
		case !mode.ToLine && p.conf != nil && p.conf.own:
			p.conf = nil
			p.session.Commentary(prompts.TextOr("LIVE_BRIDGE_LEFT", bridgeLeft))
		}
		// The other side has to be told that the call is audible at home (§ 201 StGB), as when
		// listening in without talking along (issue #2).
		if mode.ToRoom && !before.ToRoom {
			p.session.Commentary(prompts.TextOr("LIVE_BRIDGE_LISTENING", bridgeListening))
		}
	}
	return "The call is " + mode.name() + " now."
}

// ConnectSpeaker: the user dialled in from outside and wants the room at home ("monitor": they
// hear the room; "talk": both ways, the intercom). GPT Live is not needed for it and ends - it
// would listen along and cost money for nothing; the call itself stays up as the bridge.
func (p *Phone) ConnectSpeaker(name string) string {
	mode, ok := modeByName(name)
	if !ok || !mode.ToLine {
		return "Error: mode must be monitor or talk."
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	c := &p.call
	switch {
	case c.stage != stageTalking && c.stage != stageBridge:
		return "Error: no call is running."
	case c.outgoing:
		return "Error: only a caller who dialled in can be connected with the speaker at home."
	case !p.maySpeaker(c):
		// By default only the admin: a released number opens a live microphone in the home
		// (speaker_access releases and takes back).
		return "Error: this number is not released for the speaker and the microphone at home."
	case p.deps.Speaker == nil:
		return "Error: this device has no speaker for a call."
	}
	if !p.bridge.setMode(mode) {
		return "Error: this device has no speaker for a call."
	}
	p.bridge.announce(true)
	if c.stage != stageBridge {
		c.stage = stageBridge
		c.bridgeSince = time.Now()
		log.Printf("[Phone] %s is on the speaker at home (%s), the session ends", c.number, mode.name())
	}
	if p.session.Open() {
		p.session.SetPaused(false) // say the last sentence, then go
		p.session.EndSoon()
	}
	answer := "Connected: you hear the microphone at home."
	if mode.ToRoom {
		answer = "Connected both ways: you hear the microphone at home and they hear you on the speaker."
	}
	return answer + " Say in one short sentence that the connection is up and that you are leaving the line now, nothing else."
}

// play: one 20 ms frame of the call into the room, every 20 ms, until the room direction ends.
func (b *bridge) play(stop chan struct{}) {
	ticker := time.NewTicker(20 * time.Millisecond)
	defer ticker.Stop()
	for {
		select {
		case <-stop:
			return
		case <-ticker.C:
		}
		if pcm := b.frame(); pcm != nil {
			speaker := b.out()
			queued := speaker.Queued()
			if queued > speakerMax {
				continue // late audio is worth nothing: let the speaker catch up
			}
			peak := 0
			for _, v := range pcm {
				peak = max(peak, int(v), -int(v))
			}
			// The microphone on the way out is turned down while this plays - from when it plays,
			// which is what the speaker still holds later.
			b.echo.heard(peak, queued)
			b.spec.Feed(pcm) // outside the lock: the analysis takes ~0.3 ms
			speaker.Queue(pcm)
		}
	}
}

// out: where the room direction plays: this Echo's speaker, or another Echo's (remote.go).
func (b *bridge) out() Speaker {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.speaker
}

// frame: the other side plus the assistant of the next 20 ms, nil while nothing is to be played. The
// other side's queue is the clock (a phone sends its packets at a steady rate, silence included):
// playing starts once the prefill is there and waits again whenever it runs dry.
func (b *bridge) frame() []int16 {
	b.mu.Lock()
	defer b.mu.Unlock()
	if !b.mode.ToRoom || b.busy != nil && b.busy() {
		return nil
	}
	if time.Now().Before(b.quietUntil) {
		b.playing = false // the announcement has the speaker; afterwards start with a full prefill
		return nil
	}
	if !b.playing {
		if len(b.call) < bridgePrefill {
			return nil
		}
		b.playing = true
	}
	if len(b.call) == 0 {
		b.playing = false // the line fell silent: prefill again before the next frame
		return nil
	}
	b.lastFrame = time.Now()
	pcm := make([]int16, apiFrame)
	n := copy(pcm, b.call)
	b.call = b.call[:copy(b.call, b.call[n:])]
	if len(b.assistant) > 0 {
		m := min(len(b.assistant), apiFrame)
		for i := 0; i < m; i++ {
			pcm[i] = clamp16(float32(pcm[i]) + float32(b.assistant[i]))
		}
		b.assistant = b.assistant[:copy(b.assistant, b.assistant[m:])]
	}
	return pcm
}
