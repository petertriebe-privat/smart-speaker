package main

import (
	"fmt"
	"net"
	"testing"

	"echo-assistant/internal/config"
	"echo-assistant/internal/hamqtt"
)

// Name and area from HA: the suggested area is no change; a new name keeps the room; a speaker
// without Spotify stays without.
func TestHAPlacement(t *testing.T) {
	s := config.Settings{Name: "", SpotifyName: "Jarvis Büro", Room: "Arbeitszimmer", HAArea: "Büro"}
	if _, changed := haPlacement(s, "Jarvis Büro", hamqtt.Placement{Area: "Büro"}); changed {
		t.Fatal("the suggested area counted as a change")
	}
	v, changed := haPlacement(s, "Jarvis Büro", hamqtt.Placement{Name: "Jarvis Arbeit"})
	if !changed || v.Name != "Jarvis Arbeit" || v.SpotifyName != "Jarvis Arbeit" || v.Room != "Arbeitszimmer" || v.HAArea != "Büro" {
		t.Fatalf("new name: %+v", v)
	}
	v, changed = haPlacement(s, "Jarvis Büro", hamqtt.Placement{Area: "Küche"})
	if !changed || v.Room != "Küche" || v.HAArea != "Küche" || v.Name != "" {
		t.Fatalf("new area: %+v", v)
	}
	s.SpotifyName = ""
	if v, _ = haPlacement(s, "Jarvis", hamqtt.Placement{Name: "Jarvis Bad"}); v.SpotifyName != "" {
		t.Fatalf("no Spotify here: %+v", v)
	}
}

// Timers from HA's text field: with or without "Timer" / "Wecker".
func TestParseHATimer(t *testing.T) {
	for text, want := range map[string]string{"06:30": "alarm 6:30", "Wecker 6 Uhr 45": "alarm 6:45",
		"10 Minuten Nudeln": "set 10m0s", "Timer eine halbe Stunde": "set 30m0s", "Timer löschen": "cancel", "Hallo": "",
		"unknown.": "", "Unavailable": ""} {
		c := parseHATimer(text)
		got := c.Action
		switch c.Action {
		case "alarm":
			got = fmt.Sprintf("alarm %d:%02d", c.Hour, c.Minute)
		case "set":
			got = "set " + c.After.String()
		}
		if got != want {
			t.Errorf("%q: %q, want %q", text, got, want)
		}
	}
}

// Night mode has its own volume: switched on the night's is in force and the ring changes it;
// switched off the day's comes back. The sensitivity shifts the cutoff within bounds.
func TestNightAndSensitivity(t *testing.T) {
	d := &device{cfg: &config.Config{Settings: config.Settings{StateDir: t.TempDir()}}}
	d.volume = 60
	d.nightVolume.Store(25)
	d.nightFromHA("ON")
	if d.volume != 25 || d.dayVolume != 60 {
		t.Fatalf("night on: volume %d, day %d", d.volume, d.dayVolume)
	}
	d.volume = 18 // the ring at night
	d.nightVolumeChanged(18)
	d.nightFromHA("OFF")
	if d.volume != 60 || d.nightVolume.Load() != 18 {
		t.Fatalf("night off: volume %d, night %d", d.volume, d.nightVolume.Load())
	}
	if v := config.LoadHA(d.cfg.Settings.StateDir); v.Night == nil || *v.Night || v.NightVolume != 18 {
		t.Fatalf("saved: %+v", v)
	}
	d.wakeShift.Store(int64(sensitivities["hoch"] * 1000))
	if c := d.wakeCutoff(0.45); c < 0.349 || c > 0.351 {
		t.Fatalf("hoch: %v", c)
	}
	d.wakeShift.Store(int64(sensitivities["niedrig"] * 1000))
	if c := d.wakeCutoff(0.9); c != 0.95 {
		t.Fatalf("bounded: %v", c)
	}
	if d.sensitivityText() != "niedrig" {
		t.Fatal(d.sensitivityText())
	}
}

// The id comes from wlan0's MAC: unique per Echo even when every host is "(none)".
func TestMacID(t *testing.T) {
	list := []net.Interface{
		{Name: "lo", Flags: net.FlagLoopback, HardwareAddr: net.HardwareAddr{0, 0, 0, 0, 0, 0}},
		{Name: "tailscale0"},
		{Name: "usb0", HardwareAddr: net.HardwareAddr{2, 0, 0, 0, 0, 1}},
		{Name: "wlan0", HardwareAddr: net.HardwareAddr{0xa0, 0xb1, 0xc2, 0xd3, 0xe4, 0xf5}},
	}
	if got := macID(list); got != "a0b1c2d3e4f5" {
		t.Fatalf("%q", got)
	}
	if got := macID(list[:3]); got != "020000000001" {
		t.Fatalf("without wlan0: %q", got)
	}
	if got := macID(nil); got != "" {
		t.Fatalf("none: %q", got)
	}
}

func TestParseWireless(t *testing.T) {
	text := "Inter-| sta-|   Quality        |   Discarded packets               | Missed | WE\n" +
		" face | tus | link level noise |  nwid  crypt   frag  retry   misc | beacon | 22\n" +
		" wlan0: 0000   54.  -56.  -256        0      0      0      0      0        0\n"
	if got := parseWireless(text); got != "-56" {
		t.Fatalf("%q", got)
	}
	// the Echo Dot 2's driver: the level as an unsigned byte; p2p0 without a link is skipped
	dot := "  p2p0: 0000    0     0     0        0      0      0      0      0        0\n" +
		" wlan0: 0000    0   199     0        0      0      0      0      0        0\n"
	if got := parseWireless(dot); got != "-57" {
		t.Fatalf("dot: %q", got)
	}
	if got := parseWireless(""); got != "None" {
		t.Fatalf("no wireless: %q", got)
	}
}
