// Package alsa: PCM streams and mixer controls of the kernel's ALSA devices, through their
// ioctls (no alsa-lib, no cgo). Used on the Echo Dot 2, which has no DSP chain and no
// PulseAudio: The assistant drives the codecs itself.
//
// The ioctl structures are those of a 32-bit ARM kernel (long and pointers 4 bytes, long long
// aligned to 8): the Dot's 3.18 kernel runs in 32-bit mode.
package alsa

import (
	"errors"
	"fmt"
)

// Format: the kernel's SNDRV_PCM_FORMAT_* numbers.
type Format int

const (
	S16LE   Format = 2
	S24_3LE Format = 32 // 24 bits in three bytes
)

// Bytes per sample.
func (f Format) Bytes() int {
	switch f {
	case S16LE:
		return 2
	case S24_3LE:
		return 3
	}
	return 0
}

// Config of a PCM stream.
type Config struct {
	Channels     int
	Rate         int
	Format       Format
	PeriodFrames int // frames per period (one interrupt)
	Periods      int // periods in the ring buffer
}

// ErrUnsupported: built for a system without ALSA (the tests on the PC).
var ErrUnsupported = errors.New("alsa: not supported on this system")

// Control types (SNDRV_CTL_ELEM_TYPE_*).
const (
	typeBoolean    = 1
	typeInteger    = 2
	typeEnumerated = 3
	typeBytes      = 4
)

// controlInfo: what the mixer needs to know to write a control.
type controlInfo struct {
	typ   int
	count int
	items []string // ENUMERATED
}

// parseValues turns the text values of Mixer.Set into the control's numbers: one value for all
// channels, or one per channel. Booleans take on/off/1/0, enumerations their item names.
func parseValues(name string, info controlInfo, values []string) ([]int32, error) {
	if len(values) == 0 {
		return nil, fmt.Errorf("alsa: %s: no value", name)
	}
	if len(values) != 1 && len(values) != info.count {
		return nil, fmt.Errorf("alsa: %s: %d values for %d channels", name, len(values), info.count)
	}
	out := make([]int32, info.count)
	for i := range out {
		v := values[0]
		if len(values) > 1 {
			v = values[i]
		}
		switch info.typ {
		case typeBoolean:
			switch v {
			case "on", "1", "true":
				out[i] = 1
			case "off", "0", "false":
				out[i] = 0
			default:
				return nil, fmt.Errorf("alsa: %s: %q is no boolean", name, v)
			}
		case typeInteger:
			var n int32
			if _, err := fmt.Sscanf(v, "%d", &n); err != nil {
				return nil, fmt.Errorf("alsa: %s: %q is no integer", name, v)
			}
			out[i] = n
		case typeEnumerated:
			found := false
			for k, item := range info.items {
				if item == v {
					out[i], found = int32(k), true
					break
				}
			}
			if !found {
				return nil, fmt.Errorf("alsa: %s: no item %q (have %v)", name, v, info.items)
			}
		default:
			return nil, fmt.Errorf("alsa: %s: type %d is not set by value", name, info.typ)
		}
	}
	return out, nil
}
