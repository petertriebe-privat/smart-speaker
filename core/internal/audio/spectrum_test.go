package audio

import (
	"math"
	"testing"
)

const musicRate = 44100

// tone: one 20 ms block of a mono sine.
func tone(freq float64, start, rate int) []int16 {
	pcm := make([]int16, rate/50)
	for i := range pcm {
		pcm[i] = int16(12000 * math.Sin(2*math.Pi*freq*float64(start+i)/float64(rate)))
	}
	return pcm
}

// BenchmarkSpectrumFeed: one 20 ms block (50 per second of music).
func BenchmarkSpectrumFeed(b *testing.B) {
	s := NewSpectrum(musicRate, MusicBands)
	buf := tone(440, 0, musicRate)
	for i := 0; i < b.N; i++ {
		s.Feed(buf)
	}
}

func TestSpectrumBands(t *testing.T) {
	for _, c := range []struct {
		freq float64
		band int
	}{{55, 0}, {400, 2}, {2500, 4}, {10000, 6}} {
		s := NewSpectrum(musicRate, MusicBands)
		for b := 0; b < 100; b++ { // 2 s (analysed every second block)
			s.Feed(tone(c.freq, b*musicRate/50, musicRate))
		}
		if best, level := loudestBand(s); best != c.band || level < 0.9 {
			t.Errorf("%v Hz: loudest band %d (%.2f), want %d; %v", c.freq, best, level, c.band, s.Levels())
		}
	}
}

// The bands of a phone call sit in the telephone band: a voice moves all seven of them, where
// the music edges would leave five lights dark for good.
func TestSpectrumBandsOfACall(t *testing.T) {
	const callRate = 24000
	for _, c := range []struct {
		freq float64
		band int
	}{{150, 0}, {560, 2}, {1300, 4}, {3000, 6}} {
		s := NewSpectrum(callRate, PhoneBands)
		for b := 0; b < 100; b++ {
			s.Feed(tone(c.freq, b*callRate/50, callRate))
		}
		if best, level := loudestBand(s); best != c.band || level < 0.9 {
			t.Errorf("%v Hz: loudest band %d (%.2f), want %d; %v", c.freq, best, level, c.band, s.Levels())
		}
	}
	// Silence leaves the ring dark.
	s := NewSpectrum(callRate, PhoneBands)
	for b := 0; b < 100; b++ {
		s.Feed(make([]int16, callRate/50))
	}
	for band, level := range s.Levels() {
		if level > 0.2 {
			t.Errorf("silence lit band %d with %.2f", band, level)
		}
	}
}

func loudestBand(s *Spectrum) (int, float64) {
	levels := s.Levels()
	best := 0
	for i := range levels {
		if levels[i] > levels[best] {
			best = i
		}
	}
	return best, levels[best]
}
