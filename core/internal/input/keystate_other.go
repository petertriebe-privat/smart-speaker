//go:build !linux

package input

import "errors"

func keyState(string) ([]byte, error) { return nil, errors.New("input: Linux only") }
