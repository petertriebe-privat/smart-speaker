package audio

import (
	"encoding/binary"
	"io"
	"math"
	"os"
	"sync"
	"time"
)

const (
	SpeakerRate  = 24000 // GPT Live's PCM rate; PulseAudio (the Dot: dot.go) resamples to 48 kHz
	pulseRun     = "/mnt/echo-audio"
	jitterBuffer = 250 * time.Millisecond // pacat latency, as JITTER_MS on the M5
)

// Speaker plays 24 kHz mono PCM through one stream (NewStream: a pacat client on the Echo). It
// tracks when the queued audio ends, so the session knows whether the assistant is audible.
type Speaker struct {
	mu       sync.Mutex
	pipe     io.WriteCloser
	endsAt   time.Time // when everything written so far has been played
	audible  time.Time // when the last packet above SilencePeak has been played
	underrun int
	Latency  time.Duration // pacat buffer (250 ms against network jitter, as JITTER_MS on the M5) + DSP; to measure
	// GPT Live streams silence in real time too: only packets with a peak above this count as
	// speech (SILENCE_PEAK on the M5).
	SilencePeak int
	gain        float64
	levels      []levelMark // recent output levels, for Level
}

type levelMark struct {
	at    time.Time // when it is written to PulseAudio's buffer (audible Latency later)
	level float64   // peak 0..1 before the volume
}

func NewSpeaker(silencePeak int) *Speaker {
	return &Speaker{Latency: 300 * time.Millisecond, SilencePeak: silencePeak, gain: 1}
}

// PulseEnv: the environment pacat needs to reach the PulseAudio server.
func PulseEnv() []string {
	return append(os.Environ(), "PULSE_SERVER=unix:"+pulseRun+"/pulse-native", "PULSE_COOKIE="+pulseRun+"/pulse-cookie")
}

func (s *Speaker) startLocked() error {
	pipe, err := NewStream(SpeakerRate, 1, jitterBuffer, "voice")
	if err != nil {
		return err
	}
	s.pipe = pipe
	return nil
}

// Queue appends samples for playback. Blocks only if PulseAudio's buffer is full.
func (s *Speaker) Queue(pcm []int16) error {
	if len(pcm) == 0 {
		return nil
	}
	peak := 0 // before the volume: speech stays speech when turned down
	for _, v := range pcm {
		peak = max(peak, abs(int(v)))
	}
	pcm = s.scale(pcm)
	buf := make([]byte, len(pcm)*2)
	for i, v := range pcm {
		binary.LittleEndian.PutUint16(buf[i*2:], uint16(v))
	}
	s.mu.Lock()
	if s.pipe == nil {
		if err := s.startLocked(); err != nil {
			s.mu.Unlock()
			return err
		}
	}
	now := time.Now()
	if s.endsAt.Before(now) {
		// Ran dry in the middle of speech (not after a pause): later than pacat's jitter buffer
		// can bridge.
		if late := now.Sub(s.endsAt); !s.audible.IsZero() && s.audible.Equal(s.endsAt) &&
			late > jitterBuffer && late < 2*time.Second {
			s.underrun++
		}
		s.endsAt = now
	}
	// The packet's level, for the light ring: per 50 ms, at the time it is played.
	slice := SpeakerRate / 20
	for i := 0; i < len(pcm); i += slice {
		p := 0
		for _, v := range pcm[i:min(i+slice, len(pcm))] {
			p = max(p, abs(int(v)))
		}
		at := s.endsAt.Add(time.Duration(i) * time.Second / SpeakerRate)
		s.levels = append(s.levels, levelMark{at, math.Min(1, float64(p)/32767/math.Max(s.gain, 0.01))}) // before the volume
	}
	if len(s.levels) > 400 {
		s.levels = append(s.levels[:0], s.levels[len(s.levels)-200:]...)
	}
	s.endsAt = s.endsAt.Add(time.Duration(len(pcm)) * time.Second / SpeakerRate)
	if peak >= s.SilencePeak {
		s.audible = s.endsAt
	}
	pipe := s.pipe
	s.mu.Unlock()
	_, err := pipe.Write(buf)
	if err != nil {
		s.Stop()
	}
	return err
}

// Level: how loud the voice is right now (0..1), for the light ring.
func (s *Speaker) Level() float64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	t := time.Now().Add(-s.Latency - dotExtraLatency())
	for i := len(s.levels) - 1; i >= 0; i-- {
		if !s.levels[i].at.After(t) {
			if t.Sub(s.levels[i].at) > 100*time.Millisecond {
				return 0
			}
			return s.levels[i].level
		}
	}
	return 0
}

// Queued: audio still waiting to be played.
func (s *Speaker) Queued() time.Duration {
	s.mu.Lock()
	defer s.mu.Unlock()
	if d := time.Until(s.endsAt); d > 0 {
		return d
	}
	return 0
}

// Speaking: something is audible now, or still within tail after the end.
func (s *Speaker) Speaking(tail time.Duration) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return time.Now().Before(s.audible.Add(s.Latency + dotExtraLatency() + tail))
}

// Flush drops everything queued (the user interrupted, or the session failed).
func (s *Speaker) Flush() { s.Stop() }

// Stop ends the pacat client; the next Queue starts a new one.
func (s *Speaker) Stop() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.pipe != nil {
		s.pipe.Close()
		s.pipe = nil
	}
	s.endsAt, s.audible = time.Time{}, time.Time{}
}

// Underruns since start: the speaker ran dry between packets.
func (s *Speaker) Underruns() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.underrun
}

// Tone queues a sine beep (status sounds).
func (s *Speaker) Tone(freq float64, ms int, amp float64) { s.Queue(beep(freq, ms, amp)) }

// Cue: a beep that does not count as the assistant speaking (Speaking stays false), so the microphone
// paths that wait for him to be quiet keep listening: the wake word's cue, under the user's
// breath before the command (a pure tone is no word for the transcription).
func (s *Speaker) Cue(freq float64, ms int, amp float64) {
	s.mu.Lock()
	audible := s.audible
	s.mu.Unlock()
	s.Queue(beep(freq, ms, amp))
	s.mu.Lock()
	s.audible = audible
	s.mu.Unlock()
}

func beep(freq float64, ms int, amp float64) []int16 {
	n := SpeakerRate * ms / 1000
	// Silence after it: PulseAudio starts a stream only once its buffer (jitterBuffer) is full,
	// so a short tone alone stayed silent until the next one came (16.10.2026).
	pcm := make([]int16, n+int(2*jitterBuffer/time.Millisecond)*SpeakerRate/1000)
	ramp := SpeakerRate / 200 // 5 ms fade in and out against clicks
	for i := 0; i < n; i++ {
		g := 1.0
		if i < ramp {
			g = float64(i) / float64(ramp)
		} else if n-i < ramp {
			g = float64(n-i) / float64(ramp)
		}
		pcm[i] = int16(32767 * amp * g * math.Sin(2*math.Pi*freq*float64(i)/SpeakerRate))
	}
	return pcm
}

// SetVolume: 0..100, applied as a gain on our own samples. The DSP's MainVolume stays at the
// level echo-audio.sh set at start: its LIPC property (served by audioCtrl) deadlocked during
// the tests of 16.10.2026 and then timed out after 10-20 s on every call.
func (s *Speaker) SetVolume(level int) {
	level = max(0, min(100, level))
	f := float64(level) / 100
	s.mu.Lock()
	s.gain = f * f // roughly even loudness steps
	s.mu.Unlock()
}

func (s *Speaker) scale(pcm []int16) []int16 {
	s.mu.Lock()
	g := OutputGain(s.gain)
	s.mu.Unlock()
	if g >= 0.999 {
		return pcm
	}
	out := make([]int16, len(pcm))
	for i, v := range pcm {
		out[i] = int16(float64(v) * g)
	}
	return out
}
