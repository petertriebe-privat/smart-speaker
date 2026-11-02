package main

import (
	"log"
	"os"
	"strings"
	"time"

	"echo-assistant/internal/board"
	"echo-assistant/internal/errorlog"
)

// The Fire TVs keep their the assistant app working without the PC (29.10.2026: a restart of the stick
// enabled Alexa's voice app again and took the app's microphone, the remote's key opened Alexa;
// adbd refuses a connection from the stick itself, so it goes over Home Assistant's ADB):
//   - The assistant on a stick repairs its own stick at start and every few minutes (firetv.Keep:
//     Alexa's voice app off unless fire_tv_alexa, microphone, overlay, no microphone symbol).
//   - The primary device starts the app on every stick that has it and where it does not run
//     (firetv.KeepRunning): after a restart without the boot broadcast, after a crash.

const (
	tvKeepFirst = 30 * time.Second // after the start: HA and the stick's own app are up
	tvKeepEvery = 10 * time.Minute
)

// tvPackage: the Android app of the assistant (Core.java passes it on the stick).
func tvPackage() string {
	if p := os.Getenv("ASSISTANT_PACKAGE"); p != "" {
		return p
	}
	return "dev.smartspeaker.screen"
}

func (d *device) keepTVs(s *services) {
	if s.sticks == nil {
		return
	}
	time.Sleep(tvKeepFirst)
	reported := ""
	for {
		if board.FireTV {
			repaired, errText := s.sticks.Keep(d.cfg.Settings.Room, tvPackage(), d.cfg.Settings.FireTVAlexa)
			switch {
			case errText != "" && errText != reported:
				log.Printf("[TV keep] %s", errText)
				errorlog.Add("The TV's settings could not be checked over Home Assistant's ADB: %s", errText)
			case len(repaired) > 0:
				log.Printf("[TV keep] repaired: %s", strings.Join(repaired, ", "))
			}
			reported = errText
		}
		if d.isPrimary() {
			if started := s.sticks.KeepRunning(tvPackage()); len(started) > 0 {
				log.Printf("[TV keep] assistant app started on %s", strings.Join(started, ", "))
			}
		}
		time.Sleep(tvKeepEvery)
	}
}
