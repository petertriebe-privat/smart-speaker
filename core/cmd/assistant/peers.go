package main

import (
	"context"
	"fmt"
	"log"
	"math"
	"os"
	"runtime/debug"
	"strings"
	"sync"
	"time"

	"echo-assistant/internal/peer"
	"echo-assistant/internal/prompts"
)

// Several Echos (devices/echo/docs/18-several-echos.md, #62): the peer link and who answers "Hey Jarvis".

// levels: the microphone's level per frame over the last seconds, for the peer link's claims
// (only the microphone goroutine writes).
type levels struct {
	mu     sync.Mutex
	frames [256]levelFrame // 20 ms each: ~5 s
	next   int
}

type levelFrame struct {
	at     time.Time
	sum    float64 // sum of squares
	sample int
}

func (l *levels) feed(pcm []int16) {
	var sum float64
	for _, v := range pcm {
		sum += float64(v) * float64(v)
	}
	l.mu.Lock()
	l.frames[l.next] = levelFrame{at: time.Now(), sum: sum, sample: len(pcm)}
	l.next = (l.next + 1) % len(l.frames)
	l.mu.Unlock()
}

// rms between from and to.
func (l *levels) rms(from, to time.Time) float64 {
	l.mu.Lock()
	defer l.mu.Unlock()
	var sum float64
	n := 0
	for _, f := range l.frames {
		if f.sample > 0 && !f.at.Before(from) && !f.at.After(to) {
			sum += f.sum
			n += f.sample
		}
	}
	if n == 0 {
		return 0
	}
	return math.Sqrt(sum / float64(n))
}

// setupPeers starts the peer link when PEER_KEY is set; before the services (the role is
// settled first), so everything it calls copes with what does not exist yet.
func (d *device) setupPeers() {
	s := d.cfg.Settings
	name := s.Name
	if name == "" {
		name = s.SpotifyName
	}
	if name == "" {
		name, _ = os.Hostname()
	}
	p := peer.New(d.cfg.Secrets.String("PEER_KEY"), prompts.Int("PEER_PORT", 47800), s.Peers)
	if p == nil {
		log.Print("[Peer] off (PEER_KEY missing): this device acts alone")
		return
	}
	// Not primary until the role is settled (failover.go): a returning old primary must not look
	// like one to the others meanwhile.
	p.Name, p.Room, p.Priority, p.Epoch, p.Version, p.NoLead = name, s.Room, s.Priority, s.Epoch, revision(), s.NoLead
	p.Level = d.levels.rms
	// A conversation running here for a moment (not the one this very wake word just opened).
	p.Busy = func() bool {
		return d.session != nil && d.session.Open() && d.session.Elapsed() > 2*time.Second ||
			d.listening() && d.listenedFor() > 2*time.Second
	}
	p.State = d.peerState
	// A louder claim of the same words came late (the M5): it answers, this one drops the
	// conversation the wake word opened.
	p.Outvoiced = func(why string) {
		log.Printf("[Peer] wake word: %s answers", why)
		d.giveUpWake()
	}
	if homeRemote != "" {
		p.SetRemote(homeRemote) // the phone away: the primary over the home tunnel (phoneboard.go)
	}
	d.peers = p
	go p.Run(context.Background())
}

func (d *device) peerState() string {
	switch {
	case d.session == nil:
		return "starting"
	case d.session.Open() || d.listening():
		return "conversation"
	case intercomActive() != nil:
		return "intercom"
	case d.svc.phone != nil && d.svc.phone.CallForRoom():
		return "call" // another Echo can take its room (roomlink.go)
	case d.phoneSession != nil && d.phoneSession.Open() || d.svc.phone != nil && d.svc.phone.InCall():
		return "phone"
	case d.music != nil && d.music.Playing():
		return "music"
	}
	return "idle"
}

// arbitrate: the wake word was heard here at `at` and the conversation has started; another
// Echo that heard it louder takes it, and this one drops its conversation before it says anything.
func (d *device) arbitrate(score float64, at time.Time) {
	won, why := d.peers.Claim(score, at)
	d.wakeClips.note(func(r *wakeRecord) {
		r.Peer = round2(d.peers.Heard(at))
		if !won {
			r.Answered = "other"
		}
	})
	if won {
		if why != "alone" {
			log.Printf("[Peer] wake word: answering here (%s)", why)
		}
		return
	}
	log.Printf("[Peer] wake word: %s answers", why)
	d.giveUpWake()
}

// giveUpWake: another device answers the wake word heard here; this one drops what it opened
// (the first words or, once decided, the conversation) before it says anything.
func (d *device) giveUpWake() {
	c := &d.capture
	c.mu.Lock()
	wake := c.active && c.req.Wake
	c.mu.Unlock()
	if wake {
		d.wakeClips.note(func(r *wakeRecord) { r.Answered = "other" })
		d.stopListening("another device answers", true)
		return
	}
	if d.session.Open() && d.session.Request().Wake {
		d.session.Abandon()
	}
}

// peersText: the console's view of the link.
func (d *device) peersText() string {
	if d.peers == nil {
		return "peer link off (PEER_KEY missing)"
	}
	var b strings.Builder
	fmt.Fprintf(&b, "this %q priority %d", d.peers.Name, d.peers.Priority)
	for _, p := range d.peers.Alive() {
		fmt.Fprintf(&b, "; %q at %s, %s, room %q, primary %v, seen %s ago", p.Name, p.Addr, p.State, p.Room,
			p.Primary, time.Since(p.Seen).Round(time.Second))
	}
	return b.String()
}

// revision: the commit this binary was built from (the heartbeat shows whether all Echos run
// the same).
func revision() string {
	info, ok := debug.ReadBuildInfo()
	if !ok {
		return ""
	}
	rev, dirty := "", false
	for _, s := range info.Settings {
		switch s.Key {
		case "vcs.revision":
			rev = s.Value[:min(7, len(s.Value))]
		case "vcs.modified":
			dirty = s.Value == "true"
		}
	}
	if dirty {
		rev += "+"
	}
	return rev
}
