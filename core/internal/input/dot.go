package input

import (
	"encoding/binary"
	"log"
	"os"
	"path/filepath"
	"strings"
	"time"

	"echo-assistant/internal/board"
)

// The Echo Dot 2's buttons (read 20.10.2026; codes as EchoLocal's): mtk-kpd sends the action
// button (KEY_HELP 138) and KEY_MUTE, gpio-keys "keys" the volume buttons (114 down, 115 up,
// with autorepeat); 1 = pressed, 0 = released, 2 = repeat. The mute button is no latch here: the
// privacy driver cuts the microphones itself and reports it in privacy_state (1 = cut) about
// 300 ms after the press. Never read power_button_state in that directory: it crashes the kernel.
const (
	keyVolumeDown = 114
	keyVolumeUp   = 115
	dotVolumeStep = 5 // ring detents per press (x2 %, as the Echo's ring)
	mutePoll      = 250 * time.Millisecond
)

func runDot(out chan<- Event) {
	paths, _ := filepath.Glob("/dev/input/event*")
	for _, p := range paths {
		go readDot(p, out)
	}
	go func() {
		last, err := mutedDot()
		for {
			time.Sleep(mutePoll)
			m, e := mutedDot()
			if e != nil {
				if err == nil {
					log.Printf("[Input] privacy state: %v", e)
				}
				err = e
				continue
			}
			if err == nil && m != last {
				if m {
					out <- Event{Kind: Muted}
				} else {
					out <- Event{Kind: Unmuted}
				}
			}
			last, err = m, nil
		}
	}()
}

func mutedDot() (bool, error) {
	b, err := os.ReadFile(board.PrivacyFile("privacy_state"))
	if err != nil {
		return false, err
	}
	return strings.TrimSpace(string(b)) == "1", nil
}

func readDot(path string, out chan<- Event) {
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
				if binary.LittleEndian.Uint16(buf[i+8:]) != evKey {
					continue
				}
				code := binary.LittleEndian.Uint16(buf[i+10:])
				value := int32(binary.LittleEndian.Uint32(buf[i+12:]))
				switch {
				case code == keyAction && value == 1:
					actionDown = time.Now()
					out <- Event{Kind: ActionDown}
				case code == keyAction && value == 0:
					out <- Event{Kind: ActionUp, Held: time.Since(actionDown)}
				case code == keyVolumeUp && value > 0:
					out <- Event{Kind: Ring, Delta: -dotVolumeStep} // -1 is clockwise: louder
				case code == keyVolumeDown && value > 0:
					out <- Event{Kind: Ring, Delta: dotVolumeStep}
				}
			}
		}
		f.Close()
		time.Sleep(time.Second)
	}
}
