// Package audio: microphone and speaker of the Echo through the original DSP chain
// (devices/echo/hardware/gen1/src/echo-audio.sh must have started it).
package audio

import (
	"context"
	"encoding/binary"
	"fmt"
	"os"
	"sync/atomic"
	"time"
)

// The DSP writes the processed (beamformed, echo-cancelled) 16 kHz mono microphone signal into
// the shared lockless buffer /dev/shm/BMicsOut.shm. Layout, measured 16.10.2026:
//
//	0x00 u64  samples written so far (wraps the ring at capacity)
//	0x64 u32  sample size in bytes (2)
//	0x68 u32  capacity in samples (2^20)
//	0x70      ring of s16le samples
const (
	micHeader   = 0x70
	MicRate     = 16000
	micPoll     = 20 * time.Millisecond
	micIdlePoll = 200 * time.Millisecond // no samples for a second (the phone, its microphone closed)
	micMaxDelay = MicRate / 2            // reader further behind than 0.5 s: skip ahead
)

type Mic struct {
	dot      *dotAudio // the Dot's centre microphone instead of the DSP's buffer
	tv       *tvMic    // the Fire TV app's stream (tv.go)
	data     []byte
	capacity uint64
	muted    atomic.Bool
	level    atomic.Int32 // peak of the last poll
}

func OpenMic(path string) (*Mic, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		return nil, err
	}
	data, err := mapFile(f.Fd(), int(st.Size()))
	if err != nil {
		return nil, err
	}
	m := &Mic{data: data}
	size := binary.LittleEndian.Uint32(data[0x64:])
	m.capacity = uint64(binary.LittleEndian.Uint32(data[0x68:]))
	if size != 2 || m.capacity == 0 || micHeader+int(m.capacity)*2 > len(data) {
		unmapFile(data)
		return nil, fmt.Errorf("mic: unexpected buffer layout (size %d, capacity %d, file %d)", size, m.capacity, len(data))
	}
	return m, nil
}

// written reads the 64-bit counter consistently (two 32-bit halves, the writer may be between).
func (m *Mic) written() uint64 {
	if m.tv != nil {
		return m.tv.written.Load()
	}
	if m.dot != nil {
		return m.dot.written.Load()
	}
	for {
		lo1 := binary.LittleEndian.Uint32(m.data[0:])
		hi := binary.LittleEndian.Uint32(m.data[4:])
		lo2 := binary.LittleEndian.Uint32(m.data[0:])
		if lo1 == lo2 {
			return uint64(hi)<<32 | uint64(lo1)
		}
	}
}

func (m *Mic) sample(i uint64) int16 {
	if m.tv != nil {
		return m.tv.ring[i%tvMicRing]
	}
	if m.dot != nil {
		return m.dot.mic[i%dotMicRing]
	}
	return int16(binary.LittleEndian.Uint16(m.data[micHeader+int(i%m.capacity)*2:]))
}

// DotMic: the Dot's centre microphone, 16 kHz as the DSP's (StartDot must have run).
func DotMic() (*Mic, error) {
	if dot == nil {
		return nil, fmt.Errorf("mic: the Dot's codecs are not started")
	}
	return &Mic{dot: dot}, nil
}

// Alive: the DSP writes new samples within wait (the self-test at the start).
func (m *Mic) Alive(wait time.Duration) bool {
	before := m.written()
	for end := time.Now().Add(wait); time.Now().Before(end); time.Sleep(50 * time.Millisecond) {
		if m.written() != before {
			return true
		}
	}
	return false
}

// SetMuted: while muted, Run delivers silence (the mute button).
func (m *Mic) SetMuted(on bool) { m.muted.Store(on) }
func (m *Mic) Muted() bool      { return m.muted.Load() }

// Peak of the latest samples (0..32767), for voice detection.
func (m *Mic) Peak() int { return int(m.level.Load()) }

// Run calls deliver with new 16 kHz samples about every 20 ms until ctx ends.
func (m *Mic) Run(ctx context.Context, deliver func([]int16)) {
	last := m.written()
	ticker := time.NewTicker(micPoll)
	defer ticker.Stop()
	buf := make([]int16, 0, MicRate)
	// The phone's app sends nothing while its microphone is closed (its battery): after a second
	// without samples the poll slows to micIdlePoll, the first new samples speed it up again.
	idle := 0
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
		now := m.written()
		if now < last || now-last > micMaxDelay {
			last = now - MicRate/50 // restart, or we fell behind: the last 20 ms only
		}
		buf = buf[:0]
		peak := 0
		for i := last; i < now; i++ {
			v := m.sample(i)
			if a := abs(int(v)); a > peak {
				peak = a
			}
			buf = append(buf, v)
		}
		last = now
		if len(buf) == 0 {
			if idle++; idle == 50 {
				ticker.Reset(micIdlePoll)
			}
			continue
		}
		if idle >= 50 {
			ticker.Reset(micPoll)
		}
		idle = 0
		if m.muted.Load() {
			for i := range buf {
				buf[i] = 0
			}
			peak = 0
		}
		m.level.Store(int32(peak))
		deliver(buf)
	}
}

func abs(v int) int {
	if v < 0 {
		return -v
	}
	return v
}

// Upsampler converts 16 kHz to 24 kHz (factor 3/2) by linear interpolation.
type Upsampler struct {
	prev  int16
	phase int // position in thirds of an input sample, relative to prev
}

func (u *Upsampler) Process(in []int16, out []int16) []int16 {
	for _, cur := range in {
		// Output samples lie every 2/3 input sample; emit those between prev and cur.
		for u.phase < 3 {
			v := (int(u.prev)*(3-u.phase) + int(cur)*u.phase) / 3
			out = append(out, int16(v))
			u.phase += 2
		}
		u.phase -= 3
		u.prev = cur
	}
	return out
}
