package input

import (
	"os"
	"syscall"
	"unsafe"
)

// keyState: EVIOCGKEY, the current state of all keys as a bit field (KEY_MAX 0x2ff).
func keyState(path string) ([]byte, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	bits := make([]byte, 96)
	const ioRead = 2
	req := uintptr(ioRead)<<30 | uintptr(len(bits))<<16 | 'E'<<8 | 0x18
	if _, _, errno := syscall.Syscall(syscall.SYS_IOCTL, f.Fd(), req, uintptr(unsafe.Pointer(&bits[0]))); errno != 0 {
		return nil, errno
	}
	return bits, nil
}
