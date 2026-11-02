package audio

import (
	"encoding/binary"
	"io"
	"log"
	"math"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	"echo-assistant/internal/alsa"
)

// The Echo Dot 2 (board.Dot2) has no DSP chain and no PulseAudio: The assistant drives its codecs
// himself (devices/echo/hardware/dot2/docs/21-echo-dot-linux-ssh.md). One playback loop mixes every stream (his voice,
// the music) into the DAC's 48 kHz stereo and never stops: silence in between keeps the
// amplifier on, and gating it clicks (EchoMuse). One capture loop takes the centre microphone of
// the ADCs' nine channels. There is no echo cancellation: the echo gate keeps the assistant from
// hearing himself.
const (
	dotCard      = 0
	dotPlayback  = 23 // TLV320DAC3203
	dotCapture   = 24 // four TLV320ADC3101 on one TDM bus
	dotOutRate   = 48000
	dotPeriod    = 960 // 20 ms
	dotPeriods   = 4
	dotChannels  = 9 // 0-5 the ring of microphones, 6 the centre one, 7/8 the playback loopback
	dotCentre    = 6
	dotLoopback  = 7
	dotInPeriod  = 320 // 20 ms at 16 kHz
	dotInPeriods = 8   // 16 was refused (EINVAL, 20.10.2026)
	dotMicRing   = 1 << 16
)

// The codecs' mixer at start. Nothing in our Linux opens the DAPM routes, so speaker and
// microphones stay powered down until these are set. Sequences and the speaker EQ as EchoLocal
// read them off Fire OS (github.com/ygelfand/echolocal, internal/hardware/speaker/paths_dot.go
// and mic/gain.go, MIT); checked on the Dot 20.10.2026 with a tone the microphones heard.
var dotMixer = [][]string{
	{"Ext_Speaker_Amp_Switch", "Off"},
	{"Audio_DacMux_Setting", "On"},
	{"Ignore Ramp Up", "Off"},
	{"HP Driver Gain Volume", "0"},
	{"HPL Output Mixer L_DAC Switch", "on"},
	{"HPR Output Mixer R_DAC Switch", "on"},
	{"Audio_DacMux_Setting", "Off"},
	{"Right Channel Only", "On"},
	{"HP Driver Gain Volume", "6"}, // 0 dB (Fire OS); StartDot adds the speaker gain
	{"HP DAC Playback Switch", "on"},
	{"Ext_Speaker_Amp_Switch", "On"},
}

// The vendor's speaker tuning: six unity blocks and one filter (reads back as zeros until written).
var dotSpeakerEQ = []byte{
	128, 0, 1, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0,
	128, 0, 1, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0,
	128, 0, 1, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0,
	128, 0, 1, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0,
	128, 0, 1, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0,
	128, 0, 1, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0,
	127, 247, 0, 0, 128, 9, 0, 0, 127, 239, 0, 0, 0, 17, 0, 0,
	0, 17, 0, 0, 127, 222, 0, 0, 15, 0, 0,
}

// dotMics: each ADC's two inputs from its DIF1 pair. PGA 40 = +20 dB analog (Amazon's value);
// the digital volume at 24 = 0 dB (0..64 is -12..+20 dB): at +20 dB a voice close by clipped
// the 24 bits, and the software gain (StartDot) sets the level instead.
func dotMics() [][]string {
	var out [][]string
	for _, adc := range []string{"A", "B", "C", "D"} {
		out = append(out,
			[]string{"ADC_" + adc + " DIF1_L Input Gain", "off"},
			[]string{"ADC_" + adc + " DIF1_R Input Gain", "off"},
			[]string{"ADC_" + adc + " Left Ip Select ADC_" + adc + " DIF1_L switch", "on"},
			[]string{"ADC_" + adc + " Right Ip Select ADC_" + adc + " DIF1_R switch", "on"},
			[]string{"ADC_" + adc + " MICPGA Volume Ctrl", "40"},
			[]string{"ADC_" + adc + " Digital Volume Control", "24"})
	}
	return out
}

type dotAudio struct {
	mu      sync.Mutex
	streams []*dotStream
	gain    float64
	mic     [dotMicRing]int16 // the centre microphone, 16 kHz
	written atomic.Uint64     // samples written to mic so far
	loop    atomic.Int32      // peak of the playback loopback since DotLoopback last read it
	bt      atomic.Pointer[dotBT]
}

var dot *dotAudio

// dotDriverGain: the HP Driver Gain step for a gain over 0 dB (step 6 = 0 dB, 1 dB per step,
// 0..35 = -6..+29 dB).
func dotDriverGain(db float64) string {
	return strconv.Itoa(max(0, min(35, 6+int(math.Round(db)))))
}

// StartDot sets the Dot's mixer, opens playback and capture and starts their loops. micGainDB:
// the software gain on the microphone; speakerGainDB: the speaker driver over 0 dB.
func StartDot(micGainDB, speakerGainDB float64) error {
	m, err := alsa.OpenMixer(dotCard)
	if err != nil {
		return err
	}
	for _, c := range append(dotMixer, dotMics()...) {
		if c[0] == "HP Driver Gain Volume" && c[1] == "6" {
			c = []string{c[0], dotDriverGain(speakerGainDB)}
		}
		if err := m.Set(c[0], c[1:]...); err != nil {
			log.Printf("[Audio] %v", err)
		}
		if c[0] == "HP Driver Gain Volume" && c[1] == "0" {
			if err := m.SetBytes("biquad coefficients", dotSpeakerEQ); err != nil {
				log.Printf("[Audio] %v", err)
			}
		}
	}
	m.Close()
	pb, err := alsa.OpenPCM(dotCard, dotPlayback, false, alsa.Config{Channels: 2, Rate: dotOutRate,
		Format: alsa.S16LE, PeriodFrames: dotPeriod, Periods: dotPeriods})
	if err != nil {
		return err
	}
	cp, err := alsa.OpenPCM(dotCard, dotCapture, true, alsa.Config{Channels: dotChannels, Rate: MicRate,
		Format: alsa.S24_3LE, PeriodFrames: dotInPeriod, Periods: dotInPeriods})
	if err != nil {
		pb.Close()
		return err
	}
	d := &dotAudio{gain: math.Pow(10, micGainDB/20)}
	dot = d
	go d.play(pb)
	go d.capture(cp)
	log.Printf("[Audio] Dot codecs ready (playback %d frames, capture %d, mic gain %.0f dB)", pb.Buffer, cp.Buffer, micGainDB)
	return nil
}

// DotLoopback: the playback's peak as the ADCs' loopback channel saw it (0..32767) since the
// last call, proof that the DAC plays.
func DotLoopback() int {
	if dot == nil {
		return 0
	}
	return int(dot.loop.Swap(0))
}

func (d *dotAudio) play(pb *alsa.PCM) {
	acc := make([]int32, dotPeriod*2)
	out := make([]byte, dotPeriod*4)
	var streams []*dotStream
	xruns := 0
	for {
		clear(acc)
		d.mu.Lock()
		streams = append(streams[:0], d.streams...)
		d.mu.Unlock()
		for _, s := range streams {
			s.mix(acc)
		}
		for i, v := range acc {
			binary.LittleEndian.PutUint16(out[i*2:], uint16(clip16(v)))
		}
		if bt := d.bt.Load(); bt != nil && bt.send(out) {
			clear(out) // the Bluetooth speaker plays it; the DAC keeps the pace
		}
		if err := pb.Write(out); err != nil {
			log.Printf("[Audio] playback: %v", err)
			time.Sleep(time.Second)
		}
		if pb.Xruns != xruns {
			xruns = pb.Xruns
			log.Printf("[Audio] playback underrun (%d)", xruns)
		}
	}
}

func (d *dotAudio) capture(cp *alsa.PCM) {
	const frame = dotChannels * 3
	buf := make([]byte, dotInPeriod*frame)
	var x1, y1 float64 // DC blocker
	xruns := 0
	for {
		if err := cp.Read(buf); err != nil {
			log.Printf("[Audio] capture: %v", err)
			time.Sleep(time.Second)
			continue
		}
		if cp.Xruns != xruns {
			xruns = cp.Xruns
			log.Printf("[Audio] capture overrun (%d)", xruns)
		}
		n := d.written.Load()
		loop := 0
		for f := 0; f < dotInPeriod; f++ {
			o := f * frame
			x := float64(s24(buf[o+dotCentre*3:])) / 256 * d.gain
			y := x - x1 + 0.995*y1
			x1, y1 = x, y
			d.mic[(n+uint64(f))%dotMicRing] = int16(clip16(int32(y)))
			loop = max(loop, abs(int(s24(buf[o+dotLoopback*3:])>>8)))
		}
		d.written.Store(n + dotInPeriod)
		for old := d.loop.Load(); int32(loop) > old && !d.loop.CompareAndSwap(old, int32(loop)); old = d.loop.Load() {
		}
	}
}

func s24(b []byte) int32 { return int32(uint32(b[0])<<8|uint32(b[1])<<16|uint32(b[2])<<24) >> 8 }

func clip16(v int32) int16 {
	return int16(max(-32768, min(32767, v)))
}

func (d *dotAudio) remove(s *dotStream) {
	d.mu.Lock()
	defer d.mu.Unlock()
	for i, x := range d.streams {
		if x == s {
			d.streams = append(d.streams[:i], d.streams[i+1:]...)
			return
		}
	}
}

// dotStream: one source of the Dot's playback mix, as one pacat client on the Echo. It starts
// sounding once prebuffer of it is queued or has waited that long (PulseAudio's jitter buffer),
// and after running dry it buffers again.
type dotStream struct {
	d         *dotAudio
	channels  int
	step      float64 // input frames per output frame
	prebuffer time.Duration
	limit     int // frames queued at most: Write blocks above it
	mu        sync.Mutex
	space     *sync.Cond
	buf       []int16 // interleaved frames, from head on not yet played
	head      int
	pos       float64 // between frame head and head+1
	playing   bool
	firstAt   time.Time
	closed    bool
}

func (d *dotAudio) newStream(rate, channels int, prebuffer, limit time.Duration) *dotStream {
	s := &dotStream{d: d, channels: channels, step: float64(rate) / dotOutRate, prebuffer: prebuffer,
		limit: int(limit.Seconds() * float64(rate))}
	s.space = sync.NewCond(&s.mu)
	d.mu.Lock()
	d.streams = append(d.streams, s)
	d.mu.Unlock()
	return s
}

func (s *dotStream) queuedLocked() int { return len(s.buf)/s.channels - s.head }

// Write queues s16le frames; blocks while more than the limit is queued.
func (s *dotStream) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for !s.closed && s.queuedLocked() > s.limit {
		s.space.Wait()
	}
	if s.closed {
		return 0, io.ErrClosedPipe
	}
	if !s.playing && s.queuedLocked() == 0 {
		s.firstAt = time.Now()
	}
	n := len(p) / (2 * s.channels) * (2 * s.channels)
	for i := 0; i < n; i += 2 {
		s.buf = append(s.buf, int16(binary.LittleEndian.Uint16(p[i:])))
	}
	return len(p), nil
}

// Close drops what is queued and leaves the mix at once.
func (s *dotStream) Close() error {
	s.mu.Lock()
	s.closed, s.buf = true, nil
	s.space.Broadcast()
	s.mu.Unlock()
	s.d.remove(s)
	return nil
}

// mix adds the next output period (48 kHz stereo) of the stream to acc, resampled linearly.
func (s *dotStream) mix(acc []int32) {
	s.mu.Lock()
	defer s.mu.Unlock()
	frames := len(s.buf) / s.channels
	if !s.playing {
		if frames-s.head < 2 || (time.Duration(frames-s.head)*time.Second/time.Duration(s.step*dotOutRate) < s.prebuffer &&
			time.Since(s.firstAt) < s.prebuffer) {
			return
		}
		s.playing = true
	}
	ch := s.channels
	for i := 0; i < len(acc)/2; i++ {
		k := s.head
		if k+1 >= frames { // ran dry: the rest waits for the next prebuffer
			s.buf, s.head, s.pos, s.playing = s.buf[:0], 0, 0, false
			break
		}
		l := lerp(s.buf[k*ch], s.buf[(k+1)*ch], s.pos)
		r := l
		if ch == 2 {
			r = lerp(s.buf[k*ch+1], s.buf[(k+1)*ch+1], s.pos)
		}
		acc[2*i] += l
		acc[2*i+1] += r
		s.pos += s.step
		for s.pos >= 1 {
			s.pos--
			s.head++
		}
	}
	if s.head > 4096 && 2*s.head > frames {
		n := copy(s.buf, s.buf[s.head*ch:])
		s.buf, s.head = s.buf[:n], 0
	}
	s.space.Broadcast()
}

func lerp(a, b int16, f float64) int32 { return int32(float64(a) + (float64(b)-float64(a))*f) }
