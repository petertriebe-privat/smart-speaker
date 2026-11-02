package audio

import "syscall"

func mapFile(fd uintptr, size int) ([]byte, error) {
	return syscall.Mmap(int(fd), 0, size, syscall.PROT_READ, syscall.MAP_SHARED)
}

func unmapFile(data []byte) { syscall.Munmap(data) }
