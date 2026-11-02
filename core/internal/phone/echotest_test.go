package phone

import (
	"math"
	"strings"
	"testing"
	"time"
)

// speechLike: an envelope that rises and falls like syllables, 3 s.
func speechLike() []float64 {
	env := make([]float64, 300)
	for i := range env {
		env[i] = 0.1 * math.Abs(math.Sin(float64(i)/7)) * (1 + 0.5*math.Sin(float64(i)/23))
	}
	return env
}

// The analysis: one sentence back after 1.2 s at -20 dB, one on two paths (quiet after 300 ms,
// loud after 1.5 s), one not at all.
func TestEchoTestFindsDelaysAndASecondPath(t *testing.T) {
	sent := speechLike()
	e := newEchoProbe()
	e.start = time.Now()
	e.env = make([]float64, 3000) // 30 s of a quiet line
	for i := range e.env {
		e.env[i] = 1e-5
	}
	put := func(at, lag int, gain float64) {
		for i, v := range sent {
			e.env[at+lag+i] += v * gain
		}
	}
	step := 10 * time.Millisecond
	for i, at := range []int{100, 900, 1700} {
		e.sent = append(e.sent, e.start.Add(time.Duration(at)*step))
		switch i {
		case 0:
			put(at, 120, 0.1)
		case 1:
			put(at, 30, 0.05)
			put(at, 150, 0.3)
		}
	}
	out := e.result(sent)
	if !strings.HasPrefix(out, "2 of 3 sentences came back, 1 on two paths") {
		t.Fatalf("result %q", out)
	}
	if !strings.Contains(out, "sentence 1: echo after 1.2s at -20 dB") {
		t.Errorf("first sentence: %q", out)
	}
	if !strings.Contains(out, "after 1.5s") || !strings.Contains(out, "after 300ms") {
		t.Errorf("the two paths: %q", out)
	}
	if !strings.Contains(out, "sentence 3: no echo") {
		t.Errorf("third sentence: %q", out)
	}
}
