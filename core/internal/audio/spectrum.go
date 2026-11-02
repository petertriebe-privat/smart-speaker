package audio

import (
	"math"
	"math/cmplx"
	"sync"
)

// A small spectrum for the LED ring: music from librespot (package music) and a phone call that
// runs on the speaker (package phone). Every second 20 ms block (25 per second): the latest
// fftSize samples, mono at half the input rate, Hann window, FFT, the energy of SpectrumBands
// log-spaced bands in dB against one slowly falling peak (automatic gain), fast up and slower
// down so it looks like a level meter. ~0.3 ms per analysis on the Echo; the first version (1024
// points at 44.1 kHz, every block) took 1.7 ms, 8.5 % CPU (16.10.2026).

const (
	// SpectrumBands: the bands of the display, bass first (leds.Spectrum wants exactly these).
	SpectrumBands = 7
	fftSize       = 512
	dbRange       = 30.0 // shown below the running peak
)

// The band edges of the two sources. Music uses the whole range; a phone call carries only the
// telephone band (G.711, the line is filtered at 3.6 kHz), so its bands are squeezed into it -
// with the music edges five of seven lights would never move.
var (
	MusicBands = [SpectrumBands + 1]float64{40, 100, 250, 600, 1500, 3500, 7000, 10500}
	PhoneBands = [SpectrumBands + 1]float64{100, 250, 450, 700, 1100, 1700, 2500, 3500}
)

// Spectrum: the analysis of one source. Feed it from one goroutine, read it from any.
type Spectrum struct {
	mu     sync.Mutex
	levels [SpectrumBands]float64

	srate   float64 // analysed rate: half the input rate (two samples averaged)
	edges   [SpectrumBands + 1]float64
	window  [fftSize]float64
	twiddle [fftSize / 2]complex128
	history [fftSize]float64 // ring buffer of the latest mono samples at srate
	pos     int              // where the next sample goes (= the oldest)
	fill    int
	odd     bool    // a half sample waiting for its pair
	half    float64 // its value
	blocks  int
	peak    float64 // the loudest band in dB, falling slowly
	smooth  [SpectrumBands]float64
}

// NewSpectrum: inputRate is the rate of what Feed gets (44100 for librespot, 24000 for a call).
func NewSpectrum(inputRate int, edges [SpectrumBands + 1]float64) *Spectrum {
	s := &Spectrum{peak: -60, srate: float64(inputRate) / 2, edges: edges}
	for i := range s.window {
		s.window[i] = 0.5 - 0.5*math.Cos(2*math.Pi*float64(i)/fftSize)
	}
	for k := range s.twiddle {
		s.twiddle[k] = cmplx.Exp(complex(0, -2*math.Pi*float64(k)/fftSize))
	}
	return s
}

// Levels: the bands 0..1, bass first.
func (s *Spectrum) Levels() [SpectrumBands]float64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.levels
}

// Feed takes one block of mono samples at the input rate (20 ms at a time: the analysis runs
// every second block).
func (s *Spectrum) Feed(pcm []int16) {
	for _, sample := range pcm {
		v := float64(sample) / 32768
		if !s.odd {
			s.half, s.odd = v, true
			continue
		}
		s.odd = false
		s.history[s.pos] = (s.half + v) / 2
		s.pos = (s.pos + 1) % fftSize
		s.fill = min(fftSize, s.fill+1)
	}
	if s.blocks++; s.fill < fftSize || s.blocks%2 == 1 {
		return
	}
	var x [fftSize]complex128
	for i := range x { // oldest first: the ring buffer from pos
		x[i] = complex(s.history[(s.pos+i)%fftSize]*s.window[i], 0)
	}
	s.fft(x[:])
	var dbs [SpectrumBands]float64
	loudest := -120.0
	for b := 0; b < SpectrumBands; b++ {
		lo := int(s.edges[b] * fftSize / s.srate)
		hi := max(lo+1, int(s.edges[b+1]*fftSize/s.srate))
		energy := 0.0
		for k := lo; k < hi && k < fftSize/2; k++ {
			energy += real(x[k])*real(x[k]) + imag(x[k])*imag(x[k])
		}
		// voices and music have less energy higher up: +4 dB per band, so the treble shows too
		dbs[b] = 10*math.Log10(energy/float64(hi-lo)+1e-12) + 4*float64(b)
		loudest = math.Max(loudest, dbs[b])
	}
	// one reference for all bands (each its own made a pure tone light the whole ring),
	// falling ~2.5 dB per second
	s.peak = math.Max(loudest, s.peak-0.1)
	var levels [SpectrumBands]float64
	for b := 0; b < SpectrumBands; b++ {
		v := math.Max(0, math.Min(1, 1-(s.peak-dbs[b])/dbRange))
		if v > s.smooth[b] {
			s.smooth[b] += (v - s.smooth[b]) * 0.8
		} else {
			s.smooth[b] += (v - s.smooth[b]) * 0.3
		}
		levels[b] = s.smooth[b]
	}
	s.mu.Lock()
	s.levels = levels
	s.mu.Unlock()
}

// fft: in place, radix 2, len(x) == fftSize, with the precomputed twiddle factors.
func (s *Spectrum) fft(x []complex128) {
	n := len(x)
	for i, j := 1, 0; i < n; i++ {
		bit := n >> 1
		for ; j&bit != 0; bit >>= 1 {
			j ^= bit
		}
		j ^= bit
		if i < j {
			x[i], x[j] = x[j], x[i]
		}
	}
	for size := 2; size <= n; size <<= 1 {
		step := n / size
		for start := 0; start < n; start += size {
			for k := 0; k < size/2; k++ {
				a, b := x[start+k], x[start+k+size/2]*s.twiddle[k*step]
				x[start+k], x[start+k+size/2] = a+b, a-b
			}
		}
	}
}
