package audio

import (
	"encoding/binary"
	"fmt"
	"os"
)

// Ring reads one of audioproxyd's shared ring buffers of any frame size (the layout of
// BMicsOut, see mic.go). The DSP's raw capture (#61) writes four of them while it runs:
//
//	/dev/shm/BCaptureMics.shm  7 raw microphones, 16 kHz, 14-byte frames (s16le interleaved)
//	/dev/shm/BCaptureProc.shm  the processed signal (as BMicsOut), 16 kHz mono
//	/dev/shm/BCaptureRef.shm   the speaker reference, 16 kHz stereo
//	/dev/shm/BCaptureVoip.shm  the VoIP signal (empty outside calls)
//
// Started with `lipc-set-prop -i com.doppler.audio capture 268435455` (0x0FFFFFFF: continuous;
// smaller values write up to 4000 ms to /tmp/rawCapture*.wav instead), stopped with `capture 0`.
// Found 19.10.2026 by disassembling audioproxyd (LipcPropSetRawCapture, AudioRawCapture).
type Ring struct {
	data     []byte
	Frame    int    // bytes per frame
	capacity uint64 // frames
}

func OpenRing(path string) (*Ring, error) {
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
	r, err := newRing(data)
	if err != nil {
		unmapFile(data)
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	return r, nil
}

func newRing(data []byte) (*Ring, error) {
	if len(data) < micHeader {
		return nil, fmt.Errorf("ring: %d bytes, no header", len(data))
	}
	frame := int(binary.LittleEndian.Uint32(data[0x64:]))
	capacity := uint64(binary.LittleEndian.Uint32(data[0x68:]))
	if frame == 0 || capacity == 0 || micHeader+int(capacity)*frame > len(data) {
		return nil, fmt.Errorf("ring: unexpected layout (frame %d, capacity %d, file %d)", frame, capacity, len(data))
	}
	return &Ring{data: data, Frame: frame, capacity: capacity}, nil
}

func (r *Ring) Close() { unmapFile(r.data) }

// Written: frames written so far (the 64-bit counter read consistently, as Mic.written).
func (r *Ring) Written() uint64 {
	for {
		lo1 := binary.LittleEndian.Uint32(r.data[0:])
		hi := binary.LittleEndian.Uint32(r.data[4:])
		lo2 := binary.LittleEndian.Uint32(r.data[0:])
		if lo1 == lo2 {
			return uint64(hi)<<32 | uint64(lo1)
		}
	}
}

// Capacity in frames: a reader further behind than this has lost frames.
func (r *Ring) Capacity() uint64 { return r.capacity }

// Append adds the raw bytes of frames [from, to) to dst.
func (r *Ring) Append(dst []byte, from, to uint64) []byte {
	for i := from; i < to; i++ {
		off := micHeader + int(i%r.capacity)*r.Frame
		dst = append(dst, r.data[off:off+r.Frame]...)
	}
	return dst
}
