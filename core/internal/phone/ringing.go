package phone

import (
	"log"
	"time"
)

// Several Echos (devices/echo/docs/18-several-echos.md, #63): every Echo registers with the same
// account, and the FritzBox rings all of them (tested 18.10.2026). The primary device answers as
// before. A secondary one rings along - two soft tones, the ring as for the mobile phone - and
// takes the call only when the user presses its button: then it is the user's own call on its
// speaker and microphone, without GPT Live, as dial_for_me. The FritzBox ends the ringing of the
// others when one answers (CANCEL).
//
// Without its partners a secondary one is the phone of the house: when the REGISTER's answer
// lists no other device (the primary is off), it answers at once as the primary would; and a call
// nobody took within PHONE_FALLBACK_ANSWER_MS (the primary is busy or hangs) it answers itself.

// SetSecondary: this device is not the primary one. Call before Run.
func (p *Phone) SetSecondary(on bool) { p.secondary = on }

// LineRinging: a landline call rings here along with the primary device.
func (p *Phone) LineRinging() bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.call.stage == stageRinging
}

// ringAlongLocked: the ringing of a secondary device, every PHONE_RING_EVERY_MS; after
// PHONE_FALLBACK_ANSWER_MS nobody took the call: The assistant answers here.
func (p *Phone) ringAlongLocked(now time.Time) {
	c := &p.call
	if now.Sub(c.since) >= fallbackAnswer {
		log.Printf("[Phone] nobody took the call within %v: answering here", fallbackAnswer)
		if p.session.Open() {
			c.stage = stageWaiting // a conversation here: it is told, as on the primary
			return
		}
		p.pickUp()
		return
	}
	if p.deps.Speaker == nil || p.bridge.Mode().on() || p.deps.HomeBusy != nil && p.deps.HomeBusy() {
		return
	}
	if !c.ringbackAt.IsZero() && now.Sub(c.ringbackAt) < lineRingEvery {
		return
	}
	c.ringbackAt = now
	speaker := p.deps.Speaker
	go func() {
		speaker.Tone(660, 140, mobileRingAmp)
		speaker.Tone(880, 180, mobileRingAmp)
	}()
}

// takeCallHere: the button while it rings here: the user's own call on this speaker.
func (p *Phone) takeCallHere() string {
	p.mu.Lock()
	defer p.mu.Unlock()
	c := &p.call
	if c.stage != stageRinging {
		return "Error: no call rings here."
	}
	p.answer()
	if p.call.stage == stageNone { // no RTP port: hung up
		return "Error: the call could not be answered."
	}
	c.stage, c.bridgeSince, c.direct = stageBridge, time.Now(), true
	if p.bridge.setMode(modeTalk) {
		p.bridge.announce(true)
	}
	log.Printf("[Phone] call from %s taken here, on the speaker", c.number)
	return "Answered here."
}

// rejectHere: a long press while it rings here: this device stops ringing; the others go on.
func (p *Phone) rejectHere() string {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.call.stage != stageRinging {
		return ""
	}
	p.reply(486, "Busy Here", "")
	log.Print("[Phone] not taken here (button)")
	p.clearCall()
	return "Not taken here."
}
