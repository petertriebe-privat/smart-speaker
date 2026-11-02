package main

import (
	"encoding/binary"
	"testing"

	"echo-assistant/internal/wakeword"
)

func makeWav(rate int, channels [][]int16) []byte {
	nch, frames := len(channels), len(channels[0])
	b := []byte("RIFF\x00\x00\x00\x00WAVEfmt ")
	b = binary.LittleEndian.AppendUint32(b, 16)
	b = binary.LittleEndian.AppendUint16(b, 1)
	b = binary.LittleEndian.AppendUint16(b, uint16(nch))
	b = binary.LittleEndian.AppendUint32(b, uint32(rate))
	b = binary.LittleEndian.AppendUint32(b, uint32(rate*nch*2))
	b = binary.LittleEndian.AppendUint16(b, uint16(nch*2))
	b = binary.LittleEndian.AppendUint16(b, 16)
	b = append(b, "data"...)
	b = binary.LittleEndian.AppendUint32(b, uint32(frames*nch*2))
	for f := 0; f < frames; f++ {
		for c := 0; c < nch; c++ {
			b = binary.LittleEndian.AppendUint16(b, uint16(channels[c][f]))
		}
	}
	return b
}

func TestParseWavDeinterleaves(t *testing.T) {
	w, err := parseWav(makeWav(16000, [][]int16{{1, 2, 3}, {-1, -2, -3}, {100, 200, 300}}))
	if err != nil {
		t.Fatal(err)
	}
	if w.rate != 16000 || len(w.channels) != 3 || w.channels[2][1] != 200 || w.channels[1][2] != -3 {
		t.Fatalf("got rate %d, channels %v", w.rate, w.channels)
	}
	tr := w.tracks()
	if got := tr["mix"]; got[0] != 33 || got[2] != 100 {
		t.Fatalf("mix %v", got)
	}
	if order := trackOrder(tr); len(order) != 4 || order[0] != "mix" || order[3] != "ch2" {
		t.Fatalf("order %v", order)
	}
}

func TestParseWavKeepsTruncatedData(t *testing.T) {
	b := makeWav(16000, [][]int16{{1, 2, 3, 4}})
	w, err := parseWav(b[:len(b)-4]) // the last two samples cut off
	if err != nil {
		t.Fatal(err)
	}
	if len(w.channels[0]) != 2 {
		t.Fatalf("frames %d", len(w.channels[0]))
	}
}

func TestSilenceIsNotHeard(t *testing.T) {
	r, err := evaluate(wakeword.Builtin(), 0.45, 5, make([]int16, 3*16000))
	if err != nil {
		t.Fatal(err)
	}
	if len(r.rises) != 0 || r.peak > 0.1 {
		t.Fatalf("silence: %s", r)
	}
}
