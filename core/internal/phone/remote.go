package phone

import (
	"log"
	"time"
)

// Another Echo as the room of a call (phase 7 of devices/echo/docs/18-several-echos.md, #63): the call
// runs here - The assistant's landline call, the user's own, or the mobile phone's - and the user is at
// another Echo. That one's speaker plays the room direction (a Speaker that sends the frames over
// the peer link) and its microphone goes onto the line (HearRemote) instead of this one's. The
// modes are the bridge's: listen, then talk; off ends it as at home (a call that is only the
// bridge hangs up). When the bridge goes off - the call ended, or off from here - the room is
// this Echo's again and the other one is told (gone).

// remote: the room is another Echo's.
func (b *bridge) remote() bool {
	if b == nil {
		return false
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.speaker != b.local
}

// CallForRoom: a call runs here that another Echo can take the room of.
func (p *Phone) CallForRoom() bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.callForRoomLocked()
}

func (p *Phone) callForRoomLocked() bool {
	return p.call.probe == nil && (p.call.stage == stageTalking || p.call.stage == stageBridge) || p.mob.audio != nil
}

// MoveRoom: the call's room is another Echo's from now on, in mode (listen or talk); sp plays
// there, gone is called once it is over. The answer is for the tool or the console.
func (p *Phone) MoveRoom(sp Speaker, mode string, gone func()) string {
	m, ok := modeByName(mode)
	if !ok || !m.on() || m == modeMonitor {
		return "Error: mode must be listen or talk."
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if !p.callForRoomLocked() {
		return "Error: no call is running here."
	}
	b := p.bridge
	b.mu.Lock()
	b.speaker, b.gone = sp, gone
	b.mu.Unlock()
	log.Printf("[Bridge] the room is another Echo's (%s)", m.name())
	if p.mob.audio != nil { // the mobile phone's call: the room only
		if b.setMode(m) {
			b.announce(true)
		}
		return "The call is " + m.name() + " at the other Echo now."
	}
	if b.Mode() == m { // already on here: the tones tell the new room
		b.announce(true)
		return "The call is " + m.name() + " at the other Echo now."
	}
	return p.setBridgeLocked(m)
}

// RoomMode: listen, talk or off at the other Echo (its button and the tool there).
func (p *Phone) RoomMode(mode string) string {
	if !p.bridge.remote() {
		return "Error: the room of the call is not at another Echo."
	}
	m, ok := modeByName(mode)
	if !ok || m == modeMonitor {
		return "Error: mode must be off, listen or talk."
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.mob.audio != nil {
		if !m.on() {
			p.bridge.setMode(modeOff)
			return "The call is not at the other Echo any more."
		}
		p.bridge.setMode(m)
		return "The call is " + m.name() + " at the other Echo now."
	}
	return p.setBridgeLocked(m)
}

// HearRemote takes the other Echo's microphone (16 kHz, every 20 ms) while the room is there.
func (p *Phone) HearRemote(pcm []int16) {
	if p.bridge.remote() {
		p.hear(pcm)
	}
}

// remoteSpeaker: a Speaker on another Echo, for tests and the device (cmd/assistant builds the real
// one on the peer link).
type remoteSpeaker struct {
	queue func([]int16)
	tone  func(freq float64, ms int, amp float64)
	flush func()
}

// RemoteSpeaker: a Speaker whose frames go elsewhere. Queued is 0: the other Echo's speaker
// drops what comes too late itself.
func RemoteSpeaker(queue func([]int16), tone func(freq float64, ms int, amp float64), flush func()) Speaker {
	return &remoteSpeaker{queue: queue, tone: tone, flush: flush}
}

func (r *remoteSpeaker) Queue(pcm []int16) error                { r.queue(pcm); return nil }
func (r *remoteSpeaker) Tone(freq float64, ms int, amp float64) { r.tone(freq, ms, amp) }
func (r *remoteSpeaker) Flush()                                 { r.flush() }
func (r *remoteSpeaker) Queued() time.Duration                  { return 0 }
