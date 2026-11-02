package audio

import "testing"

func TestUpsamplerRate(t *testing.T) {
	var u Upsampler
	in := make([]int16, 16000)
	out := u.Process(in, nil)
	if len(out) != 24000 {
		t.Fatalf("1 s at 16 kHz gave %d samples, want 24000", len(out))
	}
}

func TestUpsamplerInterpolates(t *testing.T) {
	var u Upsampler
	// A ramp stays a ramp: 0, 300, 600 ... -> steps of 200 at 24 kHz (after the one-sample
	// start-up delay from the initial previous sample 0).
	in := []int16{0, 300, 600, 900, 1200, 1500}
	out := u.Process(in, nil)
	for i := 1; i < len(out); i++ {
		if d := int(out[i]) - int(out[i-1]); i > 2 && d != 200 {
			t.Fatalf("step %d: %v", i, out)
		}
	}
}

func TestUpsamplerChunksMatchWhole(t *testing.T) {
	in := make([]int16, 999)
	for i := range in {
		in[i] = int16(i * 7 % 3000)
	}
	var a, b Upsampler
	whole := a.Process(in, nil)
	var parts []int16
	for i := 0; i < len(in); i += 37 {
		parts = b.Process(in[i:min(i+37, len(in))], parts)
	}
	if len(whole) != len(parts) {
		t.Fatalf("lengths %d vs %d", len(whole), len(parts))
	}
	for i := range whole {
		if whole[i] != parts[i] {
			t.Fatalf("sample %d differs", i)
		}
	}
}
