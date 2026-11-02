// Package input reads the Echo's buttons (gpio-keys) and volume ring (rotary-encoder), and the
// Dot's buttons and privacy switch (dot.go).
package input

import (
	"encoding/binary"
	"log"
	"os"
	"time"

	"echo-assistant/internal/board"
)

// Measured 16.10.2026:
//   - the action (dot) button sends KEY_HELP with inverted polarity (0 = pressed, 1 = released);
//   - KEY_MUTE is no button but the state of the hardware mute latch (gpio VOLUME_MUTE): the
//     mute button toggles it together with its red LED; 0 = muted, 1 = microphones on;
//   - the ring sends REL_X ±1 per detent, -1 clockwise.
const (
	keyMute   = 113
	keyAction = 138
	evKey     = 1
	evRel     = 2
	eventSize = 16 // struct input_event on 32-bit ARM
)

type Kind int

const (
	ActionDown Kind = iota
	ActionUp
	Muted   // the hardware latch went to muted
	Unmuted // ... and back
	Ring    // Delta: detents, -1 clockwise
)

type Event struct {
	Kind  Kind
	Delta int
	Held  time.Duration // for ActionUp: how long the button was down
}

// Run reads both devices and sends events until the process ends.
func Run(out chan<- Event) {
	if board.Android {
		return // no buttons: the remote's keys reach the app
	}
	if board.Dot2 {
		runDot(out)
		return
	}
	go read("/dev/input/event0", out)
	go read("/dev/input/event1", out)
}

// MutedNow reads the hardware mute latch (EVIOCGKEY on the buttons' device).
func MutedNow() (bool, error) {
	if board.Android {
		return false, nil
	}
	if board.Dot2 {
		return mutedDot()
	}
	bits, err := keyState("/dev/input/event0")
	if err != nil {
		return false, err
	}
	return bits[keyMute/8]&(1<<(keyMute%8)) == 0, nil
}

func read(path string, out chan<- Event) {
	for {
		f, err := os.Open(path)
		if err != nil {
			log.Printf("[Input] %s: %v", path, err)
			time.Sleep(5 * time.Second)
			continue
		}
		buf := make([]byte, eventSize*16)
		var actionDown time.Time
		for {
			n, err := f.Read(buf)
			if err != nil {
				log.Printf("[Input] %s: %v", path, err)
				break
			}
			for i := 0; i+eventSize <= n; i += eventSize {
				typ := binary.LittleEndian.Uint16(buf[i+8:])
				code := binary.LittleEndian.Uint16(buf[i+10:])
				value := int32(binary.LittleEndian.Uint32(buf[i+12:]))
				switch {
				case typ == evRel && code == 0:
					out <- Event{Kind: Ring, Delta: int(value)}
				case typ == evKey && code == keyMute:
					if value == 0 {
						out <- Event{Kind: Muted}
					} else {
						out <- Event{Kind: Unmuted}
					}
				case typ == evKey && code == keyAction:
					if value == 0 {
						actionDown = time.Now()
						out <- Event{Kind: ActionDown}
					} else {
						out <- Event{Kind: ActionUp, Held: time.Since(actionDown)}
					}
				}
			}
		}
		f.Close()
		time.Sleep(time.Second)
	}
}
