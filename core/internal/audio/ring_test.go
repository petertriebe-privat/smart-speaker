package audio

import (
	"encoding/binary"
	"testing"
)

func TestRingWrapsAround(t *testing.T) {
	const frame, capacity = 4, 5
	data := make([]byte, micHeader+frame*capacity)
	binary.LittleEndian.PutUint32(data[0x64:], frame)
	binary.LittleEndian.PutUint32(data[0x68:], capacity)
	// frames 0..6 written: slots hold 5, 6, 2, 3, 4
	for i := 0; i < 7; i++ {
		off := micHeader + (i%capacity)*frame
		for k := 0; k < frame; k++ {
			data[off+k] = byte(i*10 + k)
		}
	}
	binary.LittleEndian.PutUint32(data[0:], 7)
	r, err := newRing(data)
	if err != nil {
		t.Fatal(err)
	}
	if r.Written() != 7 {
		t.Fatalf("written %d", r.Written())
	}
	got := r.Append(nil, 4, 7)
	want := []byte{40, 41, 42, 43, 50, 51, 52, 53, 60, 61, 62, 63}
	if string(got) != string(want) {
		t.Fatalf("got %v, want %v", got, want)
	}
}

func TestRingRejectsBadLayout(t *testing.T) {
	data := make([]byte, micHeader+10)
	binary.LittleEndian.PutUint32(data[0x64:], 14)
	binary.LittleEndian.PutUint32(data[0x68:], 96001)
	if _, err := newRing(data); err == nil {
		t.Fatal("a ring larger than the file was accepted")
	}
}
