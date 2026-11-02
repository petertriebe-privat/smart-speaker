package leds

import (
	"bytes"
	"encoding/hex"
	"os"
)

// The Echo Dot 2's ring: one ISSI IS31FL3236 (36 PWM channels = 12 RGB segments), driven by its
// kernel driver. Its sysfs file frame takes all 36 channels as hex; segment n is channels
// 3n..3n+2 in RGB order, segment 0 between the mute and the volume-down button, clockwise
// (EchoLocal, internal/hardware/led/led.go, MIT). The ring's rotation against the assistant's 12 o'clock
// is led_offset, as on the Echo.
const dotLEDs = "/sys/bus/i2c/devices/0-003f/"

type dotFrame struct {
	f    *os.File
	last []byte
}

func openDotFrame() (*dotFrame, error) {
	// The driver's own boot animation would draw over ours.
	os.WriteFile(dotLEDs+"boot_animation", []byte("0\n"), 0o644)
	f, err := os.OpenFile(dotLEDs+"frame", os.O_WRONLY, 0)
	if err != nil {
		return nil, err
	}
	return &dotFrame{f: f}, nil
}

// write sends a frame when it changed (a sysfs write per frame, not per channel).
func (d *dotFrame) write(colors [Count]RGB, offset int, level float64) error {
	var ch [Count * 3]byte
	for pos, c := range colors {
		seg := ((pos+offset)%Count + Count) % Count
		ch[3*seg] = byte(float64(c.R) * level)
		ch[3*seg+1] = byte(float64(c.G) * level)
		ch[3*seg+2] = byte(float64(c.B) * level)
	}
	if bytes.Equal(ch[:], d.last) {
		return nil
	}
	if _, err := d.f.WriteAt([]byte(hex.EncodeToString(ch[:])+"\n"), 0); err != nil {
		return err
	}
	d.last = append(d.last[:0], ch[:]...)
	return nil
}
