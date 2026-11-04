package phone

import (
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"echo-assistant/internal/store"
	"echo-assistant/internal/tools"
)

// fakeHF: oFono with one paired phone.
type fakeHF struct {
	mu       sync.Mutex
	modem    hfModem
	calls    []hfCall
	down     bool // ofonod is not there
	away     bool // the phone is out of reach: powering fails
	answered []string
	hungUp   []string
	powered  []bool
}

func (f *fakeHF) Modems() ([]hfModem, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.down {
		return nil, errors.New("org.freedesktop.DBus.Error.ServiceUnknown")
	}
	return []hfModem{f.modem}, nil
}

func (f *fakeHF) SetPowered(modem string, on bool) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.powered = append(f.powered, on)
	if on && f.away {
		return errors.New("org.ofono.Error.Failed: Operation failed")
	}
	f.modem.Powered, f.modem.Online = on, on // the phone connects HFP at once
	return nil
}

func (f *fakeHF) Calls(modem string) ([]hfCall, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]hfCall(nil), f.calls...), nil
}

func (f *fakeHF) Answer(call string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.answered = append(f.answered, call)
	return nil
}

func (f *fakeHF) Hangup(call string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.hungUp = append(f.hungUp, call)
	return nil
}

func (f *fakeHF) Dial(modem, number string) error { return nil }

func (f *fakeHF) set(calls ...hfCall) {
	f.mu.Lock()
	f.calls = calls
	f.mu.Unlock()
}

// fakeSCO: hw:0,2 without the Bluetooth chip.
type fakeSCO struct {
	mu      sync.Mutex
	deliver func([]int16)
	written int
	stopped bool
}

func (f *fakeSCO) Start(deliver func([]int16)) error {
	f.mu.Lock()
	f.deliver = deliver
	f.mu.Unlock()
	return nil
}

func (f *fakeSCO) Write(pcm8 []int16) {
	f.mu.Lock()
	f.written += len(pcm8)
	f.mu.Unlock()
}

func (f *fakeSCO) Lead() (time.Duration, int64) { return 0, 0 }

func (f *fakeSCO) Stop() {
	f.mu.Lock()
	f.stopped = true
	f.mu.Unlock()
}

// spoken: what the phone said through Say (in its own goroutine), read under the lock.
type spoken struct {
	mu    sync.Mutex
	texts []string
}

func (s *spoken) add(text string) {
	s.mu.Lock()
	s.texts = append(s.texts, text)
	s.mu.Unlock()
}

func (s *spoken) count() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.texts)
}

func (s *spoken) at(i int) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.texts[i]
}

func mobileHarness(t *testing.T) (*harness, *fakeHF, *[]*fakeSCO, *spoken) {
	t.Helper()
	h := newHarness(t, nil)
	hf := &fakeHF{modem: hfModem{Path: "/hfp/adapter_phone"}}
	var lines []*fakeSCO
	said := &spoken{}
	h.p.hf = hf
	h.p.newSCO = func() scoLine {
		line := &fakeSCO{}
		lines = append(lines, line)
		return line
	}
	h.p.deps.Say = said.add
	return h, hf, &lines, said
}

func TestMobileCallRingsIsAnsweredAndEnds(t *testing.T) {
	h, hf, lines, said := mobileHarness(t)
	// daytime: at night the ring plays no tones (mobileQuiet), and the test failed after 22:00
	y, m, d := time.Now().Date()
	now := time.Date(y, m, d, 12, 0, 0, 0, time.Local)

	// Hands-free off (the default): the modem stays unpowered, nothing rings.
	h.p.pollMobile(now)
	if len(hf.powered) != 0 {
		t.Fatalf("powered %v although hands-free is off", hf.powered)
	}
	// Switched on: the modem is powered, the phone connects.
	h.p.mob.mode = "on"
	h.p.pollMobile(now)
	h.p.pollMobile(now)
	if len(hf.powered) != 1 || !hf.powered[0] {
		t.Fatalf("powered %v, want [true]", hf.powered)
	}

	// A call comes in: it rings at home, with the name, but the phone's own ring tone over SCO
	// (in-band ringing) stays out of the room.
	call := hfCall{Path: "/hfp/adapter_phone/voicecall01", State: "incoming", Number: "+49 151 12345678"}
	hf.set(call)
	h.p.pollMobile(now)
	if !h.p.MobileRinging() {
		t.Fatal("not ringing")
	}
	waitFor(t, "the ring tones", func() bool { return h.speaker.toneCount() >= 2 })
	waitFor(t, "the caller announced", func() bool { return said.count() == 1 })
	if !strings.Contains(said.at(0), "Handy") {
		t.Errorf("announced %q", said.at(0))
	}
	if len(*lines) != 0 {
		t.Fatal("call audio opened while it rings")
	}
	// The next cycle only after mobileRing.
	h.p.pollMobile(now.Add(time.Second))
	time.Sleep(50 * time.Millisecond)
	if n := h.speaker.toneCount(); n != 2 {
		t.Errorf("%d tones within one cycle, want 2", n)
	}

	// The button answers.
	if answer := h.p.MobileButton(); !strings.HasPrefix(answer, "Answered") {
		t.Fatalf("button: %q", answer)
	}
	if len(hf.answered) != 1 || hf.answered[0] != call.Path {
		t.Fatalf("answered %v", hf.answered)
	}
	call.State = "active"
	hf.set(call)
	h.p.pollMobile(now.Add(2 * time.Second))
	if len(*lines) != 1 || !h.p.MobileActive() {
		t.Fatal("the answered call is not on the speaker")
	}
	if h.p.bridge.Mode() != modeTalk {
		t.Errorf("bridge %s, want talk", h.p.bridge.Mode().name())
	}
	line := (*lines)[0]
	// The far end reaches the room's queue (8 -> 24 kHz), the room the far end (16 -> 8 kHz).
	line.deliver(constant(3000, frameLen))
	h.p.bridge.mu.Lock()
	queued := len(h.p.bridge.call)
	h.p.bridge.mu.Unlock()
	if queued != apiFrame {
		t.Errorf("%d samples of the far end queued, want %d", queued, apiFrame)
	}
	for i := 0; i < 5; i++ {
		h.p.Hear(sine(400, micRate, micFrame, 4000))
	}
	if line.written != 5*frameLen {
		t.Errorf("%d samples to the phone, want %d", line.written, 5*frameLen)
	}
	// The button now hangs up.
	if answer := h.p.MobileButton(); answer != "Ended." {
		t.Errorf("button during the call: %q", answer)
	}
	// The call is gone: audio stops, the room goes back to normal.
	hf.set()
	h.p.pollMobile(now.Add(3 * time.Second))
	if !line.stopped || h.p.MobileActive() || h.p.bridge.Mode().on() {
		t.Errorf("after the call: stopped %v, active %v, bridge %s", line.stopped, h.p.MobileActive(), h.p.bridge.Mode().name())
	}
}

func TestMobileRingLeavesAConversationAlone(t *testing.T) {
	h, hf, _, said := mobileHarness(t)
	h.p.mob.mode = "on"
	h.p.pollMobile(time.Now())
	h.busy = true // someone talks with the assistant at the speaker
	hf.set(hfCall{Path: "/hfp/adapter_phone/voicecall01", State: "incoming"})
	h.p.pollMobile(time.Now())
	time.Sleep(50 * time.Millisecond)
	if h.speaker.toneCount() != 0 || said.count() != 0 {
		t.Error("rang into a conversation at the speaker")
	}
	if !h.p.MobileRinging() {
		t.Error("the ring should still show it")
	}
}

// The phone out of reach: powering fails after seconds of paging, so it is not tried on every
// poll but again after mobilePowerRetry.
func TestMobilePowerWaitsWhileThePhoneIsAway(t *testing.T) {
	h, hf, _, _ := mobileHarness(t)
	hf.away = true
	h.p.mob.mode = "on"
	now := time.Now()
	for i := 0; i < 10; i++ {
		h.p.pollMobile(now.Add(time.Duration(i) * mobilePoll))
	}
	if len(hf.powered) != 1 {
		t.Fatalf("tried to power %d times within %s, want once", len(hf.powered), 10*mobilePoll)
	}
	hf.away = false
	h.p.pollMobile(now.Add(mobilePowerRetry + time.Second))
	if len(hf.powered) != 2 || !hf.modem.Powered {
		t.Errorf("after the pause: tries %v, powered %v", hf.powered, hf.modem.Powered)
	}
}

// Auto (the default): the phone's own switch decides. It connects hands-free itself on coming
// home, and the assistant must leave that alone - cutting it made the phone give up (17.10.2026).
func TestMobileAutoLeavesThePhonesChoice(t *testing.T) {
	h, hf, _, _ := mobileHarness(t)
	hf.modem.Powered, hf.modem.Online = true, true // the phone connected itself
	for i := 0; i < 5; i++ {
		h.p.pollMobile(time.Now())
	}
	if len(hf.powered) != 0 || !h.p.mobileOnline() {
		t.Fatalf("auto touched the phone's connection: %v", hf.powered)
	}
	// "Anrufe wieder aufs Handy": off, and it stays off.
	r := &tools.Registry{}
	h.p.addMobileTools(r)
	r.Call("mobile_handsfree", `{"mode": "off"}`, tools.Context{Channel: tools.Console})
	h.p.pollMobile(time.Now())
	if hf.modem.Powered {
		t.Error("off did not disconnect")
	}
	// It holds over a restart of the assistant.
	dir := store.Dir
	h2 := newHarness(t, nil)
	store.Dir = dir // the same data partition
	h2.p.loadMobileMode()
	if h2.p.mob.mode != "off" {
		t.Errorf("mode after a restart %q, want off", h2.p.mob.mode)
	}
}

// The way to the phone at the pace of real time: a burst after a stall (the microphone's
// goroutine held up, aplay starting late) does not stay as delay - three seconds of it made the
// caller hear herself that late (17.10.2026).
func TestPacerKeepsTheWayToThePhoneLive(t *testing.T) {
	var p pacer
	start := time.Now()
	sent := 0
	for i := 0; i < 50; i++ { // a second's worth at once
		if _, ok := p.admit(start, frameLen); ok {
			sent++
		}
	}
	if max := int(scoLead/(20*time.Millisecond)) + 1; sent > max {
		t.Errorf("a burst let %d frames through, want at most %d", sent, max)
	}
	// At real time everything goes.
	var q pacer
	for i := 0; i < 500; i++ {
		if _, ok := q.admit(start.Add(time.Duration(i)*20*time.Millisecond), frameLen); !ok {
			t.Fatalf("frame %d at real time dropped", i)
		}
	}
}

func TestMobileKnockingCallKeepsTheRunningOne(t *testing.T) {
	running := hfCall{Path: "/c/1", State: "active"}
	knocking := hfCall{Path: "/c/2", State: "waiting"}
	if c, _ := mobileCurrent([]hfCall{knocking, running}); c.Path != running.Path {
		t.Errorf("picked %s, want the running call", c.Path)
	}
	if c, ok := mobileCurrent([]hfCall{{Path: "/c/3", State: "disconnected"}}); ok {
		t.Errorf("picked a disconnected call %s", c.Path)
	}
}

func TestMobileHandsfreeTool(t *testing.T) {
	h, hf, _, _ := mobileHarness(t)
	r := &tools.Registry{}
	h.p.addMobileTools(r)
	h.p.pollMobile(time.Now()) // oFono knows the paired phone
	if out := r.Call("mobile_handsfree", `{"mode": "on"}`, tools.Context{Channel: tools.Console}); !strings.HasPrefix(out, "On") {
		t.Errorf("on: %q", out)
	}
	h.p.pollMobile(time.Now())
	if out := r.Call("mobile_handsfree", `{"mode": "off"}`, tools.Context{Channel: tools.Console}); !strings.HasPrefix(out, "Off") {
		t.Errorf("off: %q", out)
	}
	h.p.pollMobile(time.Now())
	if len(hf.powered) != 2 || !hf.powered[0] || hf.powered[1] {
		t.Errorf("powered %v, want [true false]", hf.powered)
	}
	// Without ofonod the tool says so instead of promising, and why (here: never switched on).
	hf.down = true
	h.p.pollMobile(time.Now())
	if out := r.Call("mobile_handsfree", `{"mode": "on"}`, tools.Context{Channel: tools.Console}); !strings.Contains(out, "do not work") ||
		!strings.Contains(out, "not switched on for this speaker") {
		t.Errorf("without oFono: %q", out)
	}
	if out := r.Call("mobile_call", `{"action": "answer"}`, tools.Context{Channel: tools.Console}); !strings.HasPrefix(out, "Error") {
		t.Errorf("answer without a call: %q", out)
	}
}
