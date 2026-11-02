package main

import (
	"testing"
	"time"
)

func TestClockAnswer(t *testing.T) {
	at := time.Date(2026, 10, 28, 21, 7, 0, 0, time.Local)
	for action, want := range map[string]string{
		"time": "Es ist 21 Uhr 7.",
		"date": "Heute ist Mittwoch, der 28. Oktober.",
	} {
		if got := clockAnswer(action, at); got != want {
			t.Errorf("%s: %q, want %q", action, got, want)
		}
	}
	if got := clockAnswer("time", at.Add(53*time.Minute)); got != "Es ist 22 Uhr." {
		t.Errorf("full hour: %q", got)
	}
}
