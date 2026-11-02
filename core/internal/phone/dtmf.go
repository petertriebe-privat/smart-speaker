package phone

import "math"

// dtmfDetector finds touch tones in the caller's audio (8 kHz), the fallback for calls without
// RFC 4733 telephone-event. Blocks of 20 ms are analysed with the Goertzel algorithm; a key counts
// once when its tones appear, until a block without tones.
type dtmfDetector struct {
	block []int16
	last  byte
}

// Feed takes decoded line samples and returns the keys that started within them.
func (d *dtmfDetector) Feed(samples []int16) []byte {
	var keys []byte
	d.block = append(d.block, samples...)
	for len(d.block) >= frameLen {
		key := detectBlock(d.block[:frameLen])
		d.block = d.block[frameLen:]
		if key != d.last && key != 0 {
			keys = append(keys, key)
		}
		d.last = key
	}
	return keys
}

func goertzel(block []int16, freq float64) float64 {
	coeff := 2 * math.Cos(2*math.Pi*freq/lineRate)
	var s1, s2 float64
	for _, v := range block {
		s := float64(v) + coeff*s1 - s2
		s2, s1 = s1, s
	}
	return s1*s1 + s2*s2 - coeff*s1*s2
}

// detectBlock: the key whose two tones carry nearly all of the block's energy, 0 if none.
func detectBlock(block []int16) byte {
	var energy float64
	for _, v := range block {
		energy += float64(v) * float64(v)
	}
	n := float64(len(block))
	if energy/n < 40000 { // rms below about 200: no tone, line noise
		return 0
	}
	norm := n / 2 * energy // a pure tone gives 1
	bestLow, bestHigh := -1, -1
	var powerLow, powerHigh float64
	lows, highs := []float64{697, 770, 852, 941}, []float64{1209, 1336, 1477}
	for i, f := range lows {
		if p := goertzel(block, f) / norm; p > powerLow {
			powerLow, bestLow = p, i
		}
	}
	for i, f := range highs {
		if p := goertzel(block, f) / norm; p > powerHigh {
			powerHigh, bestHigh = p, i
		}
	}
	if bestLow < 0 || bestHigh < 0 || powerLow < 0.2 || powerHigh < 0.2 || powerLow+powerHigh < 0.7 {
		return 0
	}
	for k := range dtmfKeys {
		if dtmfLow[k] == lows[bestLow] && dtmfHigh[k] == highs[bestHigh] {
			return dtmfKeys[k]
		}
	}
	return 0
}
