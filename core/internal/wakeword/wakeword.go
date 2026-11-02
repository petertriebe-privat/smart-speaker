// Package wakeword: wake word detection with a microWakeWord streaming model ("Hey Jarvis"), as
// devices/atoms3r/src/audio/wakeword.cpp, in pure Go: the microfrontend's features (frontend.go,
// fft.go) feed a small int8 TFLite interpreter (model.go, kernels.go). 16 kHz audio in, a
// detection out when the averaged probability of the last few inferences passes the cutoff.
package wakeword

import (
	_ "embed"
	"sync"
)

const warmupInferences = 34 // ~1 s: the streaming state needs history

// builtin: "Hey Jarvis" by Kevin Ahrendt (microWakeWord, v2; hey_jarvis.json), the M5's model.
//
//go:embed hey_jarvis.tflite
var builtin []byte

// Builtin: the "Hey Jarvis" model.
func Builtin() []byte { return builtin }

// Detector is one model listening to one audio stream. Feed is not safe for concurrent use.
type Detector struct {
	frontend *Frontend
	model    *Model
	cutoff   int // 0..255
	history  []uint8
	histPos  int
	strideAt int
	runs     int

	mu      sync.Mutex
	average int // latest averaged probability 0..255
	peak    int // highest average since the last Peak call

	// OnNearMiss: the probability rose above 0.3 and fell back without a detection (its peak
	// 0..1): to tune the cutoff for real voices. Called from Feed.
	OnNearMiss func(peak float64)
	episode    int // peak of the current rise, 0 = none
}

// New: model bytes (.tflite), cutoff 0..1 (M5: 0.85), sliding window in inferences (5).
func New(model []byte, cutoff float64, window int) (*Detector, error) {
	m, err := LoadModel(model)
	if err != nil {
		return nil, err
	}
	return &Detector{frontend: NewFrontend(), model: m, cutoff: int(cutoff * 255), history: make([]uint8, max(1, window))}, nil
}

// SetCutoff changes the threshold 0..1 (lower while music plays: the DSP's echo cancellation
// also takes much of the user's voice then). Call from the goroutine that feeds.
func (d *Detector) SetCutoff(cutoff float64) { d.cutoff = int(cutoff * 255) }

// Cutoff: the threshold in force (0..1).
func (d *Detector) Cutoff() float64 { return float64(d.cutoff) / 255 }

// Reset: start over (after a conversation): the warm-up runs again.
func (d *Detector) Reset() {
	d.frontend = NewFrontend()
	d.model.Reset()
	clear(d.history)
	d.strideAt, d.runs = 0, 0
}

// Feed takes 16 kHz samples; true if the wake word was heard in them.
func (d *Detector) Feed(samples []int16) bool {
	heard := false
	d.frontend.Process(samples, func(values []uint16) {
		if d.infer(values) {
			heard = true
		}
	})
	return heard
}

// Probability: the latest averaged probability and the highest since the last call (0..1).
func (d *Detector) Probability() (latest, peak float64) {
	d.mu.Lock()
	defer d.mu.Unlock()
	latest, peak = float64(d.average)/255, float64(d.peak)/255
	d.peak = 0
	return
}

// Latest: the latest averaged probability (0..1), the peak left as it is.
func (d *Detector) Latest() float64 {
	d.mu.Lock()
	defer d.mu.Unlock()
	return float64(d.average) / 255
}

func (d *Detector) infer(values []uint16) bool {
	in := d.model.Input()
	base := d.strideAt * features
	for i, v := range values {
		// As TFLM's micro_speech: feature * 256 / (25.6 * 26.0) - 128.
		q := (int32(v)*256+333)/666 - 128
		in.i8[base+i] = int8(clamp(q, -128, 127))
	}
	if d.strideAt++; d.strideAt < d.model.Stride() {
		return false
	}
	d.strideAt = 0
	d.history[d.histPos] = d.model.Invoke()
	d.histPos = (d.histPos + 1) % len(d.history)
	sum := 0
	for _, p := range d.history {
		sum += int(p)
	}
	avg := sum / len(d.history)
	d.mu.Lock()
	d.average, d.peak = avg, max(d.peak, avg)
	d.mu.Unlock()
	if d.runs++; d.runs < warmupInferences {
		return false
	}
	if avg > d.cutoff {
		clear(d.history) // one utterance triggers once
		d.runs, d.episode = 0, 0
		return true
	}
	switch {
	case avg >= 77: // 0.3
		d.episode = max(d.episode, avg)
	case avg < 26 && d.episode > 0: // back below 0.1
		if d.OnNearMiss != nil {
			d.OnNearMiss(float64(d.episode) / 255)
		}
		d.episode = 0
	}
	return false
}
