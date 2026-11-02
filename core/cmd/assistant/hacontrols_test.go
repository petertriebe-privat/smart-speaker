package main

import (
	"testing"

	"echo-assistant/internal/config"
	"echo-assistant/internal/tools"
)

// The ring is a setting in percent, never off: a saved 0 from the light days becomes the default,
// HA's number and set_ring_brightness change it, 0 is clamped to 1 %.
func TestRingBrightness(t *testing.T) {
	d := &device{cfg: &config.Config{Settings: config.Settings{StateDir: t.TempDir()}}, registry: &tools.Registry{}}
	d.initHASettings()
	if got := d.ringLevel.Load(); got != config.DefaultLedLevel {
		t.Fatalf("a saved 0 at the start: %d", got)
	}
	if got := d.ringText(); got != "47" {
		t.Fatalf("120/255 in percent: %s", got)
	}
	d.ringFromHA("20")
	if got := d.ringText(); got != "20" {
		t.Fatalf("from HA: %s", got)
	}
	if got := config.LoadHA(d.cfg.Settings.StateDir).LedLevel; got == nil || *got != 51 {
		t.Fatalf("saved: %v", got)
	}
	d.ringFromHA("0")
	if got := d.ringLevel.Load(); got < 1 {
		t.Fatalf("0 switched it off: %d", got)
	}
	d.addRingTool()
	ctx := tools.Context{Channel: tools.Console}
	if got := d.registry.Call("set_ring_brightness", `{"level":0,"step":30}`, ctx); got != "Ring brightness is now 31 %." {
		t.Fatalf("step: %q", got)
	}
	if got := d.registry.Call("set_ring_brightness", `{"level":60}`, ctx); got != "Ring brightness is now 60 %." {
		t.Fatalf("level: %q", got)
	}
}
