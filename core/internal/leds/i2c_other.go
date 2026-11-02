//go:build !linux

package leds

import "errors"

type bus struct{}

func openBus() (*bus, error)            { return nil, errors.New("leds: Linux only") }
func (b *bus) write(byte, []byte) error { return nil }
