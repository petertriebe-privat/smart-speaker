package phone

import (
	"echo-assistant/internal/prompts"
	"log"
	"math"
	"sync"
	"time"

	"echo-assistant/internal/live"
)

// The assistant brought into a call on the speaker at home (issue #54, the Echo only): the call on the
// mobile phone over Bluetooth or a landline call that is only the room (dial-in, dial_for_me).
// "Jarvis" or a short press of the button while it runs starts a GPT Live conversation whose
// line is the conference: it hears the microphone at home and the far end mixed into one
// stream, and its voice goes into the room and onto the line at once. It greets the
// participants, speaks when addressed, and leaves on "du kannst gehen" (end_call), a short
// press, or CONFERENCE_IDLE_MS of silence - the call itself goes on.
//
// Mixed, not fed twice: the conversation takes one stream at 24 kHz, so the far end waits in
// conference.far until the microphone's next 20 ms come and the two are added.

// conference: under Phone.mu.
type conference struct {
	out   *confOut // nil for own: his voice goes over the call's own line
	since time.Time
	own   bool // the user joined a call the assistant has itself (speaker_call talk): a three-way call

	mu  sync.Mutex
	far []int16      // the far end, 24 kHz, waiting for the microphone
	mic MicResampler // the room 16 -> 24 kHz (the microphone's goroutine)
}

// confFarMax: the far end kept for the mix at most; more means the microphone stalled.
const confFarMax = apiRate / 5

// confOut: the conversation's voice (live.Output) - into the room and onto the call's line.
type confOut struct {
	p *Phone

	mu      sync.Mutex
	audible time.Time // when the last audible part has been played
	ends    time.Time // when everything queued has been played
	line    []int16   // a mobile call: his voice, 24 kHz, waiting for the microphone's frames
}

func (o *confOut) Queue(pcm []int16) error {
	p := o.p
	p.mu.Lock()
	landline := p.call.line
	mobile := p.mob.audio != nil
	p.mu.Unlock()
	if mobile {
		p.bridge.Assistant(pcm) // the room; the line takes it with the microphone (hearMobile)
		o.mu.Lock()
		if over := len(o.line) + len(pcm) - bridgeMax; over > 0 {
			o.line = o.line[min(over, len(o.line)):]
		}
		o.line = append(o.line, pcm...)
		o.mu.Unlock()
	} else if landline != nil {
		landline.Queue(pcm) // the landline sends it and gives it to the room (Line.Queue)
	}
	peak := 0
	for _, v := range pcm {
		peak = max(peak, int(v), -int(v))
	}
	now := time.Now()
	o.mu.Lock()
	if o.ends.Before(now) {
		o.ends = now
	}
	o.ends = o.ends.Add(time.Duration(len(pcm)) * time.Second / apiRate)
	if peak > audible {
		o.audible = o.ends
	}
	o.mu.Unlock()
	return nil
}

func (o *confOut) Queued() time.Duration {
	o.mu.Lock()
	defer o.mu.Unlock()
	return max(0, time.Until(o.ends))
}

func (o *confOut) Speaking(tail time.Duration) bool {
	o.mu.Lock()
	defer o.mu.Unlock()
	return time.Now().Before(o.audible.Add(tail))
}

func (o *confOut) Flush() {
	p := o.p
	p.bridge.mu.Lock()
	p.bridge.assistant = nil
	p.bridge.mu.Unlock()
	p.mu.Lock()
	landline := p.call.line
	p.mu.Unlock()
	if landline != nil {
		landline.Flush()
	}
	o.mu.Lock()
	o.line, o.ends, o.audible = nil, time.Now(), time.Time{}
	o.mu.Unlock()
}

func (o *confOut) Tone(freq float64, ms int, amp float64) {
	pcm := make([]int16, ms*apiRate/1000)
	for i := range pcm {
		pcm[i] = int16(amp * 32767 * math.Sin(2*math.Pi*freq*float64(i)/apiRate))
	}
	o.Queue(pcm)
}

// take: his voice for one frame of the mobile line (n samples at 24 kHz), zeros if none.
func (o *confOut) take(n int) []int16 {
	o.mu.Lock()
	defer o.mu.Unlock()
	out := make([]int16, n)
	k := copy(out, o.line)
	o.line = o.line[k:]
	return out
}

// --- Starting and ending ----------------------------------------------------------------------

// callOnSpeaker: the call that is on the speaker at home and whom it is with ("" = none). A call
// The assistant has itself (stageTalking) is not one: it is in it already.
func (p *Phone) callOnSpeakerLocked() string {
	m := &p.mob
	if m.audio != nil {
		return orNumber(m.name, m.number)
	}
	if p.call.stage == stageBridge && p.call.probe == nil && p.bridge.Mode().on() {
		return orNumber(p.call.name, p.call.number)
	}
	return ""
}

func orNumber(name, number string) string {
	switch {
	case name != "":
		return name
	case number != "":
		return number
	}
	return "the caller (number withheld)"
}

// ConferenceActive: The assistant is in the call on the speaker.
func (p *Phone) ConferenceActive() bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.conf != nil
}

// CallOnSpeaker: a call is on the speaker at home that the assistant could join.
func (p *Phone) CallOnSpeaker() bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.callOnSpeakerLocked() != ""
}

// StartConference brings the assistant into the call on the speaker; the answer is for the log.
func (p *Phone) StartConference() string {
	p.mu.Lock()
	who := p.callOnSpeakerLocked()
	switch {
	case who == "":
		p.mu.Unlock()
		return "Error: no call is on the speaker."
	case p.conf != nil:
		p.mu.Unlock()
		return "The assistant is in the call already."
	case p.session.Open():
		p.mu.Unlock()
		return "Error: a phone conversation of the assistant is running."
	}
	out := &confOut{p: p}
	p.conf = &conference{out: out, since: time.Now()}
	p.mu.Unlock()
	req := live.Request{Phone: true, Line: out, Person: p.people.Admin(), Caller: who, Conference: who}
	if !p.session.Start(req) {
		p.mu.Lock()
		p.conf = nil
		p.mu.Unlock()
		return "Error: the conversation did not start."
	}
	log.Printf("[Conference] the assistant joins the call with %s", who)
	return "The assistant is in the call."
}

// EndConference takes the assistant out; the call goes on.
func (p *Phone) EndConference(why string) {
	p.mu.Lock()
	conf := p.conf
	if conf != nil && conf.own {
		conf = nil // The assistant's own call: it stays; the user leaves with speaker_call or the button
	}
	if conf != nil {
		p.conf = nil
	}
	p.mu.Unlock()
	if conf == nil {
		return
	}
	if p.session.Open() && p.session.Request().Line == live.Output(conf.out) {
		p.session.Close()
	}
	log.Printf("[Conference] the assistant leaves the call after %s: %s", time.Since(conf.since).Round(time.Second), why)
}

// endConferenceLocked: the call is over, so the assistant's part in it is too. The conversation is
// closed outside the lock.
func (p *Phone) endConferenceLocked(why string) {
	conf := p.conf
	if conf == nil {
		return
	}
	p.conf = nil
	log.Printf("[Conference] %s: "+prompts.Name()+" leaves after %s", why, time.Since(conf.since).Round(time.Second))
	go func() {
		if p.session.Open() && p.session.Request().Line == live.Output(conf.out) {
			p.session.Close()
		}
	}()
}

// conferenceEnded: the conversation of the conference is over (CallEnded): the call goes on.
func (p *Phone) conferenceEnded(e live.Ended) bool {
	p.mu.Lock()
	conf := p.conf
	ours := conf != nil && e.Request.Line == live.Output(conf.out)
	if ours {
		p.conf = nil
	}
	p.mu.Unlock()
	if ours {
		log.Printf("[Conference] the assistant left the call after %s", time.Since(conf.since).Round(time.Second))
	}
	return ours || e.Request.Conference != ""
}

// --- The action button during a call (#54) -----------------------------------------------------

// buttonLong: a press this long is "end" (reject, hang up), a shorter one "yes / next".
var buttonLong = ms("BUTTON_LONG_MS", 1000)

// ButtonOwned: the action button belongs to a call right now; its press is then decided on
// release (CallButton), short or long.
func (p *Phone) ButtonOwned() bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.mob.call != "" || p.conf != nil || p.callOnSpeakerLocked() != "" ||
		p.call.stage == stageTalking || p.call.stage == stageRinging
}

// CallButton: the action button released after held.
//
//	the phone rings        short: answer          long: reject
//	the landline rings along on a secondary Echo   short: take it here   long: not here
//	a call on the speaker  short: The assistant joins    long: hang up
//	The assistant in the call     short: The assistant leaves   long: hang up
//	The assistant's own call       short: listen/talk/off as before   long: hang up
func (p *Phone) CallButton(held time.Duration) string {
	long := held >= buttonLong
	p.mu.Lock()
	ringing := p.mob.call != "" && p.mob.audio == nil && !p.mob.outgoing
	mobile := p.mob.audio != nil || p.mob.call != "" && p.mob.outgoing
	conf := p.conf != nil && !p.conf.own // the three-way call of his own goes on with the cycle
	onSpeaker := p.callOnSpeakerLocked() != ""
	own := p.call.stage == stageTalking
	lineRinging := p.call.stage == stageRinging // a secondary Echo rings along (ringing.go)
	p.mu.Unlock()
	switch {
	case lineRinging && long:
		return p.rejectHere()
	case lineRinging:
		return p.takeCallHere()
	case ringing && long:
		return p.mobileAction("reject")
	case ringing:
		return p.mobileAction("answer")
	case long && mobile:
		return p.mobileAction("hangup")
	case long && (onSpeaker || own || conf):
		p.mu.Lock()
		if p.call.stage != stageNone {
			p.hangUp("the action button")
		}
		p.mu.Unlock()
		return "Ended."
	case conf:
		p.EndConference("the action button")
		return "The assistant left the call."
	case onSpeaker:
		return p.StartConference()
	case own:
		return p.CycleBridge()
	}
	return ""
}

// --- The conference's ears ---------------------------------------------------------------------

// confFar: the far end's audio (24 kHz) for the conference's mix, if there is one; false
// otherwise (the landline then gives it to its own conversation as before).
func (p *Phone) confFar(pcm []int16) bool {
	p.mu.Lock()
	conf := p.conf
	p.mu.Unlock()
	if conf == nil {
		return false
	}
	conf.mu.Lock()
	conf.far = append(conf.far, pcm...)
	if over := len(conf.far) - confFarMax; over > 0 {
		conf.far = conf.far[over:]
	}
	conf.mu.Unlock()
	return true
}

// confHear: the microphone at home (16 kHz) and the far end that waited, mixed, to the
// conversation of the conference.
func (p *Phone) confHear(pcm []int16) {
	p.mu.Lock()
	conf := p.conf
	p.mu.Unlock()
	if conf == nil {
		return
	}
	conf.mu.Lock()
	room := conf.mic.Process(pcm)
	n := min(len(room), len(conf.far))
	for i := 0; i < n; i++ {
		room[i] = clamp16(float32(room[i]) + float32(conf.far[i]))
	}
	conf.far = conf.far[n:]
	conf.mu.Unlock()
	p.session.FeedLine(room)
}

// lineHeard: a landline's far end - into the conference if one runs, else to its conversation.
func (p *Phone) lineHeard(pcm []int16) {
	if !p.confFar(pcm) {
		p.session.FeedLine(pcm)
	}
}
