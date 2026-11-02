//go:build !linux

package audio

import "errors"

// The DSP buffer exists on the Echo only; other systems build for tests.
func mapFile(uintptr, int) ([]byte, error) { return nil, errors.New("mic: Linux only") }

func unmapFile([]byte) {}
