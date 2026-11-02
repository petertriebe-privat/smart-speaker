package audio

import (
	"encoding/binary"
	"math"
	"testing"
	"time"
)

func pcmBytes(samples []int16) []byte {
	b := make([]byte, 2*len(samples))
	for i, v := range samples {
		binary.LittleEndian.PutUint16(b[2*i:], uint16(v))
	}
	return b
}

func TestDotStreamPrebufferAndResample(t *testing.T) {
	d := &dotAudio{}
	s := d.newStream(24000, 1, 100*time.Millisecond, time.Second)
	acc := make([]int32, dotPeriod*2)
	// 50 ms queued: below the prebuffer, and not yet waited for it
	s.Write(pcmBytes(make([]int16, 1200)))
	s.mix(acc)
	if s.playing {
		t.Fatal("started before the prebuffer")
	}
	ramp := make([]int16, 4800) // 200 ms
	for i := range ramp {
		ramp[i] = int16(i)
	}
	s.Write(pcmBytes(ramp))
	clear(acc)
	s.mix(acc)
	if !s.playing {
		t.Fatal("did not start with 250 ms queued")
	}
	// 24 kHz -> 48 kHz: every input sample twice (once interpolated), both channels alike
	if acc[0] != acc[1] || s.head != dotPeriod/2 {
		t.Fatalf("left %d right %d, head %d", acc[0], acc[1], s.head)
	}
}

func TestDotStreamRunsDryAndCloses(t *testing.T) {
	d := &dotAudio{}
	s := d.newStream(48000, 2, 0, time.Second)
	s.Write(pcmBytes([]int16{1000, -1000, 1000, -1000}))
	acc := make([]int32, dotPeriod*2)
	s.mix(acc)
	if acc[0] != 1000 || acc[1] != -1000 || s.playing || len(s.buf) != 0 {
		t.Fatalf("acc %d %d, playing %v, queued %d", acc[0], acc[1], s.playing, len(s.buf))
	}
	s.Close()
	if len(d.streams) != 0 {
		t.Fatal("still in the mix")
	}
	if _, err := s.Write(pcmBytes([]int16{1, 2})); err == nil {
		t.Fatal("write after close")
	}
}

func TestOutputGainFollowsBluetooth(t *testing.T) {
	saved := dot
	defer func() { dot = saved }()
	dot = nil
	if g := OutputGain(0.16); g != 0.16 {
		t.Fatalf("no Dot: gain %v, want the volume's 0.16", g)
	}
	dot = &dotAudio{}
	b := &dotBT{periods: make(chan []byte, 1)}
	dot.bt.Store(b)
	if g := OutputGain(0.16); g != 0.16 {
		t.Fatalf("link down: gain %v, want 0.16", g)
	}
	b.active.Store(true)
	if g := OutputGain(0.16); math.Abs(g-0.4) > 1e-9 {
		t.Fatalf("streaming: gain %v, want the flatter 0.4", g)
	}
	if d := dotExtraLatency(); d != dotBTLatency {
		t.Fatalf("streaming: extra latency %v, want %v", d, dotBTLatency)
	}
}

func TestDotDriverGain(t *testing.T) {
	for db, want := range map[float64]string{0: "6", 12: "18", 2.6: "9", -10: "0", 40: "35"} {
		if got := dotDriverGain(db); got != want {
			t.Errorf("dotDriverGain(%v) = %s, want %s", db, got, want)
		}
	}
}

func TestS24(t *testing.T) {
	for _, c := range []struct {
		b    []byte
		want int32
	}{{[]byte{1, 0, 0}, 1}, {[]byte{0xff, 0xff, 0xff}, -1}, {[]byte{0, 0, 0x80}, -8388608}, {[]byte{0xff, 0xff, 0x7f}, 8388607}} {
		if got := s24(c.b); got != c.want {
			t.Errorf("s24(%v) = %d, want %d", c.b, got, c.want)
		}
	}
}
