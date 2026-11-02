package phone

import (
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"log"
	"strings"
	"sync"
	"time"

	"echo-assistant/internal/prompts"
	"echo-assistant/internal/store"
)

// How a call on the mobile phone rings at home (issue #51): a cycle every mobileRing, at most
// mobileRings (then the call is on the mailbox anyway) - two soft rising tones, half a second,
// the announcement ("Anna ruft auf dem Handy an"), silence. At night only the announcement, at
// half the level: the call wakes nobody, but whoever is awake hears it. Never the phone's own
// ring tone (in-band ringing over SCO): The assistant rings itself.

var (
	mobileRing      = ms("MOBILE_RING_EVERY_MS", 6000)
	mobileRings     = prompts.Int("MOBILE_RINGS", 5)
	mobileQuietFrom = prompts.Int("MOBILE_QUIET_FROM", 22) // hour; the night lasts until ...
	mobileQuietTo   = prompts.Int("MOBILE_QUIET_TO", 7)
)

const (
	mobileRingAmp  = 0.05                   // about a third of the announcement tones of #49
	mobileNameIn   = 900 * time.Millisecond // the name after the tones (two tones and half a second)
	mobileNightAmp = 0.5                    // the announcement at night
	mobileClips    = "mobile-voice"         // the announcements on the data partition
)

// The system note to a conversation at the speaker while the phone rings (English, as all of
// them); {name} is the caller.
const mobileRingingNote = "System note, not from the user: a call on the user's mobile phone is ringing, from {name}. " +
	"Tell them in one short sentence and ask whether you should answer it; with yes, call mobile_call (answer), " +
	"with no, mobile_call (reject)."

// mobileQuiet: the night by the hours MOBILE_QUIET_FROM / MOBILE_QUIET_TO.
func mobileQuiet(now time.Time) bool {
	h := now.Hour()
	if mobileQuietFrom == mobileQuietTo {
		return false
	}
	if mobileQuietFrom < mobileQuietTo {
		return h >= mobileQuietFrom && h < mobileQuietTo
	}
	return h >= mobileQuietFrom || h < mobileQuietTo
}

// mobileAnnouncement: what is said while it rings.
func mobileAnnouncement(name, number string) string {
	switch {
	case name != "":
		return name + " ruft auf dem Handy an."
	case number == "":
		return "Ein Anruf auf dem Handy, Nummer unterdrückt."
	}
	return "Ein Anruf auf dem Handy, Nummer nicht im Adressbuch."
}

// mobileRingLocked: one ring cycle every mobileRing, at most mobileRings. Into a conversation at
// the speaker it does not ring: that one is told once, and the assistant asks (as LIVE_CALL_WAITING for
// the landline). While the landline has the speaker, only the LED ring shows it.
func (p *Phone) mobileRingLocked(now time.Time) {
	m := &p.mob
	if p.deps.HomeBusy != nil && p.deps.HomeBusy() {
		if !m.noted && p.deps.HomeNote != nil {
			m.noted = true
			who := m.name
			if who == "" {
				who = "a number not in the address book"
				if m.number == "" {
					who = "a withheld number"
				}
			}
			note := strings.ReplaceAll(prompts.TextOr("LIVE_MOBILE_RINGING", mobileRingingNote), "{name}", who)
			go p.deps.HomeNote(note)
		}
		return
	}
	if m.rings >= mobileRings || !m.ringAt.IsZero() && now.Sub(m.ringAt) < mobileRing {
		return
	}
	if p.deps.Speaker == nil || p.bridge.Mode().on() {
		return
	}
	m.rings++
	m.ringAt = now
	speaker, quiet := p.deps.Speaker, mobileQuiet(now)
	text := mobileAnnouncement(m.name, m.number)
	go func() {
		if !quiet {
			speaker.Tone(660, 140, mobileRingAmp)
			speaker.Tone(880, 180, mobileRingAmp)
			time.Sleep(mobileNameIn)
		}
		p.playAnnouncement(text, quiet)
	}()
}

// playAnnouncement: the announcement from the clip store, made by text to speech the first time.
// A new one is only made then - this cycle has just the tones, the next ones the name. Without
// Speech (tests, a device without TTS) it is spoken through Say each time.
func (p *Phone) playAnnouncement(text string, quiet bool) {
	if p.deps.Speech == nil {
		if p.deps.Say != nil {
			p.deps.Say(text)
		}
		return
	}
	pcm, ok := p.clips.get(text)
	if !ok {
		go p.clips.make(text, p.deps.Speech)
		return
	}
	if quiet {
		scaled := make([]int16, len(pcm))
		for i, v := range pcm {
			scaled[i] = int16(float64(v) * mobileNightAmp)
		}
		pcm = scaled
	}
	// Still ringing? The user may have answered while the tones played.
	if p.MobileRinging() {
		p.deps.Speaker.Queue(pcm)
	}
}

// clipStore: the announcements, in memory and on the data partition (24 kHz PCM), as the
// "Ja, Sir?" clips of internal/acknowledge: made once, then without waiting and without cost.
type clipStore struct {
	mu     sync.Mutex
	clips  map[string][]int16
	making map[string]bool
}

func clipName(text string) string {
	sum := sha256.Sum256([]byte(text))
	return mobileClips + "/" + hex.EncodeToString(sum[:8]) + ".pcm"
}

func (c *clipStore) get(text string) ([]int16, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if pcm, ok := c.clips[text]; ok {
		return pcm, true
	}
	data, err := store.Read(clipName(text))
	if err != nil || len(data) < 2 {
		return nil, false
	}
	pcm := make([]int16, len(data)/2)
	for i := range pcm {
		pcm[i] = int16(binary.LittleEndian.Uint16(data[i*2:]))
	}
	if c.clips == nil {
		c.clips = map[string][]int16{}
	}
	c.clips[text] = pcm
	return pcm, true
}

func (c *clipStore) make(text string, speech func(string) ([]int16, error)) {
	c.mu.Lock()
	if c.making == nil {
		c.making = map[string]bool{}
	}
	if c.making[text] {
		c.mu.Unlock()
		return
	}
	c.making[text] = true
	c.mu.Unlock()
	defer func() {
		c.mu.Lock()
		delete(c.making, text)
		c.mu.Unlock()
	}()
	pcm, err := speech(text)
	if err != nil || len(pcm) == 0 {
		log.Printf("[Mobile] announcement %q: %v", text, err)
		return
	}
	data := make([]byte, len(pcm)*2)
	for i, v := range pcm {
		binary.LittleEndian.PutUint16(data[i*2:], uint16(v))
	}
	if err := store.Write(clipName(text), data); err != nil {
		log.Printf("[Mobile] announcement not stored: %v", err)
	}
	c.mu.Lock()
	if c.clips == nil {
		c.clips = map[string][]int16{}
	}
	c.clips[text] = pcm
	c.mu.Unlock()
}
