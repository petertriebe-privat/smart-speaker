package phone

import (
	"context"
	"echo-assistant/internal/prompts"
	"fmt"
	"log"
	"sync/atomic"
	"time"

	"echo-assistant/internal/textutil"
)

// A call on the user's mobile phone, held at the Echo over Bluetooth (issue #51): the Echo is the
// phone's hands-free unit. A call rings at home, softly, with the caller's name (mobile_ring.go);
// answered (by voice, the action button or at the phone itself) it is the same call on the
// speaker as in #49 - the bridge, the gate, the cap on the backlog, the announcement and the ring
// - only the line is hw:0,2 instead of RTP. The user can also call out over the phone
// (mobile_dial.go).
//
// Whether the phone's calls come here is the user's choice, made at the phone: the switch for
// calls in its Bluetooth settings, which the phone remembers and acts on when it reconnects
// (auto, the default - #51: "HFP verbunden = Jarvis darf das Handy nutzen"). mobile_handsfree
// overrides it by voice: on connects from here, off keeps oFono's modem unpowered, so BlueZ
// turns the profile down and the phone keeps its calls. Music (A2DP) is not affected either way.
// A call the user makes over the phone powers it for that call only.

var (
	mobilePoll       = ms("MOBILE_POLL_MS", 500)          // how often oFono is asked
	mobilePowerRetry = ms("MOBILE_POWER_RETRY_MS", 60000) // hands-free wanted, the phone not in reach
)

// mobileState: the phone's call at the Echo; under Phone.mu.
type mobileState struct {
	mode    string // mobile_handsfree: "" (auto: the phone's own switch decides), "on", "off"
	oneCall bool   // hands-free for one call the user makes over the phone, off again after it
	release bool   // that call is over: switch hands-free off once (auto mode)

	autoAnswer bool // the echo test: the Echo answers the phone's call itself
	answering  bool // ... and did so for this call
	gone       bool // oFono did not answer the last poll (logged once)
	online     bool // a powered modem has its service level connection
	paired     bool // oFono knows a phone (it is paired)

	powerRetry time.Time // powering failed: not before this
	powerFails int

	dial *mobileDial // a call to make over the phone once it is connected

	modem, call  string // the call being handled, "" = none
	state        string // its last oFono state
	number, name string
	outgoing     bool // dialled (at the phone or by dial_for_me), not incoming
	rings        int
	ringAt       time.Time
	noted        bool    // the conversation at the speaker was told that it rings
	audio        scoLine // while the call is on the speaker
	line         *mobileLine
	since        time.Time
}

// mobileLine: the sample rate steps of one mobile call, each used by one goroutine, and what
// flowed (a line in the log every mobileStatsEvery while the call runs).
type mobileLine struct {
	up   Upsampler    // the far end 8 -> 24 kHz (the SCO reader)
	mic  MicResampler // the room 16 -> 24 kHz (the microphone)
	down Downsampler  // ... and 24 -> 8 kHz

	in, out, gated, inPeak, outPeak atomic.Int64 // frames from the phone / to it, of those gated; peaks
	statsAt                         time.Time    // the poll's goroutine only
}

var mobileStatsEvery = ms("MOBILE_STATS_MS", 10000)

func peak16(pcm []int16) int64 {
	m := 0
	for _, v := range pcm {
		m = max(m, int(v), -int(v))
	}
	return int64(m)
}

func keepMax(a *atomic.Int64, v int64) {
	for {
		old := a.Load()
		if v <= old || a.CompareAndSwap(old, v) {
			return
		}
	}
}

// stats: the log line about the last mobileStatsEvery, then the counters start over.
func (l *mobileLine) stats(b *bridge, audio scoLine) string {
	b.mu.Lock()
	queued := len(b.call) * 1000 / apiRate
	b.mu.Unlock()
	speaker := "?"
	if q, ok := b.out().(interface{ Queued() time.Duration }); ok {
		speaker = q.Queued().Round(10 * time.Millisecond).String()
	}
	lead, dropped := audio.Lead()
	return fmt.Sprintf("in %d frames (peak %d), out %d (peak %d, gated %d, %d dropped, lead %s), room queue %d ms, speaker %s",
		l.in.Swap(0), l.inPeak.Swap(0), l.out.Swap(0), l.outPeak.Swap(0), l.gated.Swap(0), dropped,
		lead.Round(time.Millisecond), queued, speaker)
}

// EnableMobile switches the mobile phone on: oFono over the system bus. Call it before AddTools
// and RunMobile, on the Echo only.
func (p *Phone) EnableMobile() {
	p.hf = &ofono{}
	p.newSCO = newSCO
	p.loadMobileMode()
}

// RunMobile asks oFono every mobilePoll and runs the phone's call until ctx ends. It does
// nothing while ofonod is not there (Bluetooth not up).
func (p *Phone) RunMobile(ctx context.Context) {
	if p.hf == nil {
		return
	}
	ticker := time.NewTicker(mobilePoll)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			p.mobileEnd(prompts.Name() + " stops")
			return
		case now := <-ticker.C:
			p.pollMobile(now)
		}
	}
}

// pollMobile: one look at oFono. The D-Bus calls run outside the lock; mobileStep decides.
func (p *Phone) pollMobile(now time.Time) {
	modems, err := p.hf.Modems()
	p.mu.Lock()
	force, want := p.mob.powerWanted()
	wasGone := p.mob.gone
	p.mob.gone = err != nil
	p.mob.paired = err == nil && len(modems) > 0
	p.mu.Unlock()
	if err != nil {
		if !wasGone {
			log.Printf("[Mobile] oFono not reachable: %v", err)
		}
		p.mobileEnd("oFono is gone")
		p.mobileDialStep(now, "", false)
		return
	}
	if wasGone {
		log.Printf("[Mobile] oFono is there, %d phone(s)", len(modems))
	}
	var current hfCall
	modem, online, first := "", false, ""
	for _, m := range modems {
		if force && m.Powered != want {
			p.mobilePower(now, m.Path, want)
			continue
		}
		if !m.Powered || !m.Online {
			continue
		}
		online = true
		if first == "" {
			first = m.Path
		}
		if current.Path != "" {
			continue
		}
		calls, err := p.hf.Calls(m.Path)
		if err != nil {
			continue
		}
		if c, ok := mobileCurrent(calls); ok {
			current, modem = c, m.Path
		}
	}
	p.mu.Lock()
	p.mob.online = online
	p.mob.release = false // one poll switches it off; a connection the phone makes later is its own
	p.mu.Unlock()
	p.mobileDialStep(now, first, online)
	p.mobileStep(now, modem, current)
	p.mu.Lock()
	line, audio := p.mob.line, p.mob.audio
	p.mu.Unlock()
	if line != nil && audio != nil {
		if line.statsAt.IsZero() {
			line.statsAt = now
		} else if now.Sub(line.statsAt) >= mobileStatsEvery {
			line.statsAt = now
			log.Printf("[Mobile] %s", line.stats(p.bridge, audio))
		}
	}
}

// mobilePower switches a modem the way the user wants it. Powering connects HFP, and with the
// phone out of reach oFono answers "Operation failed" after some seconds of paging (17.10.2026):
// then not again for mobilePowerRetry - paging every half second would hold the poll, fill the
// log and keep the radio busy that the WiFi shares. Switching off is always done at once.
func (p *Phone) mobilePower(now time.Time, modem string, on bool) {
	p.mu.Lock()
	wait := on && now.Before(p.mob.powerRetry)
	p.mu.Unlock()
	if wait {
		return
	}
	err := p.hf.SetPowered(modem, on)
	p.mu.Lock()
	defer p.mu.Unlock()
	if err == nil {
		p.mob.powerRetry, p.mob.powerFails = time.Time{}, 0
		log.Printf("[Mobile] %s powered=%v", modem, on)
		return
	}
	if p.mob.powerFails == 0 {
		log.Printf("[Mobile] %s powered=%v: %v (the phone out of reach? again every %s)", modem, on, err, mobilePowerRetry)
	}
	p.mob.powerFails++
	p.mob.powerRetry = time.Now().Add(mobilePowerRetry)
}

// mobileCurrent: the call that matters. One that is talking or being dialled comes before one
// that only rings: a second call knocking ("waiting") must not push the running one off the
// speaker.
func mobileCurrent(calls []hfCall) (hfCall, bool) {
	var ringing hfCall
	for _, c := range calls {
		switch c.State {
		case "active", "held", "dialing", "alerting":
			return c, true
		case "incoming", "waiting":
			if ringing.Path == "" {
				ringing = c
			}
		}
	}
	return ringing, ringing.Path != ""
}

// mobileStep moves the call on to what oFono says.
func (p *Phone) mobileStep(now time.Time, modem string, c hfCall) {
	p.mu.Lock()
	defer p.mu.Unlock()
	m := &p.mob
	if c.Path == "" {
		if m.call != "" {
			p.mobileEndLocked("the call is over")
		}
		return
	}
	if c.Path != m.call {
		if m.call != "" {
			p.mobileEndLocked("another call")
		}
		m.modem, m.call, m.state, m.since = modem, c.Path, "", now
		m.number = textutil.PhoneNormalized(c.Number)
		m.name, m.rings, m.ringAt, m.noted = p.mobileCaller(m.number), 0, time.Time{}, false
		m.outgoing = c.State == "dialing" || c.State == "alerting"
		log.Printf("[Mobile] call %s, %s (%s)", c.State, m.number, m.name)
		p.mobileDuck(true)
	}
	before := m.state
	if c.State != m.state {
		log.Printf("[Mobile] %s -> %s", m.state, c.State)
		m.state = c.State
	}
	switch c.State {
	case "incoming", "waiting":
		if m.autoAnswer {
			if !m.answering {
				m.answering = true
				go p.hf.Answer(c.Path)
			}
			return
		}
		p.mobileRingLocked(now)
	case "dialing", "alerting", "active", "held":
		// Dialling from the phone brings its ringback over SCO, which belongs in the room; the
		// phone's own ring tone of an incoming call (in-band ringing) never does, so the room is
		// connected only from here on, not from the moment SCO opens.
		if m.audio == nil {
			p.mobileConnectLocked()
		} else if c.State == "active" && (before == "dialing" || before == "alerting") {
			p.bridge.announce(true) // the other side picked up: two tones, as with the landline
		}
	}
}

// mobileCaller: the household's name for a number, else the address book's, else "".
func (p *Phone) mobileCaller(number string) string {
	if number == "" {
		return ""
	}
	if id := p.people.ByPhone(number); id != "" {
		return p.people.Name(id)
	}
	if p.deps.CallerName != nil {
		return p.deps.CallerName(number)
	}
	return ""
}

// mobileConnectLocked: the call is answered (or dialled): onto the speaker and the microphone at
// home, both ways. Not while the landline has the speaker - one call in the room.
func (p *Phone) mobileConnectLocked() {
	m := &p.mob
	if p.deps.Speaker == nil || p.newSCO == nil {
		return
	}
	if p.call.stage != stageNone && p.bridge.Mode().on() {
		if m.since.Add(time.Second).After(time.Now()) {
			log.Print("[Mobile] the landline has the speaker: this call stays silent here")
		}
		return
	}
	line := &mobileLine{}
	audio := p.newSCO()
	err := audio.Start(func(pcm8 []int16) {
		line.in.Add(1)
		keepMax(&line.inPeak, peak16(pcm8))
		pcm24 := line.up.Process(pcm8)
		p.bridge.Call(pcm24)
		p.confFar(pcm24) // The assistant in the call hears the far end (conference.go)
	})
	if err != nil {
		log.Printf("[Mobile] SCO audio: %v", err)
		return
	}
	m.audio, m.line = audio, line
	// The tones say "you can talk now": at once for an answered call, at the pick-up for one
	// being dialled (its ringback plays meanwhile).
	if p.bridge.setMode(modeTalk) && (m.state == "active" || m.state == "held") {
		p.bridge.announce(true)
	}
	log.Printf("[Mobile] %s is on the speaker at home", m.number)
}

// mobileEnd / mobileEndLocked: the call is over at the Echo; the speaker and the microphone go
// back to normal, the room hears the closing tone if it heard the call.
func (p *Phone) mobileEnd(why string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.mobileEndLocked(why)
}

func (p *Phone) mobileEndLocked(why string) {
	m := &p.mob
	if m.call == "" {
		return
	}
	p.endConferenceLocked("the call is over")
	if m.audio != nil {
		m.audio.Stop()
		if p.call.stage == stageNone { // the speaker was this call's, not the landline's
			p.bridge.setMode(modeOff)
			p.bridge.announce(false)
		}
		log.Printf("[Mobile] %s ended after %s: %s", m.number, time.Since(m.since).Round(time.Second), why)
	} else if !m.outgoing {
		log.Printf("[Mobile] %s rang %d time(s), not answered here: %s", m.number, m.rings, why)
	}
	p.mobileDuck(false)
	// A call made over the phone for itself only: hands-free goes off with it.
	release := m.oneCall && m.mode == ""
	*m = mobileState{mode: m.mode, online: m.online, gone: m.gone, paired: m.paired,
		powerRetry: m.powerRetry, powerFails: m.powerFails, dial: m.dial, release: release, autoAnswer: m.autoAnswer}
}

// powerWanted: whether the assistant switches the modem (force) and to what. In auto mode the phone's
// own "calls" switch decides: it connects hands-free itself when the user turned it on, and
// remembers that - forcing the modem off there broke it (17.10.2026: four times the phone
// connected on coming home, four times the assistant cut it, and the phone gave up). A call for itself
// only powers it, and switches it off once after.
func (m *mobileState) powerWanted() (force, on bool) {
	switch {
	case m.oneCall:
		return true, true
	case m.mode == "on":
		return true, true
	case m.mode == "off":
		return true, false
	case m.release:
		return true, false
	}
	return false, false
}

// mobileDuck: the music goes down while the phone rings or a call on it runs, and back up after.
func (p *Phone) mobileDuck(on bool) {
	if p.deps.Duck != nil {
		go p.deps.Duck(on)
	}
}

// hearMobile: the microphone at home onto the mobile call (from Hear). True if there is one.
func (p *Phone) hearMobile(pcm []int16) bool {
	p.mu.Lock()
	audio, line := p.mob.audio, p.mob.line
	p.mu.Unlock()
	if audio == nil {
		return false
	}
	gated := p.bridge.echo.active()
	room := line.mic.Process(p.mic.process(pcm, gated))
	p.mu.Lock()
	conf := p.conf
	p.mu.Unlock()
	if conf != nil && conf.out != nil { // The assistant in the call: its voice onto the line with the room
		voice := conf.out.take(len(room))
		for i := range room {
			room[i] = clamp16(float32(room[i]) + float32(voice[i]))
		}
	}
	out := line.down.Process(room)
	line.out.Add(1)
	if gated {
		line.gated.Add(1)
	}
	keepMax(&line.outPeak, peak16(out))
	audio.Write(out)
	return true
}

// MobileRinging: a call on the phone rings at home (the LED ring shows it from the first second).
func (p *Phone) MobileRinging() bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.mob.call != "" && p.mob.audio == nil && !p.mob.outgoing
}

// MobileBusy: the phone rings or a call on it runs (the music stays down meanwhile).
func (p *Phone) MobileBusy() bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.mob.call != "" || p.mob.dial != nil
}

// MobileActive: a call on the phone is on the speaker at home.
func (p *Phone) MobileActive() bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.mob.audio != nil
}

// mobileOnline: the phone's hands-free connection stands - the user is at home with it.
func (p *Phone) mobileOnline() bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.hf != nil && p.mob.online
}

// MobileButton: the action button during a call on the phone: answers while it rings, hangs up
// while it is on the speaker. "" if there is no such call (the button then does its usual work).
func (p *Phone) MobileButton() string {
	p.mu.Lock()
	call, audio := p.mob.call, p.mob.audio
	p.mu.Unlock()
	switch {
	case call == "":
		return ""
	case audio == nil:
		return p.mobileAction("answer")
	}
	return p.mobileAction("hangup")
}

// mobileAction: answer, reject or hangup the phone's call; the answer is for a tool.
func (p *Phone) mobileAction(action string) string {
	p.mu.Lock()
	call, state := p.mob.call, p.mob.state
	p.mu.Unlock()
	if p.hf == nil {
		return "Error: no mobile phone on this device."
	}
	if call == "" {
		return "Error: no call on the mobile phone."
	}
	var err error
	switch action {
	case "answer":
		if state != "incoming" && state != "waiting" {
			return "The call is already answered."
		}
		err = p.hf.Answer(call)
	case "reject", "hangup":
		err = p.hf.Hangup(call)
	default:
		return "Error: action must be answer, reject or hangup."
	}
	if err != nil {
		return "Error: " + err.Error()
	}
	log.Printf("[Mobile] %s", action)
	if action == "answer" {
		return "Answered: the call is on the speaker now."
	}
	return "Ended."
}
