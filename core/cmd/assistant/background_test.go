package main

import (
	"strings"
	"testing"
	"time"
)

// The list shows the running tasks; drop marks one so its result is not told.
func TestBackgroundList(t *testing.T) {
	bg := &background{running: map[int]*bgTask{}}
	if got := bg.list(0); !strings.Contains(got, "No task") {
		t.Fatalf("empty: %q", got)
	}
	bg.running[1] = &bgTask{id: 1, task: "Vergleiche drei Angebote", started: time.Now()}
	bg.running[2] = &bgTask{id: 2, task: "Plane die Reise", started: time.Now()}
	got := bg.list(0)
	if !strings.Contains(got, "[1] Vergleiche") || !strings.Contains(got, "[2] Plane") || strings.Index(got, "[1]") > strings.Index(got, "[2]") {
		t.Fatalf("list: %q", got)
	}
	if got := bg.list(2); !strings.Contains(got, "not be told") || !bg.running[2].dropped {
		t.Fatalf("drop: %q", got)
	}
}
