package leds

import (
	"os"
	"syscall"
)

const i2cSlaveForce = 0x0706 // the kernel's lp5523 driver owns the addresses

// bus: the LP5523 chips on /dev/i2c-2, written directly. The stock driver's sysfs files
// (all_leds, brightness) only update its cache: measured 16.10.2026, the PWM registers stayed 0.
type bus struct {
	f    *os.File
	addr int // the chip selected last (-1 = none): one ioctl per chip change, not per register
}

func openBus() (*bus, error) {
	f, err := os.OpenFile("/dev/i2c-2", os.O_RDWR, 0)
	if err != nil {
		return nil, err
	}
	return &bus{f: f, addr: -1}, nil
}

func (b *bus) write(addr byte, data []byte) error {
	if int(addr) != b.addr {
		if _, _, errno := syscall.Syscall(syscall.SYS_IOCTL, b.f.Fd(), i2cSlaveForce, uintptr(addr)); errno != 0 {
			b.addr = -1
			return errno
		}
		b.addr = int(addr)
	}
	_, err := b.f.Write(data)
	return err
}
