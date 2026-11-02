//go:build !linux

package netx

import (
	"errors"
	"time"
)

func setClock(time.Time) error { return errors.New("setting the clock: Linux only") }
