package phone

import (
	"math"
	"testing"
)

func TestAlawAllCodesRoundTrip(t *testing.T) {
	for c := 0; c < 256; c++ {
		if got := encodeAlaw(decodeAlaw(byte(c))); got != byte(c) {
			t.Errorf("A-law code %#x -> %d -> %#x", c, decodeAlaw(byte(c)), got)
		}
	}
}

func TestUlawCodesRoundTrip(t *testing.T) {
	for c := 0; c < 256; c++ {
		if c == 0x7F { // negative zero decodes to 0 and encodes as positive zero (0xFF)
			continue
		}
		if got := encodeUlaw(decodeUlaw(byte(c))); got != byte(c) {
			t.Errorf("u-law code %#x -> %d -> %#x", c, decodeUlaw(byte(c)), got)
		}
	}
}

func TestG711Quantization(t *testing.T) {
	for _, enc := range []struct {
		name string
		e    func(int16) byte
		d    func(byte) int16
	}{{"alaw", encodeAlaw, decodeAlaw}, {"ulaw", encodeUlaw, decodeUlaw}} {
		for v := -32768; v <= 32767; v += 7 {
			got := int(enc.d(enc.e(int16(v))))
			// Relative error of the companding: at most 1/16 of the value plus the smallest step.
			if diff := math.Abs(float64(got - v)); diff > math.Abs(float64(v))/16+16 {
				t.Fatalf("%s: %d -> %d", enc.name, v, got)
			}
		}
	}
	if encodeAlaw(0) != 0xD5 || encodeUlaw(0) != 0xFF {
		t.Errorf("silence: %#x %#x", encodeAlaw(0), encodeUlaw(0))
	}
	// Reference values of the ITU tables.
	if got := decodeAlaw(0xD5); got != 8 {
		t.Errorf("decodeAlaw(0xD5) = %d", got)
	}
	if got := decodeUlaw(0xFF); got != 0 {
		t.Errorf("decodeUlaw(0xFF) = %d", got)
	}
}

func sine(freq float64, rate, n int, amp float64) []int16 {
	out := make([]int16, n)
	for i := range out {
		out[i] = int16(amp * math.Sin(2*math.Pi*freq*float64(i)/float64(rate)))
	}
	return out
}

func rms(pcm []int16) float64 {
	var sum float64
	for _, v := range pcm {
		sum += float64(v) * float64(v)
	}
	return math.Sqrt(sum / float64(len(pcm)))
}

func TestResampleLengths(t *testing.T) {
	var d Downsampler
	var u Upsampler
	if got := len(d.Process(make([]int16, 480))); got != 160 {
		t.Errorf("down: %d", got)
	}
	if got := len(u.Process(make([]int16, 160))); got != 480 {
		t.Errorf("up: %d", got)
	}
	// The phase carries over between calls that are no multiple of 3.
	total := len(d.Process(make([]int16, 4))) + len(d.Process(make([]int16, 5))) + len(d.Process(make([]int16, 3)))
	if total != 4 {
		t.Errorf("phase: %d", total)
	}
}

func TestResampleKeepsTonesInTheBand(t *testing.T) {
	// 1 kHz at 24 kHz -> 8 kHz keeps its level; 1 kHz at 8 kHz -> 24 kHz as well.
	var d Downsampler
	down := d.Process(sine(1000, apiRate, 2400, 10000))
	if got, want := rms(down[100:]), 10000/math.Sqrt2; math.Abs(got-want) > want*0.05 {
		t.Errorf("down rms %.0f, want %.0f", got, want)
	}
	var u Upsampler
	up := u.Process(sine(1000, lineRate, 800, 10000))
	if got, want := rms(up[300:]), 10000/math.Sqrt2; math.Abs(got-want) > want*0.05 {
		t.Errorf("up rms %.0f, want %.0f", got, want)
	}
}

func TestResampleRejectsAboveTheBand(t *testing.T) {
	// 6 kHz would fold back into the band if it were not filtered.
	var d Downsampler
	down := d.Process(sine(6000, apiRate, 2400, 10000))
	if got := rms(down[100:]); got > 300 {
		t.Errorf("6 kHz leaks through: rms %.0f", got)
	}
}

func TestFullPathThroughG711(t *testing.T) {
	var d Downsampler
	var u Upsampler
	in := sine(800, apiRate, 4800, 8000)
	pcm8 := d.Process(in)
	for i, s := range pcm8 {
		pcm8[i] = decodeAlaw(encodeAlaw(s))
	}
	out := u.Process(pcm8)
	if got, want := rms(out[600:]), rms(in[600:]); math.Abs(got-want) > want*0.1 {
		t.Errorf("rms %.0f, want about %.0f", got, want)
	}
}
