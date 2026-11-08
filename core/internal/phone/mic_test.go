package phone

import (
	"strings"
	"testing"
	"time"
)

func peakOf(pcm []int16) int {
	peak := 0
	for _, v := range pcm {
		peak = max(peak, int(v), -int(v))
	}
	return peak
}

func TestMicGainLiftsQuietSpeech(t *testing.T) {
	m := newMicChain()
	quiet := sine(400, micRate, micFrame, 1500) // someone two metres from the microphone
	out := quiet
	for i := 0; i < 200; i++ { // 4 s
		out = m.process(quiet, false)
	}
	if got := peakOf(out); got < micTarget*8/10 || got > micTarget*12/10 {
		t.Errorf("quiet speech came out at %d, want around %d", got, micTarget)
	}
	if m.Gain() < 2 {
		t.Errorf("gain %.1f", m.Gain())
	}
	// Loud speech is not amplified, and never clips.
	m2 := newMicChain()
	loud := sine(400, micRate, micFrame, 20000)
	for i := 0; i < 200; i++ {
		out = m2.process(loud, false)
	}
	if got := peakOf(out); got > 32000 {
		t.Errorf("loud speech came out at %d", got)
	}
	if m2.Gain() > 1.01 {
		t.Errorf("loud speech was amplified by %.2f", m2.Gain())
	}
	// Room noise alone must not be lifted to speech level.
	m3 := newMicChain()
	for i := 0; i < 200; i++ {
		out = m3.process(sine(400, micRate, micFrame, 60), false)
	}
	if got := peakOf(out); got > 1000 {
		t.Errorf("room noise came out at %d", got)
	}
}

func TestMicGateDampsTheEcho(t *testing.T) {
	m := newMicChain()
	speech := sine(400, micRate, micFrame, 8000)
	for i := 0; i < 100; i++ {
		m.process(speech, false)
	}
	open := peakOf(m.process(speech, false))
	shut := peakOf(m.process(speech, true))
	if shut*100 > open*(micGatePct+2) {
		t.Errorf("the gate left %d of %d, want about %d %%", shut, open, micGatePct)
	}
	// What the microphone hears while the gate is shut is mostly echo: the gain control must not
	// learn from it and wind itself up.
	before := m.Gain()
	for i := 0; i < 200; i++ {
		m.process(sine(400, micRate, micFrame, 50), true)
	}
	if m.Gain() != before {
		t.Errorf("the gain control learned from the echo: %.2f -> %.2f", before, m.Gain())
	}
	// After quiet speech lifted the gain, the gate still leaves only its share of the raw
	// microphone: a tenth of an eightfold gain let the echo through almost unchanged.
	lifted := newMicChain()
	quiet := sine(400, micRate, micFrame, 1000)
	for i := 0; i < 300; i++ {
		lifted.process(quiet, false)
	}
	if lifted.Gain() < 4 {
		t.Fatalf("gain %.1f, the test needs a lifted one", lifted.Gain())
	}
	echo := sine(400, micRate, micFrame, 3000)
	if got := peakOf(lifted.process(echo, true)); got*100 > 3000*(micGatePct+2) {
		t.Errorf("with gain %.1f the gate left %d of 3000, want about %d %%", lifted.Gain(), got, micGatePct)
	}
}

// A caller in the street: the line carries noise between the words (peaks around 300). The
// noise alone must not keep the gate shut - it did, and the room reached the caller at a tenth
// (7.11.2026) - but every word of the caller still shuts it.
func TestRoomEchoNoisyLineLeavesTheGateOpen(t *testing.T) {
	var r roomEcho
	noise := func(i int) int { return 250 + (i*37)%100 } // 250..349
	for i := 0; i < 150; i++ {                           // 3 s to learn the line
		r.heard(noise(i), 0)
	}
	r.report()
	for i := 0; i < 100; i++ {
		r.heard(noise(i), 0)
	}
	if r.loud != 0 {
		t.Errorf("the line's noise shut the gate %d times in 2 s (threshold %d)", r.loud, r.threshold())
	}
	// Two seconds of the caller's speech: every frame counts, the floor does not climb past it.
	for i := 0; i < 100; i++ {
		r.heard(3000+(i*53)%2000, 0)
	}
	if r.loud != 100 {
		t.Errorf("speech shut the gate in %d of 100 frames (threshold %d)", r.loud, r.threshold())
	}
	if r.threshold() > gateMax {
		t.Errorf("threshold %d above the cap %d", r.threshold(), gateMax)
	}
	// After the words the noise brings the floor back down at once.
	r.heard(300, 0)
	if r.threshold() > 300*gateOverNoise {
		t.Errorf("threshold %d after the words, want at most %d", r.threshold(), 300*gateOverNoise)
	}
	if got := r.report(); !strings.Contains(got, "201 frames") {
		t.Errorf("report %q", got)
	}
	if r.report() != "" {
		t.Error("the report did not start anew")
	}
	// A digitally silent line (most mobile calls): the old threshold, quiet word ends count.
	var q roomEcho
	for i := 0; i < 100; i++ {
		q.heard(0, 0)
	}
	q.heard(gateAudible+50, 0)
	if !q.active() {
		t.Error("on a silent line a quiet word end did not shut the gate")
	}
}

func TestMicReportTellsWhyTheRoomWasQuiet(t *testing.T) {
	m := newMicChain()
	if m.report() != "" {
		t.Error("a report without frames")
	}
	quiet := sine(400, micRate, micFrame, 1000)
	for i := 0; i < 30; i++ {
		m.process(quiet, false)
	}
	for i := 0; i < 70; i++ {
		m.process(quiet, true)
	}
	got := m.report()
	for _, want := range []string{"100 frames", "gate shut 70 %", "room peak 1000"} {
		if !strings.Contains(got, want) {
			t.Errorf("report %q lacks %q", got, want)
		}
	}
}

func TestRoomEchoKeepsTheGateShutForAMoment(t *testing.T) {
	var r roomEcho
	if r.active() {
		t.Error("the gate is shut although the room was never loud")
	}
	r.heard(gateAudible/2, 0) // silence on the line does not shut it
	if r.active() {
		t.Error("quiet audio shut the gate")
	}
	r.heard(12000, 0)
	if !r.active() {
		t.Error("the gate did not shut although the room is loud")
	}
	// It stays shut for a while: the speaker plays what was queued only about its latency later.
	r.mu.Lock()
	r.last = time.Now().Add(-micEchoTap / 2)
	r.mu.Unlock()
	if !r.active() {
		t.Error("the gate opened too early")
	}
	r.mu.Lock()
	r.last = time.Now().Add(-micEchoTap - time.Second)
	r.mu.Unlock()
	if r.active() {
		t.Error("the gate stayed shut")
	}
}

// The whole way out: a quiet room is lifted, and while the other side is audible in the room
// almost nothing goes back onto the line.
func TestMicPathFromTheRoomToTheLine(t *testing.T) {
	h := newHarness(t, nil)
	h.talking(t)
	h.p.SetBridge("talk")
	quiet := sine(400, micRate, micFrame, 1200)
	for i := 0; i < 150; i++ {
		h.p.Hear(quiet)
	}
	h.p.call.line.mu.Lock()
	lifted := peakOf(h.p.call.line.mic)
	h.p.call.line.mic = nil
	h.p.call.line.mu.Unlock()
	if lifted < 4000 {
		t.Errorf("the room reaches the line with a peak of only %d", lifted)
	}
	// The other side is loud in the room: the way out is damped.
	h.p.bridge.echo.heard(20000, 0)
	for i := 0; i < 20; i++ {
		h.p.Hear(quiet)
	}
	h.p.call.line.mu.Lock()
	// Not the first frame: the resampler's filter still carries the loud samples from before.
	damped := peakOf(h.p.call.line.mic[apiFrame:])
	h.p.call.line.mu.Unlock()
	if damped*3 > lifted {
		t.Errorf("while the room is loud, %d still goes out (open: %d)", damped, lifted)
	}
}
