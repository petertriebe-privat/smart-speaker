package phone

import (
	"fmt"
	"sync"
	"time"

	"echo-assistant/internal/prompts"
)

// The microphone at home on its way to the line. The DSP hands over a beamformed, echo-cancelled
// stream - good enough for GPT Live, not good enough for a telephone line, for two reasons:
//
//   - What the DSP leaves of the echo is inaudible to a model (measured as RMS 6-20 against
//     The assistant's own voice, hardware/gen1/docs/audio.md) but plainly audible to a person on the other
//     end, because it comes back to them half a second late: the round trip is the speaker's
//     250 ms jitter buffer plus the DSP plus RTP both ways. Delayed echo is the one thing a
//     speakerphone must not have, so while the other side is audible in the room, the microphone
//     is turned down hard on its way out (gate). That is what every speakerphone does; the price
//     is that talking at the same time is damped.
//   - The level is made for a far-field microphone in a room, not for a line: a quiet talker two
//     metres away arrives at the other end as a whisper. So the way out gets its own slow gain
//     control that lifts quiet speech and never lets it clip.
//
// Both only in the direction room -> line. What comes in from the line is not touched.
var (
	micTarget  = prompts.Int("PHONE_MIC_TARGET", 11000)   // peak the gain control aims for
	micMaxGain = prompts.Int("PHONE_MIC_MAX_GAIN", 8)     // never louder than this
	micFloor   = prompts.Int("PHONE_MIC_FLOOR", 150)      // below this it is room noise, not speech
	micGatePct = prompts.Int("PHONE_MIC_GATE_PCT", 10)    // how much is left while the gate is shut
	micEchoTap = ms("PHONE_MIC_GATE_MS", 700)             // the gate stays shut this long after the room was loud
	micDecay   = prompts.Int("PHONE_MIC_PEAK_DECAY", 150) // how fast the measured peak falls per 20 ms
)

// micChain: the gain control and the gate. One goroutine (the microphone's) uses it; report may
// come from another.
type micChain struct {
	gain float32
	peak int

	mu    sync.Mutex // the counts below, for report
	stats micStats
}

// micStats: what the way out did since the last report (the end of a call or intercom), so a
// call the other side hardly heard can be told apart afterwards: gate shut most of the time, or
// the room too quiet even with the gain control at its limit.
type micStats struct {
	frames, gated int
	gainSum       float64 // over the open frames
	gainMax       float32
	speech        int // loudest raw frame while the gate was open
	speechFrames  int // open frames above micFloor: someone spoke in the room
}

func newMicChain() *micChain { return &micChain{gain: 1} }

// process returns the block the way it should go onto the line. gated: the other side is
// audible in the room right now, so what the microphone hears is mostly its echo.
func (m *micChain) process(pcm []int16, gated bool) []int16 {
	frame := 0
	for _, v := range pcm {
		frame = max(frame, int(v), -int(v))
	}
	// The gain control learns only from what the room itself says: while the gate is shut, the
	// microphone hears the other side, and lifting that would be exactly wrong.
	if !gated {
		m.peak = max(frame, m.peak-micDecay)
		if m.peak > micFloor {
			want := float32(micTarget) / float32(m.peak)
			if want > float32(micMaxGain) {
				want = float32(micMaxGain)
			}
			if want < 1 {
				want = 1
			}
			// Slowly, so a single loud word does not duck the whole room.
			step := float32(0.1)
			if want < m.gain {
				step = 0.02 // down even more slowly than up
			}
			m.gain += (want - m.gain) * step
		}
	}
	gain := m.gain
	if gated {
		// A share of the raw microphone, not of the lifted one: the gain control lifts up to
		// eightfold, and a tenth of that let the echo through almost unchanged (the caller heard
		// themselves at once in the second intercom test, 17.10.2026).
		gain = min(gain, 1) * float32(micGatePct) / 100
	}
	out := make([]int16, len(pcm))
	for i, v := range pcm {
		out[i] = clamp16(float32(v) * gain)
	}
	m.mu.Lock()
	s := &m.stats
	s.frames++
	if gated {
		s.gated++
	} else {
		s.gainSum += float64(m.gain)
		s.gainMax = max(s.gainMax, m.gain)
		s.speech = max(s.speech, frame)
		if frame > micFloor {
			s.speechFrames++
		}
	}
	m.mu.Unlock()
	return out
}

// Gain: what the gain control is doing right now, for the console.
func (m *micChain) Gain() float32 { return m.gain }

// report: the way out since the last report, for the log ("" when nothing went out); starts
// counting anew.
func (m *micChain) report() string {
	m.mu.Lock()
	s := m.stats
	m.stats = micStats{}
	m.mu.Unlock()
	if s.frames == 0 {
		return ""
	}
	mean := 0.0
	if open := s.frames - s.gated; open > 0 {
		mean = s.gainSum / float64(open)
	}
	return fmt.Sprintf("%d frames, gate shut %d %%, gain mean %.1f max %.1f, room peak %d (%d frames above %d)",
		s.frames, s.gated*100/s.frames, mean, s.gainMax, s.speech, s.speechFrames, micFloor)
}

// --- what the room hears, for the gate -------------------------------------------------------

// roomEcho: when the other side was last audible in the room (bridge.play, Speakerphone.Played).
// The gate on the way out is kept shut for a moment longer, because the speaker plays what was
// queued only about its latency later, and the room keeps ringing for a moment after that.
type roomEcho struct {
	mu    sync.Mutex
	last  time.Time
	floor int // the line's noise: the quietest frames of late, see heard

	frames, loud, floorMax int // since the last report
}

// gateAudible: a frame of the call this loud shuts the gate on a quiet line. Below audible (300,
// "speaking" for the line): the quiet ends of words went out ungated, and lifted up to eightfold
// by the gain control, their echo too.
//
// A line is not always quiet between words, though: a mobile phone in the street or a car sends
// its noise, and at a peak of 100 (-50 dBFS) that noise alone can keep the gate shut for the
// whole call - the room then reaches the caller at a tenth (most likely why the owner hardly
// understood the room on 7.11.2026). So a frame shuts the gate only when it is clearly above the
// line's own noise (gateOverNoise times), and the noise cannot raise the threshold beyond
// gateMax: speech on a telephone line peaks at several thousand, so its echo still shuts it.
const gateAudible = 100

var (
	gateOverNoise = prompts.Int("PHONE_MIC_GATE_OVER_NOISE", 4)
	gateMax       = prompts.Int("PHONE_MIC_GATE_MAX", 1000)
)

// gateRise: how slowly the noise floor follows a louder line (per 20 ms frame it climbs by
// 1/gateRise of the distance, about 5 s): words hardly lift it, and the pause after them brings
// it down again at once.
const gateRise = 250

// threshold: the peak above which a frame of the call is the other side's voice in the room.
func (r *roomEcho) threshold() int {
	return min(max(gateAudible, r.floor*gateOverNoise), max(gateMax, gateAudible))
}

// heard: a frame of the call goes to the speaker, which plays it after `later` (what it still
// holds). The gate counts from when it is audible.
func (r *roomEcho) heard(peak int, later time.Duration) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if peak < r.floor {
		r.floor = peak
	} else {
		r.floor = min(peak, r.floor+max(1, (peak-r.floor)/gateRise))
	}
	r.frames++
	r.floorMax = max(r.floorMax, r.floor)
	if peak <= r.threshold() {
		return
	}
	r.loud++
	r.last = time.Now().Add(later)
}

// active: is the room still full of the other side's voice?
func (r *roomEcho) active() bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return !r.last.IsZero() && time.Since(r.last) < micEchoTap
}

// report: what the room heard of the line since the last report, for the log; starts anew.
func (r *roomEcho) report() string {
	r.mu.Lock()
	defer r.mu.Unlock()
	frames, loud, floorMax := r.frames, r.loud, r.floorMax
	r.frames, r.loud, r.floorMax = 0, 0, 0
	if frames == 0 {
		return ""
	}
	return fmt.Sprintf("the other side: %d frames, %d %% loud, noise up to %d", frames, loud*100/frames, floorMax)
}
