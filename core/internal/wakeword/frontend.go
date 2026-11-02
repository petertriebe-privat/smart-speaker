package wakeword

import (
	"math"
	"math/bits"
)

// The audio frontend every microWakeWord model is trained with: TFLite Micro's microfrontend
// (window, kissfft int16, mel filterbank, noise reduction, PCAN gain, log scale), ported to Go
// bit for bit, with the settings of devices/atoms3r/src/audio/wakeword.cpp: 30 ms window, 10 ms
// step, 40 channels 125-7500 Hz.

const (
	sampleRate = 16000
	features   = 40
	windowMs   = 30
	stepMs     = 10

	windowBits         = 12
	filterbankBits     = 12
	noiseReductionBits = 14
	pcanSnrBits        = 12
	pcanOutputBits     = 6
	wideDynamicBits    = 32
	logScaleLog2       = 16
	logSegmentsLog2    = 7
	logScale           = 65536
	logCoeff           = 45426

	smoothingBits      = 10
	evenSmoothing      = 0.025
	oddSmoothing       = 0.06
	minSignalRemaining = 0.05
	pcanStrength       = 0.95
	pcanOffset         = 80.0
	pcanGainBits       = 21
	logScaleShift      = 6
	lowerBand          = 125.0
	upperBand          = 7500.0
)

var logLut = [...]uint16{0, 224, 442, 654, 861, 1063, 1259, 1450, 1636, 1817, 1992, 2163,
	2329, 2490, 2646, 2797, 2944, 3087, 3224, 3358, 3487, 3611, 3732, 3848,
	3960, 4068, 4172, 4272, 4368, 4460, 4549, 4633, 4714, 4791, 4864, 4934,
	5001, 5063, 5123, 5178, 5231, 5280, 5326, 5368, 5408, 5444, 5477, 5507,
	5533, 5557, 5578, 5595, 5610, 5622, 5631, 5637, 5640, 5641, 5638, 5633,
	5626, 5615, 5602, 5586, 5568, 5547, 5524, 5498, 5470, 5439, 5406, 5370,
	5332, 5291, 5249, 5203, 5156, 5106, 5054, 5000, 4944, 4885, 4825, 4762,
	4697, 4630, 4561, 4490, 4416, 4341, 4264, 4184, 4103, 4020, 3935, 3848,
	3759, 3668, 3575, 3481, 3384, 3286, 3186, 3084, 2981, 2875, 2768, 2659,
	2549, 2437, 2323, 2207, 2090, 1971, 1851, 1729, 1605, 1480, 1353, 1224,
	1094, 963, 830, 695, 559, 421, 282, 142, 0, 0}

func msb32(n uint32) int { return 32 - bits.LeadingZeros32(n) }
func msb64(n uint64) int { return 64 - bits.LeadingZeros64(n) }

// Frontend turns 16 kHz samples into 40 feature values every 10 ms.
type Frontend struct {
	// window
	size, step   int
	coefficients []int16
	input        []int16
	inputUsed    int
	output       []int16
	// fft
	fft      *fftr
	fftSize  int
	fftIn    []int16
	spectrum []complex16
	energy   []int32
	// filterbank
	channels                     int
	startIndex, endIndex         int
	chanFreqStarts, chanWeightSt []int
	chanWidths                   []int
	weights, unweights           []int16
	work                         []uint64
	scaled                       []uint32
	// noise reduction
	even, odd, minSignal uint32
	estimate             []uint32
	// pcan
	gainLut  []int16 // indexed with the C code's offset (lut - 6 trick): see wideDynamic
	snrShift int
	out      []uint16
}

func NewFrontend() *Frontend {
	f := &Frontend{size: windowMs * sampleRate / 1000, step: stepMs * sampleRate / 1000, channels: features}
	// Window (Hann, as WindowPopulateState).
	f.coefficients = make([]int16, f.size)
	pi := float32(math.Pi)
	arg := pi * 2 / float32(f.size)
	for i := range f.coefficients {
		v := float32(0.5) - float32(0.5)*cosf(arg*(float32(i)+0.5))
		f.coefficients[i] = int16(floorf(v*float32(1<<windowBits) + 0.5))
	}
	f.input = make([]int16, f.size)
	f.output = make([]int16, f.size)
	// FFT.
	f.fftSize = 1
	for f.fftSize < f.size {
		f.fftSize <<= 1
	}
	f.fft = newFFTR(f.fftSize)
	f.fftIn = make([]int16, f.fftSize)
	f.spectrum = make([]complex16, f.fftSize/2+1)
	f.energy = make([]int32, f.fftSize/2+1)
	f.setupFilterbank(f.fftSize/2 + 1)
	// Noise reduction.
	f.even = toFixed(evenSmoothing)
	f.odd = toFixed(oddSmoothing)
	f.minSignal = toFixed(minSignalRemaining)
	f.estimate = make([]uint32, f.channels)
	// PCAN.
	correction := msb32(uint32(f.fftSize)) - 1 - filterbankBits/2
	f.snrShift = pcanGainBits - correction - pcanSnrBits
	f.setupPcan(smoothingBits - correction)
	f.out = make([]uint16, f.channels)
	return f
}

// toFixed: a float config value times 2^14, truncated (C: float * int -> uint32_t).
func toFixed(v float32) uint32 {
	scale := float32(1 << noiseReductionBits)
	return uint32(v * scale)
}

// The float functions of tflite-micro's frontend (cosf, log1pf, floorf, powf): computed in
// double and rounded to float, which is what a correctly rounded C library returns.
func cosf(x float32) float32         { return float32(math.Cos(float64(x))) }
func log1pf(x float32) float32       { return float32(math.Log1p(float64(x))) }
func floorf(x float32) float32       { return float32(math.Floor(float64(x))) }
func powf(x, y float32) float32      { return float32(math.Pow(float64(x), float64(y))) }
func freqToMel(freq float32) float32 { return 1127 * log1pf(freq/700) }

func (f *Frontend) setupFilterbank(spectrumSize int) {
	const indexAlignment, blockSize = 2, 4 // kFilterbankIndexAlignment / sizeof(int16), kFilterbankChannelBlockSize
	n1 := f.channels + 1
	f.chanFreqStarts, f.chanWeightSt, f.chanWidths = make([]int, n1), make([]int, n1), make([]int, n1)
	f.work = make([]uint64, n1)
	f.scaled = make([]uint32, f.channels)
	center := make([]float32, n1)
	melLow, melHi := freqToMel(lowerBand), freqToMel(upperBand)
	spacing := (melHi - melLow) / float32(n1)
	for i := range center {
		center[i] = melLow + spacing*float32(i+1)
	}
	hzPerSbin := float32(0.5) * sampleRate / (float32(spectrumSize) - 1)
	f.startIndex = int(float32(1.5) + float32(lowerBand)/hzPerSbin)
	actualStarts, actualWidths := make([]int, n1), make([]int, n1)
	chanStart, weightStart, needsZeros := f.startIndex, 0, false
	for ch := 0; ch < n1; ch++ {
		freq := chanStart
		for freqToMel(float32(freq)*hzPerSbin) <= center[ch] {
			freq++
		}
		width := freq - chanStart
		actualStarts[ch], actualWidths[ch] = chanStart, width
		if width == 0 {
			f.chanFreqStarts[ch], f.chanWeightSt[ch], f.chanWidths[ch] = 0, 0, blockSize
			if !needsZeros {
				needsZeros = true
				for j := 0; j < ch; j++ {
					f.chanWeightSt[j] += blockSize
				}
				weightStart += blockSize
			}
		} else {
			aligned := chanStart / indexAlignment * indexAlignment
			alignedWidth := chanStart - aligned + width
			padded := ((alignedWidth-1)/blockSize + 1) * blockSize
			f.chanFreqStarts[ch], f.chanWeightSt[ch], f.chanWidths[ch] = aligned, weightStart, padded
			weightStart += padded
		}
		chanStart = freq
	}
	f.weights, f.unweights = make([]int16, weightStart), make([]int16, weightStart)
	for ch := 0; ch < n1; ch++ {
		freq := actualStarts[ch]
		offset := freq - f.chanFreqStarts[ch]
		denom := melLow
		if ch > 0 {
			denom = center[ch-1]
		}
		for j := 0; j < actualWidths[ch]; j, freq = j+1, freq+1 {
			w := (center[ch] - freqToMel(float32(freq)*hzPerSbin)) / (center[ch] - denom)
			idx := f.chanWeightSt[ch] + offset + j
			f.weights[idx] = int16(floorf(w*float32(1<<filterbankBits) + 0.5))
			f.unweights[idx] = int16(floorf((1-w)*float32(1<<filterbankBits) + 0.5))
		}
		if freq > f.endIndex {
			f.endIndex = freq
		}
	}
}

func pcanGain(inputBits int, x uint32) int16 {
	xf := float32(x) / float32(uint32(1)<<inputBits)
	strength := float32(pcanStrength)
	g := float32(uint32(1)<<pcanGainBits) * powf(xf+pcanOffset, -strength)
	if g > 0x7FFF {
		return 0x7FFF
	}
	return int16(g + 0.5)
}

func (f *Frontend) setupPcan(inputBits int) {
	// The C code keeps lut[0], lut[1] and then 4 entries per interval from interval 2 on, at
	// lut[4*interval - 6 + k]. Same layout here.
	f.gainLut = make([]int16, 4*wideDynamicBits-3)
	f.gainLut[0] = pcanGain(inputBits, 0)
	f.gainLut[1] = pcanGain(inputBits, 1)
	for interval := 2; interval <= wideDynamicBits; interval++ {
		x0 := uint32(1) << (interval - 1)
		x1 := x0 + x0>>1
		x2 := 2 * x0
		if interval == wideDynamicBits {
			x2 = x0 + (x0 - 1)
		}
		y0, y1, y2 := pcanGain(inputBits, x0), pcanGain(inputBits, x1), pcanGain(inputBits, x2)
		diff1, diff2 := int32(y1)-int32(y0), int32(y2)-int32(y0)
		a1 := 4*diff1 - diff2
		a2 := diff2 - a1
		base := 4*interval - 6
		f.gainLut[base] = y0
		f.gainLut[base+1] = int16(a1)
		f.gainLut[base+2] = int16(a2)
	}
}

func (f *Frontend) wideDynamic(x uint32) int16 {
	if x <= 2 {
		return f.gainLut[x]
	}
	interval := int16(msb32(x))
	lut := f.gainLut[4*int(interval)-6:]
	var frac int16
	if interval < 11 {
		frac = int16((x << (11 - interval)) & 0x3FF)
	} else {
		frac = int16((x >> (interval - 11)) & 0x3FF)
	}
	result := (int32(lut[2]) * int32(frac)) >> 5
	result += int32(uint32(lut[1]) << 5)
	result *= int32(frac)
	result = (result + (1 << 14)) >> 15
	result += int32(lut[0])
	return int16(result)
}

func pcanShrink(x uint32) uint32 {
	if x < (2 << pcanSnrBits) {
		return (x * x) >> (2 + 2*pcanSnrBits - pcanOutputBits)
	}
	return (x >> (pcanSnrBits - pcanOutputBits)) - (1 << pcanOutputBits)
}

func sqrt32(num uint32) uint16 {
	if num == 0 {
		return 0
	}
	var res uint32
	maxBit := 32 - msb32(num)
	maxBit |= 1
	bit := uint32(1) << (31 - maxBit)
	for it := (31-maxBit)/2 + 1; it > 0; it-- {
		if num >= res+bit {
			num -= res + bit
			res = res>>1 + bit
		} else {
			res >>= 1
		}
		bit >>= 2
	}
	if num > res && res != 0xFFFF {
		res++
	}
	return uint16(res)
}

func sqrt64(num uint64) uint32 {
	if num>>32 == 0 {
		return uint32(sqrt32(uint32(num)))
	}
	var res uint64
	maxBit := 64 - msb64(num)
	maxBit |= 1
	bit := uint64(1) << (63 - maxBit)
	for it := (63-maxBit)/2 + 1; it > 0; it-- {
		if num >= res+bit {
			num -= res + bit
			res = res>>1 + bit
		} else {
			res >>= 1
		}
		bit >>= 2
	}
	if num > res && res != 0xFFFFFFFF {
		res++
	}
	return uint32(res)
}

func log2Fraction(x, log2x uint32) uint32 {
	frac := int32(int64(x) - int64(1)<<log2x)
	if log2x < logScaleLog2 {
		frac <<= logScaleLog2 - log2x
	} else {
		frac >>= log2x - logScaleLog2
	}
	baseSeg := uint32(frac) >> (logScaleLog2 - logSegmentsLog2)
	segUnit := (uint32(1) << logScaleLog2) >> logSegmentsLog2
	c0, c1 := int32(logLut[baseSeg]), int32(logLut[baseSeg+1])
	segBase := int32(segUnit * baseSeg)
	relPos := ((c1 - c0) * (frac - segBase)) >> logScaleLog2
	return uint32(frac + c0 + relPos)
}

func logValue(x uint32, scaleShift uint32) uint32 {
	integer := uint32(msb32(x) - 1)
	fraction := log2Fraction(x, integer)
	log2 := integer<<logScaleLog2 + fraction
	const round = logScale / 2
	loge := uint32((uint64(logCoeff)*uint64(log2) + round) >> logScaleLog2)
	return ((loge << scaleShift) + round) >> logScaleLog2
}

// Process consumes samples; each time a window is complete it calls emit with 40 features
// (0..~670; the slice is reused).
func (f *Frontend) Process(samples []int16, emit func([]uint16)) {
	for len(samples) > 0 {
		n := copy(f.input[f.inputUsed:], samples)
		samples = samples[n:]
		f.inputUsed += n
		if f.inputUsed < f.size {
			return
		}
		emit(f.frame())
	}
}

func (f *Frontend) frame() []uint16 {
	// Window.
	var maxAbs int16
	for i := 0; i < f.size; i++ {
		v := int16((int32(f.input[i]) * int32(f.coefficients[i])) >> windowBits)
		f.output[i] = v
		if v < 0 {
			v = -v
		}
		if v > maxAbs {
			maxAbs = v
		}
	}
	copy(f.input, f.input[f.step:])
	f.inputUsed -= f.step
	// FFT.
	shift := 15 - msb32(uint32(uint16(maxAbs)))
	for i := 0; i < f.size; i++ {
		f.fftIn[i] = int16(uint16(f.output[i]) << shift)
	}
	for i := f.size; i < f.fftSize; i++ {
		f.fftIn[i] = 0
	}
	f.fft.run(f.fftIn, f.spectrum)
	// Energy.
	for i := f.startIndex; i < f.endIndex; i++ {
		re, im := int32(f.spectrum[i].r), int32(f.spectrum[i].i)
		f.energy[i] = int32(uint32(re*re + im*im))
	}
	// Filterbank.
	var weightAcc, unweightAcc uint64
	for ch := 0; ch <= f.channels; ch++ {
		start, ws := f.chanFreqStarts[ch], f.chanWeightSt[ch]
		for j := 0; j < f.chanWidths[ch]; j++ {
			mag := uint64(f.energy[start+j])
			// weights are int16 promoted to int then multiplied with uint64: two's complement.
			weightAcc += uint64(int64(f.weights[ws+j])) * mag
			unweightAcc += uint64(int64(f.unweights[ws+j])) * mag
		}
		f.work[ch] = weightAcc
		weightAcc, unweightAcc = unweightAcc, 0
	}
	for i := 0; i < f.channels; i++ {
		f.scaled[i] = sqrt64(f.work[i+1]) >> shift
	}
	// Noise reduction.
	for i := 0; i < f.channels; i++ {
		smoothing := f.even
		if i&1 == 1 {
			smoothing = f.odd
		}
		oneMinus := uint32(1<<noiseReductionBits) - smoothing
		scaledUp := f.scaled[i] << smoothingBits
		estimate := uint32((uint64(scaledUp)*uint64(smoothing) + uint64(f.estimate[i])*uint64(oneMinus)) >> noiseReductionBits)
		f.estimate[i] = estimate
		if estimate > scaledUp {
			estimate = scaledUp
		}
		floor := uint32((uint64(f.scaled[i]) * uint64(f.minSignal)) >> noiseReductionBits)
		subtracted := (scaledUp - estimate) >> smoothingBits
		if subtracted > floor {
			f.scaled[i] = subtracted
		} else {
			f.scaled[i] = floor
		}
	}
	// PCAN gain control.
	for i := 0; i < f.channels; i++ {
		gain := uint32(f.wideDynamic(f.estimate[i]))
		snr := uint32((uint64(f.scaled[i]) * uint64(gain)) >> f.snrShift)
		f.scaled[i] = pcanShrink(snr)
	}
	// Log scale.
	correction := msb32(uint32(f.fftSize)) - 1 - filterbankBits/2
	for i := 0; i < f.channels; i++ {
		v := f.scaled[i]
		if correction < 0 {
			v >>= -correction
		} else {
			v <<= correction
		}
		if v > 1 {
			v = logValue(v, logScaleShift)
		} else {
			v = 0
		}
		if v > 0xFFFF {
			v = 0xFFFF
		}
		f.out[i] = uint16(v)
	}
	return f.out
}
