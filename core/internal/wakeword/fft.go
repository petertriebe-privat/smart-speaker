package wakeword

import "math"

// kissfft (v130) in 16-bit fixed point (FIXED_POINT 16), as the microfrontend uses it: a real
// FFT of n points through a complex FFT of n/2. Ported bit for bit: int16 wraps, the scaling of
// every stage (C_FIXDIV) and the rounding (sround) as in _kiss_fft_guts.h.

type complex16 struct{ r, i int16 }

const sampMax = 32767

func sround(x int32) int16 { return int16((x + (1 << 14)) >> 15) }

func fixdiv(c *complex16, k int32) {
	c.r = sround(int32(c.r) * (sampMax / k))
	c.i = sround(int32(c.i) * (sampMax / k))
}

func cmul(a, b complex16) complex16 {
	return complex16{
		r: sround(int32(a.r)*int32(b.r) - int32(a.i)*int32(b.i)),
		i: sround(int32(a.r)*int32(b.i) + int32(a.i)*int32(b.r)),
	}
}

func cexp(phase float64) complex16 {
	return complex16{r: int16(math.Floor(0.5 + sampMax*math.Cos(phase))), i: int16(math.Floor(0.5 + sampMax*math.Sin(phase)))}
}

type kissFFT struct {
	n        int
	factors  []int
	twiddles []complex16
}

func newKissFFT(n int) *kissFFT {
	k := &kissFFT{n: n, twiddles: make([]complex16, n)}
	const pi = 3.141592653589793238462643383279502884197169399375105820974944
	for i := range k.twiddles {
		k.twiddles[i] = cexp(-2 * pi * float64(i) / float64(n))
	}
	// kf_factor: powers of 4, then 2, then the remaining primes.
	p, m := 4, n
	floorSqrt := math.Floor(math.Sqrt(float64(n)))
	for {
		for m%p != 0 {
			switch p {
			case 4:
				p = 2
			case 2:
				p = 3
			default:
				p += 2
			}
			if float64(p) > floorSqrt {
				p = m
			}
		}
		m /= p
		k.factors = append(k.factors, p, m)
		if m <= 1 {
			break
		}
	}
	return k
}

func (k *kissFFT) run(in, out []complex16) { k.work(out, in, 1, k.factors) }

func (k *kissFFT) work(out, f []complex16, fstride int, factors []int) {
	p, m := factors[0], factors[1]
	if m == 1 {
		for j := 0; j < p*m; j++ {
			out[j] = f[j*fstride]
		}
	} else {
		for j := 0; j < p; j++ {
			k.work(out[j*m:], f[j*fstride:], fstride*p, factors[2:])
		}
	}
	switch p {
	case 2:
		k.bfly2(out, fstride, m)
	case 4:
		k.bfly4(out, fstride, m)
	default:
		panic("wakeword: FFT size needs radix 2 or 4 only")
	}
}

func (k *kissFFT) bfly2(out []complex16, fstride, m int) {
	for j := 0; j < m; j++ {
		a, b := &out[j], &out[j+m]
		fixdiv(a, 2)
		fixdiv(b, 2)
		t := cmul(*b, k.twiddles[j*fstride])
		b.r, b.i = a.r-t.r, a.i-t.i
		a.r, a.i = a.r+t.r, a.i+t.i
	}
}

func (k *kissFFT) bfly4(out []complex16, fstride, m int) {
	m2, m3 := 2*m, 3*m
	for j := 0; j < m; j++ {
		f0, f1, f2, f3 := &out[j], &out[j+m], &out[j+m2], &out[j+m3]
		fixdiv(f0, 4)
		fixdiv(f1, 4)
		fixdiv(f2, 4)
		fixdiv(f3, 4)
		s0 := cmul(*f1, k.twiddles[j*fstride])
		s1 := cmul(*f2, k.twiddles[j*fstride*2])
		s2 := cmul(*f3, k.twiddles[j*fstride*3])
		s5 := complex16{f0.r - s1.r, f0.i - s1.i}
		f0.r, f0.i = f0.r+s1.r, f0.i+s1.i
		s3 := complex16{s0.r + s2.r, s0.i + s2.i}
		s4 := complex16{s0.r - s2.r, s0.i - s2.i}
		f2.r, f2.i = f0.r-s3.r, f0.i-s3.i
		f0.r, f0.i = f0.r+s3.r, f0.i+s3.i
		f1.r, f1.i = s5.r+s4.i, s5.i-s4.r
		f3.r, f3.i = s5.r-s4.i, s5.i+s4.r
	}
}

// fftr: kiss_fftr, the real FFT of n points (n even), n/2+1 bins.
type fftr struct {
	sub           *kissFFT
	packed, tmp   []complex16
	superTwiddles []complex16
}

func newFFTR(n int) *fftr {
	half := n / 2
	r := &fftr{sub: newKissFFT(half), packed: make([]complex16, half), tmp: make([]complex16, half),
		superTwiddles: make([]complex16, half/2)}
	for i := range r.superTwiddles {
		r.superTwiddles[i] = cexp(-3.14159265358979323846264338327 * (float64(i+1)/float64(half) + .5))
	}
	return r
}

func (r *fftr) run(in []int16, out []complex16) {
	ncfft := r.sub.n
	for i := range r.packed {
		r.packed[i] = complex16{in[2*i], in[2*i+1]}
	}
	r.sub.run(r.packed, r.tmp)
	tdc := r.tmp[0]
	fixdiv(&tdc, 2)
	out[0] = complex16{r: tdc.r + tdc.i}
	out[ncfft] = complex16{r: tdc.r - tdc.i}
	for k := 1; k <= ncfft/2; k++ {
		fpk := r.tmp[k]
		fpnk := complex16{r.tmp[ncfft-k].r, -r.tmp[ncfft-k].i}
		fixdiv(&fpk, 2)
		fixdiv(&fpnk, 2)
		f1k := complex16{fpk.r + fpnk.r, fpk.i + fpnk.i}
		f2k := complex16{fpk.r - fpnk.r, fpk.i - fpnk.i}
		tw := cmul(f2k, r.superTwiddles[k-1])
		// HALF_OF on the int sum, then stored as int16.
		out[k] = complex16{int16((int32(f1k.r) + int32(tw.r)) >> 1), int16((int32(f1k.i) + int32(tw.i)) >> 1)}
		out[ncfft-k] = complex16{int16((int32(f1k.r) - int32(tw.r)) >> 1), int16((int32(tw.i) - int32(f1k.i)) >> 1)}
	}
}
