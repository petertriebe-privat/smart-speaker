package main

import (
	"echo-assistant/internal/prompts"
	"encoding/json"
	"fmt"
	"log"
	"math"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"echo-assistant/internal/store"
)

// Guided wake word recordings (#61, E3; later the voice profile of #45):
//
//	jarvis ctl enroll <person> <session> [takes]   a beep per take, the person says "Jarvis"
//	jarvis ctl enroll <person> negative [seconds]  free talk without the wake word (default 90 s)
//	jarvis ctl enroll status | stop | delete <person>
//
// The whole session is recorded without gaps, the raw microphones and BMicsOut side by side
// (recordRaw), into <state_dir>/voices/<person>/<session>-<time>/; takes.json says when each take
// was spoken (from BMicsOut's level), for cutting the clips on the PC. A take without voice
// within 3 s is asked again (twice at most). The wake word is off meanwhile: the takes must not
// open conversations. Household voices: never in the repository; "delete" removes a person.

const (
	enrollDefaultTakes   = 20
	enrollDefaultSeconds = 90
	enrollWait           = 3 * time.Second         // for the voice after the beep
	enrollMaxTake        = 2500 * time.Millisecond // a take longer than this is cut
	enrollTail           = 400 * time.Millisecond  // quiet that ends a take
	enrollPause          = 1200 * time.Millisecond // between takes
	enrollRetries        = 2
	enrollDeaf           = 500 * time.Millisecond // after the beep's call (its echo is told by its pitch)
	enrollBeepHz         = 880
	enrollMinLoud        = 4 // loud 20 ms chunks a word has at least (a click has one)
)

var enrollName = regexp.MustCompile(`^[a-z0-9_-]{1,32}$`)

// enrollRef: the running enrollment, nil = none.
type enrollRef = atomic.Pointer[enrollment]

type enrollTake struct {
	N         int     `json:"n"`
	Beep      float64 `json:"beep_s"` // seconds from the recording's start
	Start     float64 `json:"start_s"`
	End       float64 `json:"end_s"`
	Peak      int     `json:"peak"`      // BMicsOut's highest sample in the take
	Threshold int     `json:"threshold"` // the level that counted as voice
}

type enrollment struct {
	Person  string       `json:"person"`
	Session string       `json:"session"`
	Device  string       `json:"device"`
	Started string       `json:"started"`
	Music   bool         `json:"music"`
	Seconds int          `json:"seconds,omitempty"` // free sessions
	Takes   []enrollTake `json:"takes"`
	Missed  int          `json:"missed"` // beeps without voice
	want    int
	dir     string
	stop    chan struct{}
	done    chan struct{}

	mu        sync.Mutex
	recStart  time.Time
	recent    []int // chunk peaks of the last second (floor estimate)
	armed     bool
	threshold int
	first     time.Time // first loud chunk since armed
	lastLoud  time.Time
	peak      int
	loud      int // loud chunks since first
	state     string
}

// enrollArgs parses "enroll ..." into person, session and the count (takes or seconds).
func enrollArgs(fields []string) (person, session string, count int, err error) {
	if len(fields) < 2 || len(fields) > 3 {
		return "", "", 0, fmt.Errorf("enroll <person> <session> [takes] | enroll <person> negative [seconds] | enroll status | stop | delete <person>")
	}
	person, session = fields[0], fields[1]
	if !enrollName.MatchString(person) || !enrollName.MatchString(session) {
		return "", "", 0, fmt.Errorf("person and session: a-z, 0-9, _ and - only")
	}
	count = enrollDefaultTakes
	if strings.HasPrefix(session, "negative") {
		count = enrollDefaultSeconds
	}
	if len(fields) == 3 {
		if count, err = strconv.Atoi(fields[2]); err != nil || count < 1 || count > 600 {
			return "", "", 0, fmt.Errorf("count 1..600")
		}
	}
	return person, session, count, nil
}

// voiceThreshold: the level that counts as voice, from the chunk peaks of the last second: well
// above the room (music or noise) and never below the quietest far voice of the 19.10.2026
// recordings (BMicsOut peaks of 150-400 for normal speech), and above the beep's echo (93).
func voiceThreshold(recent []int) int {
	if len(recent) == 0 {
		return 50
	}
	s := append([]int(nil), recent...)
	sort.Ints(s)
	return max(50, 3*s[len(s)/2])
}

func (d *device) enrollCommand(arg string) string {
	fields := strings.Fields(arg)
	if len(fields) > 0 {
		switch fields[0] {
		case "status":
			s := "no enrollment running"
			if e := d.enroll.Load(); e != nil {
				s = e.status()
			}
			if g := d.guide.Load(); g != nil {
				s = fmt.Sprintf("guided training of %s, part %d of %d: %s", g.person, g.step.Load(), len(guideSteps), s)
			}
			return s
		case "stop":
			return d.stopEnroll()
		case "delete":
			if len(fields) != 2 || !enrollName.MatchString(fields[1]) {
				return "error: enroll delete <person>"
			}
			if e := d.enroll.Load(); e != nil && e.Person == fields[1] || d.guide.Load() != nil {
				return "error: an enrollment is running"
			}
			if err := os.RemoveAll(filepath.Join(store.Dir, "voices", fields[1])); err != nil {
				return "error: " + err.Error()
			}
			return "deleted the recordings of " + fields[1]
		}
	}
	if len(fields) >= 2 && fields[1] == "guided" && enrollName.MatchString(fields[0]) {
		// guided [part]: from that part on (an interrupted training goes on where it stopped)
		from := 1
		if len(fields) == 3 {
			if n, err := strconv.Atoi(fields[2]); err == nil && n >= 1 && n <= len(guideSteps) {
				from = n
			} else {
				return fmt.Sprintf("error: part 1..%d", len(guideSteps))
			}
		}
		return d.startGuide(fields[0], from)
	}
	person, session, count, err := enrollArgs(fields)
	if err != nil {
		return "error: " + err.Error()
	}
	if d.session.Open() {
		return "error: a conversation is open"
	}
	e := d.newEnrollment(person, session, count)
	if d.guide.Load() != nil || !d.enroll.CompareAndSwap(nil, e) {
		return "error: an enrollment is running"
	}
	go d.runEnrollment(e)
	if e.Seconds > 0 {
		return fmt.Sprintf("enrolling %s/%s: talk freely for %d s without the wake word", person, session, count)
	}
	return fmt.Sprintf("enrolling %s/%s: %d takes, say \""+prompts.Name()+"\" after each beep", person, session, count)
}

// newEnrollment: one recorded part; sessions named negative... are free talk for count seconds,
// the others count takes.
func (d *device) newEnrollment(person, session string, count int) *enrollment {
	now := time.Now()
	device := d.cfg.Settings.Name
	if device == "" {
		device = d.cfg.Settings.SpotifyName
	}
	e := &enrollment{
		Person: person, Session: session, Device: device, Started: now.Format(time.RFC3339),
		Music: d.music != nil && d.music.Playing(), want: count,
		dir:  filepath.Join(store.Dir, "voices", person, session+"-"+now.Format("20060102-150405")),
		stop: make(chan struct{}), done: make(chan struct{}), state: "starting",
	}
	if strings.HasPrefix(session, "negative") {
		e.Seconds, e.want = count, 0
	}
	return e
}

// stopEnroll ends the guided training and the running part (ctl, the action button).
func (d *device) stopEnroll() string {
	g, e := d.guide.Load(), d.enroll.Load()
	if g == nil && e == nil {
		return "no enrollment running"
	}
	if g != nil {
		g.cancel()
	}
	if e != nil {
		e.finish()
		<-e.done
		return "stopped: " + e.status()
	}
	return "stopped"
}

// enrolling: an enrollment or the guided training runs (the wake word is off).
func (d *device) enrolling() bool { return d.enroll.Load() != nil || d.guide.Load() != nil }

func (e *enrollment) finish() {
	e.mu.Lock()
	defer e.mu.Unlock()
	select {
	case <-e.stop:
	default:
		close(e.stop)
	}
}

func (e *enrollment) stopped() bool {
	select {
	case <-e.stop:
		return true
	default:
		return false
	}
}

func (e *enrollment) status() string {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.Seconds > 0 {
		return fmt.Sprintf("%s/%s: %s", e.Person, e.Session, e.state)
	}
	return fmt.Sprintf("%s/%s: %s, %d of %d takes, %d missed", e.Person, e.Session, e.state, len(e.Takes), e.want, e.Missed)
}

func (e *enrollment) setState(s string) {
	e.mu.Lock()
	e.state = s
	e.mu.Unlock()
}

// feed: the microphone (BMicsOut) while the enrollment runs, from hear.
func (e *enrollment) feed(pcm []int16) {
	peak := 0
	for _, v := range pcm {
		peak = max(peak, int(v), -int(v))
	}
	now := time.Now()
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.recent = append(e.recent, peak); len(e.recent) > 50 { // ~1 s of 20 ms chunks
		e.recent = e.recent[1:]
	}
	if !e.armed || peak < e.threshold || beepShare(pcm) > 0.3 {
		return // quiet, or the beep's own echo (a pure 880 Hz tone)
	}
	if e.first.IsZero() {
		e.first = now
	}
	e.lastLoud = now
	e.peak = max(e.peak, peak)
	e.loud++
}

// beepShare: the share of the chunk's energy at the beep's 880 Hz (Goertzel, 16 kHz). The beep's
// echo in BMicsOut has 0.3-0.8 of it, a voice a few hundredths (measured 20.10.2026).
func beepShare(pcm []int16) float64 {
	if len(pcm) == 0 {
		return 0
	}
	k := 2 * math.Cos(2*math.Pi*enrollBeepHz/16000)
	var s1, s2, energy float64
	for _, v := range pcm {
		x := float64(v)
		s1, s2 = x+k*s1-s2, s1
		energy += x * x
	}
	if energy == 0 {
		return 0
	}
	return (s1*s1 + s2*s2 - k*s1*s2) * 2 / float64(len(pcm)) / energy
}

// arm: listen for a take from now on; returns the threshold.
func (e *enrollment) arm() int {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.armed, e.first, e.lastLoud, e.peak, e.loud = true, time.Time{}, time.Time{}, 0, 0
	e.threshold = voiceThreshold(e.recent)
	return e.threshold
}

func (e *enrollment) since(t time.Time) float64 { return t.Sub(e.recStart).Seconds() }

func (d *device) runEnrollment(e *enrollment) {
	defer func() {
		d.enroll.CompareAndSwap(e, nil)
		close(e.done)
	}()
	recStop := make(chan struct{})
	recDone := make(chan error, 1)
	// takes.json counts from the WAV files' first sample (until 20.10.2026 about 0.5 s before it:
	// the capture's start)
	e.recStart = time.Now()
	go func() {
		rec, err := recordRaw(e.dir, []rawRing{ringMics, ringMicsOut}, recStop, func() {
			e.mu.Lock()
			e.recStart = time.Now()
			e.mu.Unlock()
		})
		if err == nil {
			log.Printf("[Enroll] recorded:\n%s", rec)
		}
		recDone <- err
	}()
	time.Sleep(time.Second) // the capture starts, the floor estimate fills
	log.Printf("[Enroll] %s/%s into %s", e.Person, e.Session, e.dir)
	d.speaker.Tone(523, 120, 0.15)
	d.speaker.Tone(784, 160, 0.15)
	if e.Seconds > 0 {
		e.setState("recording")
		select {
		case <-e.stop:
		case <-time.After(time.Duration(e.Seconds) * time.Second):
		}
	} else {
		d.enrollTakes(e)
	}
	d.speaker.Tone(784, 120, 0.15)
	d.speaker.Tone(523, 160, 0.15)
	time.Sleep(500 * time.Millisecond)
	close(recStop)
	err := <-recDone
	e.mu.Lock()
	e.state = "done"
	data, _ := json.MarshalIndent(e, "", "  ")
	e.mu.Unlock()
	if werr := os.WriteFile(filepath.Join(e.dir, "takes.json"), data, 0o600); err == nil {
		err = werr
	}
	if err != nil {
		log.Printf("[Enroll] %v", err)
		e.setState("failed: " + err.Error())
		return
	}
	log.Printf("[Enroll] done: %s", e.status())
}

func (d *device) enrollTakes(e *enrollment) {
	for n, retries := 1, 0; n <= e.want && !e.stopped(); {
		e.setState(fmt.Sprintf("take %d", n))
		beep := time.Now()
		d.speaker.Tone(enrollBeepHz, 150, 0.2)
		// Listen only once the beep is over: it sounds ~0.9-1.45 s after the call (PulseAudio's
		// buffer) and the DSP leaves a peak of ~300 of it in BMicsOut (20.10.2026). The recording
		// runs on meanwhile; a fast "Jarvis" is only counted a little late.
		time.Sleep(time.Until(beep.Add(enrollDeaf)))
		threshold := e.arm()
		start, end, peak := e.awaitTake()
		e.mu.Lock()
		e.armed = false
		if start.IsZero() {
			e.Missed++
			e.mu.Unlock()
			if retries++; retries > enrollRetries {
				log.Printf("[Enroll] take %d: no voice, skipped", n)
				n, retries = n+1, 0
			}
			continue
		}
		e.Takes = append(e.Takes, enrollTake{N: n, Beep: e.since(beep), Start: e.since(start), End: e.since(end),
			Peak: peak, Threshold: threshold})
		e.mu.Unlock()
		log.Printf("[Enroll] take %d: %.2f s, peak %d (threshold %d)", n, end.Sub(start).Seconds(), peak, threshold)
		n, retries = n+1, 0
		select {
		case <-e.stop:
		case <-time.After(enrollPause):
		}
	}
}

// awaitTake: the take's first and last loud moment and its peak; zero start = nobody spoke.
func (e *enrollment) awaitTake() (start, end time.Time, peak int) {
	armed := time.Now()
	for !e.stopped() {
		time.Sleep(20 * time.Millisecond)
		e.mu.Lock()
		first, last, p, loud := e.first, e.lastLoud, e.peak, e.loud
		if !first.IsZero() && loud < enrollMinLoud && time.Since(last) > enrollTail {
			// a click or a knock, not a word: forget it and wait on
			e.first, e.lastLoud, e.peak, e.loud = time.Time{}, time.Time{}, 0, 0
			first = time.Time{}
		}
		e.mu.Unlock()
		switch {
		case first.IsZero() || loud < enrollMinLoud:
			if first.IsZero() && time.Since(armed) > enrollWait {
				return time.Time{}, time.Time{}, 0
			}
		case time.Since(last) > enrollTail || time.Since(first) > enrollMaxTake:
			return first, last, p
		}
	}
	return time.Time{}, time.Time{}, 0
}
