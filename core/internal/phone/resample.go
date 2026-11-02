package phone

import "math"

const (
	lineRate  = 8000  // G.711
	apiRate   = 24000 // GPT Live / live.Output
	micRate   = 16000 // the Echo's microphone (audio.MicRate), for talking along on the line
	factor    = apiRate / lineRate
	micUp     = apiRate / 8000    // 16 kHz -> 24 kHz as 3/2: insert 2 zeros ...
	micDown   = micRate / 8000    // ... and keep every second sample
	frameLen  = lineRate / 50     // 160 samples = 20 ms, one packet
	apiFrame  = frameLen * factor // the same 20 ms at 24 kHz
	micFrame  = micRate / 50      // the same 20 ms at 16 kHz
	firTaps   = 33
	firCutoff = 3600.0 / apiRate           // the telephone band
	micCutoff = 3600.0 / (micRate * micUp) // the same band in the 48 kHz step of 16 -> 24 kHz
)

// Low-pass at 24 kHz with a cutoff of 3.6 kHz for both directions: before keeping every third
// sample (else the assistant's sibilants fold back as noise) and after stuffing two zeros between the
// caller's samples (else the spectrum repeats above 4 kHz).
var taps = makeTaps(firCutoff)

// The same band for the microphone's 3/2 step, whose intermediate stream runs at 48 kHz.
var micTaps = makeTaps(micCutoff)

func makeTaps(firCutoff float64) [firTaps]float32 {
	var out [firTaps]float32
	var sum float64
	var raw [firTaps]float64
	for i := range raw {
		n := float64(i) - (firTaps-1)/2.0
		sinc := 2 * firCutoff
		if n != 0 {
			sinc = math.Sin(2*math.Pi*firCutoff*n) / (math.Pi * n)
		}
		window := 0.54 - 0.46*math.Cos(2*math.Pi*float64(i)/(firTaps-1)) // Hamming
		raw[i] = sinc * window
		sum += raw[i]
	}
	for i := range raw {
		out[i] = float32(raw[i] / sum) // gain 1 at 0 Hz
	}
	return out
}

// fir: the filter over a 24 kHz stream; keeps its history between packets. set picks another
// tap set (nil = taps, the 24 kHz one).
type fir struct {
	set     *[firTaps]float32
	history [firTaps]float32
	at      int
}

func (f *fir) push(x float32) {
	f.history[f.at] = x
	f.at = (f.at + 1) % firTaps
}

func (f *fir) output() float32 {
	set := &taps
	if f.set != nil {
		set = f.set
	}
	var y float32
	k := f.at
	for i := 0; i < firTaps; i++ {
		if k == 0 {
			k = firTaps
		}
		k--
		y += set[i] * f.history[k]
	}
	return y
}

func clamp16(x float32) int16 {
	switch {
	case x > 32767:
		return 32767
	case x < -32768:
		return -32768
	}
	return int16(x)
}

// Downsampler: 24 kHz -> 8 kHz (The assistant's voice to the line).
type Downsampler struct {
	f     fir
	phase int
}

// Process filters in and keeps every third sample; the phase carries over between calls.
func (d *Downsampler) Process(in []int16) []int16 {
	out := make([]int16, 0, len(in)/factor+1)
	for _, v := range in {
		d.f.push(float32(v))
		d.phase++
		if d.phase == factor {
			d.phase = 0
			out = append(out, clamp16(d.f.output()))
		}
	}
	return out
}

// MicResampler: 16 kHz -> 24 kHz (the microphone at home onto the line, for talking along).
// 3/2 in one step: two zeros after each sample make 48 kHz, the filter takes the telephone band
// out of it, and every second sample is kept. The gain of 3 pays for the stuffed zeros; what is
// left above 3.6 kHz the Downsampler removes on the way to the line.
type MicResampler struct {
	f     fir
	phase int
}

func (m *MicResampler) Process(in []int16) []int16 {
	if m.f.set == nil {
		m.f.set = &micTaps
	}
	out := make([]int16, 0, len(in)*micUp/micDown+1)
	for _, v := range in {
		sample := micUp * float32(v)
		for k := 0; k < micUp; k++ {
			if k == 0 {
				m.f.push(sample)
			} else {
				m.f.push(0)
			}
			if m.phase++; m.phase == micDown {
				m.phase = 0
				out = append(out, clamp16(m.f.output()))
			}
		}
	}
	return out
}

// Upsampler: 8 kHz -> 24 kHz (the caller's voice to GPT Live).
type Upsampler struct {
	f fir
}

// Process stuffs two zeros after each sample and filters; the zeros take 2/3 of the energy,
// hence the gain of 3.
func (u *Upsampler) Process(in []int16) []int16 {
	out := make([]int16, 0, len(in)*factor)
	for _, v := range in {
		sample := factor * float32(v)
		for k := 0; k < factor; k++ {
			if k == 0 {
				u.f.push(sample)
			} else {
				u.f.push(0)
			}
			out = append(out, clamp16(u.f.output()))
		}
	}
	return out
}
