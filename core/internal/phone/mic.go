package phone

import (
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

// micChain: the gain control and the gate. One goroutine (the microphone's) uses it.
type micChain struct {
	gain float32
	peak int
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
	return out
}

// Gain: what the gain control is doing right now, for the console.
func (m *micChain) Gain() float32 { return m.gain }

// --- what the room hears, for the gate -------------------------------------------------------

// loud: the bridge played an audible frame into the room just now (bridge.frame). The gate on
// the way out is kept shut for a moment longer, because the speaker plays what was queued only
// about its latency later, and the room keeps ringing for a moment after that.
type roomEcho struct {
	mu   sync.Mutex
	last time.Time
}

// gateAudible: a frame of the call this loud shuts the gate. Below audible (300, "speaking" for
// the line): the quiet ends of words went out ungated, and lifted up to eightfold by the gain
// control, their echo too. A mobile line sends digital silence between words, so this does not
// keep the gate shut on noise.
const gateAudible = 100

// heard: a frame of the call goes to the speaker, which plays it after `later` (what it still
// holds). The gate counts from when it is audible.
func (r *roomEcho) heard(peak int, later time.Duration) {
	if peak <= gateAudible {
		return
	}
	r.mu.Lock()
	r.last = time.Now().Add(later)
	r.mu.Unlock()
}

// active: is the room still full of the other side's voice?
func (r *roomEcho) active() bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return !r.last.IsZero() && time.Since(r.last) < micEchoTap
}
