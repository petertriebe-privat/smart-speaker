package main

import (
	"cmp"
	"fmt"
	"log"
	"strconv"
	"strings"
	"time"

	"echo-assistant/internal/config"
	"echo-assistant/internal/errorlog"
	"echo-assistant/internal/hamqtt"
	"echo-assistant/internal/live"
	"echo-assistant/internal/timers"
	"echo-assistant/internal/tools"
)

// Settings and controls of the speaker in Home Assistant (#65): timers and alarms, a conversation
// started from an automation (a wake-up call at the speaker), the wake word's sensitivity, the
// role, the night mode and the ring. HA decides when (its automations); the speaker only applies.
// What HA sets is kept in ha.json, so it holds after a restart.

// sensitivities: the wake word select's options; the shift of both cutoffs (technique: a lower
// cutoff hears more and starts falsely more often).
var sensitivities = map[string]float64{"niedrig": 0.15, "normal": 0, "hoch": -0.1}

// nightRing caps the ring's brightness in night mode (0..255).
const nightRing = 30

// initHASettings applies what HA set (ha.json, already in the settings) at the start.
func (d *device) initHASettings() {
	s := d.cfg.Settings
	d.wakeShift.Store(int64(sensitivities[s.WakeSensitivity] * 1000))
	d.night.Store(s.Night)
	d.nightVolume.Store(int32(max(minVolume, cmp.Or(s.NightVolume, 25))))
	// 0 = switched off by HA while the ring was a light (until 29.10.2026): the default again.
	d.ringLevel.Store(int32(cmp.Or(max(s.LedLevel, 0), config.DefaultLedLevel)))
	d.applyRing()
}

// wakeCutoff: a cutoff shifted by the sensitivity HA set.
func (d *device) wakeCutoff(base float64) float64 {
	return max(0.15, min(0.95, base+float64(d.wakeShift.Load())/1000))
}

func (d *device) applyRing() {
	if d.ring == nil {
		return
	}
	level := int(d.ringLevel.Load())
	if d.night.Load() {
		level = min(level, nightRing)
	}
	d.ring.SetLevel(level)
}

func (d *device) saveHA(change func(*config.HAValues)) {
	if err := config.UpdateHA(d.cfg.Settings.StateDir, change); err != nil {
		errorlog.Add("HA device: %s: %v", config.HAFile, err)
	}
}

func (d *device) haControls() []hamqtt.Entity {
	sensitivityNames := []string{"niedrig", "normal", "hoch"}
	return []hamqtt.Entity{
		// Timers and alarms as by voice: "06:30", "10 Minuten Nudeln", "Wecker 6 Uhr 45". The field
		// shows empty (not HA's "Unknown") and is read only when a text is sent.
		{Component: "text", Key: "timer", Command: d.timerFromHA, State: func() string { return "" },
			Config: map[string]any{"name": "Timer oder Wecker stellen", "icon": "mdi:alarm-plus", "max": 100}},
		{Component: "button", Key: "timer_cancel", Command: func(string) { d.cancelTimersFromHA() }, Config: map[string]any{
			"name": "Timer löschen", "icon": "mdi:alarm-off", "payload_press": "PRESS"}},
		// A conversation as by the action button. Automations with an occasion use the event
		// jarvis_conversation with a room (a text field in HA's UI was sent by mistake, 27.10.2026).
		{Component: "button", Key: "talk", Command: func(string) { d.talkFromHA() }, Config: map[string]any{
			"name": "Gespräch beginnen", "icon": "mdi:account-voice", "payload_press": "PRESS"}},
		{Component: "sensor", Key: "last_person", State: d.lastPersonText, Config: map[string]any{
			"name": "Zuletzt gesprochen", "icon": "mdi:account-clock"}},
		{Component: "select", Key: "wake_sensitivity", State: d.sensitivityText, Command: d.sensitivityFromHA,
			Config: map[string]any{"name": "Wake-Word-Empfindlichkeit", "icon": "mdi:ear-hearing",
				"options": sensitivityNames, "entity_category": "config"}},
		{Component: "switch", Key: "may_lead", State: func() string { return hamqtt.OnOff(!d.cfg.Settings.NoLead) },
			Command: d.mayLeadFromHA, Config: map[string]any{"name": "Darf Hauptgerät werden", "icon": "mdi:crown-outline",
				"entity_category": "config"}},
		{Component: "switch", Key: "night", State: func() string { return hamqtt.OnOff(d.night.Load()) },
			Command: d.nightFromHA, Config: map[string]any{"name": "Nachtmodus", "icon": "mdi:weather-night"}},
		{Component: "number", Key: "night_volume", State: func() string { return strconv.Itoa(int(d.nightVolume.Load())) },
			Command: d.nightVolumeFromHA, Config: map[string]any{"name": "Lautstärke nachts", "icon": "mdi:volume-low",
				"min": minVolume, "max": 100, "step": 1, "mode": "slider", "unit_of_measurement": "%",
				"entity_category": "config"}},
		// A setting, not a lamp of the room (setRing): out of "Licht aus", area targets and Assist.
		{Component: "number", Key: "ring_brightness", State: d.ringText, Command: d.ringFromHA, Config: map[string]any{
			"name": "Ring-Helligkeit", "icon": "mdi:circle-outline", "min": 1, "max": 100, "step": 1,
			"mode": "slider", "unit_of_measurement": "%", "entity_category": "config"}},
	}
}

// timerFromHA: the timer parser of the quick commands; "Timer" or "Wecker" is put in front when
// the text has neither ("06:30", "10 Minuten").
func (d *device) timerFromHA(text string) {
	if d.timers == nil {
		return
	}
	cmd := parseHATimer(text)
	var t timers.Timer
	switch cmd.Action {
	case "alarm":
		t = d.timers.Add("alarm", 0, cmd.Hour, cmd.Minute, cmd.Label)
	case "set":
		t = d.timers.Add("timer", cmd.After, 0, 0, cmd.Label)
	case "cancel":
		d.cancelTimersFromHA()
		return
	default:
		errorlog.Add("HA device: timer %q not understood", text)
		return
	}
	log.Printf("[HA device] timer: %s", timers.Confirmation(t, time.Now()))
	d.ha.Changed()
}

// haPlaceholder: what HA's UI sends from an empty text field ("unknown." started a conversation
// in the study, 27.10.2026): never a command.
func haPlaceholder(text string) bool {
	switch strings.ToLower(strings.Trim(strings.TrimSpace(text), ".")) {
	case "", "unknown", "unavailable", "none", "unbekannt", "nicht verfügbar":
		return true
	}
	return false
}

func parseHATimer(text string) timers.Command {
	if haPlaceholder(text) {
		return timers.Command{}
	}
	cmd := timers.Parse(text)
	for _, prefix := range []string{"Wecker um ", "Timer "} {
		if cmd.Action == "" {
			cmd = timers.Parse(prefix + text)
		}
	}
	return cmd
}

func (d *device) cancelTimersFromHA() {
	if d.timers == nil {
		return
	}
	d.stopTimer("Home Assistant")
	log.Printf("[HA device] timers cancelled: %d", d.timers.Cancel(0))
}

// talkFromHA: the action button's conversation; not while muted (the button gives a low tone
// then) or while one runs.
func (d *device) talkFromHA() {
	if d.muted.Load() || d.session.Open() {
		log.Printf("[HA device] talk: muted %v, open %v: nothing", d.muted.Load(), d.session.Open())
		return
	}
	d.session.Start(live.Request{})
}

func (d *device) lastPersonText() string {
	if name, _ := d.lastPerson.Load().(string); name != "" {
		return name
	}
	return "–"
}

func (d *device) sensitivityText() string {
	for name, shift := range sensitivities {
		if int64(shift*1000) == d.wakeShift.Load() {
			return name
		}
	}
	return "normal"
}

func (d *device) sensitivityFromHA(name string) {
	shift, ok := sensitivities[name]
	if !ok {
		return
	}
	d.wakeShift.Store(int64(shift * 1000))
	d.saveHA(func(v *config.HAValues) { v.WakeSensitivity = name })
}

// mayLeadFromHA: the role takes a restart (the election reads it at the start, failover.go).
func (d *device) mayLeadFromHA(payload string) {
	noLead := payload != "ON"
	if noLead == d.cfg.Settings.NoLead {
		return
	}
	d.saveHA(func(v *config.HAValues) { v.NoLead = &noLead })
	go d.restart(fmt.Sprintf("Home Assistant: may become primary %v", !noLead))
}

// nightFromHA: the night mode has its own volume. Switched on, the day's volume is kept and the
// night volume is in force: the ring, voice, Spotify and HA's volume set it from then on.
// Switched off, the day's volume comes back and the night's stays for the next night.
func (d *device) nightFromHA(payload string) {
	on := payload == "ON"
	d.volMu.Lock()
	if on != d.night.Load() {
		if on {
			d.dayVolume = d.volume
			d.volume = int(d.nightVolume.Load())
		} else {
			d.nightVolume.Store(int32(d.volume))
			d.volume = d.dayVolume
		}
		d.night.Store(on)
	}
	night := int(d.nightVolume.Load())
	d.volMu.Unlock()
	d.saveHA(func(v *config.HAValues) { v.Night, v.NightVolume = &on, night })
	d.applyRing()
	d.kickVolume()
}

func (d *device) nightVolumeFromHA(payload string) {
	v, err := strconv.ParseFloat(payload, 64)
	if err != nil {
		return
	}
	level := max(minVolume, min(100, int(v+0.5)))
	d.nightVolume.Store(int32(level))
	d.saveHA(func(h *config.HAValues) { h.NightVolume = level })
	if d.night.Load() { // in force now
		d.volMu.Lock()
		d.volume = level
		d.volMu.Unlock()
		d.kickVolume()
	}
}

// nightVolumeChanged: the volume changed in night mode (volumeWorker): it is the night volume,
// saved once the ring has rested for a few seconds (not on every detent).
func (d *device) nightVolumeChanged(v int) {
	d.nightVolume.Store(int32(v))
	d.volMu.Lock()
	defer d.volMu.Unlock()
	if d.nightSave != nil {
		d.nightSave.Stop()
	}
	d.nightSave = time.AfterFunc(5*time.Second, func() {
		level := int(d.nightVolume.Load())
		d.saveHA(func(h *config.HAValues) { h.NightVolume = level })
	})
}

// The ring's brightness is a setting in percent (1..100), never off and never a lamp: it is the
// only answer to the wake word (the start is silent). As an HA light, "Licht aus" in the room took
// it along with the room's lights and the Echo seemed dead (28.10.2026); now it changes only when
// someone means the ring: HA's setting or set_ring_brightness.

func ringPercent(level int) int { return max(1, min(100, (level*100+127)/255)) }

func (d *device) ringText() string { return strconv.Itoa(ringPercent(int(d.ringLevel.Load()))) }

func (d *device) ringFromHA(payload string) {
	if percent, err := strconv.ParseFloat(strings.TrimSpace(payload), 64); err == nil {
		d.setRing(int(percent + 0.5))
	}
}

// setRing sets the brightness in percent (clamped to 1..100) and returns the new value.
func (d *device) setRing(percent int) int {
	percent = max(1, min(100, percent))
	level := max(1, (percent*255+50)/100)
	d.ringLevel.Store(int32(level))
	d.saveHA(func(v *config.HAValues) { v.LedLevel = &level })
	d.applyRing()
	if d.ha != nil {
		d.ha.Changed()
	}
	return percent
}

func (d *device) addRingTool() {
	d.registry.Add(tools.Tool{
		Name: "set_ring_brightness",
		Description: "Sets the brightness of this speaker's own light ring (the assistant's ring, its feedback). Only when " +
			"the user explicitly names the ring ('Lichtring', 'Ring', 'dein Licht'); never for the room's " +
			"lights ('Licht aus', 'Licht dimmen' are lamps in Home Assistant). It cannot be switched off. Give " +
			"level (percent) or step (e.g. -20 for 'dunkler'), not both. Returns the new brightness.",
		Parameters: `{"type": "object", "additionalProperties": false,
			"properties": {"level": {"type": "integer", "minimum": 1, "maximum": 100},
			               "step": {"type": "integer", "minimum": -100, "maximum": 100}},
			"required": []}`,
		Run: func(args map[string]any, _ tools.Context) string {
			level, hasLevel := tools.Int(args, "level")
			step, hasStep := tools.Int(args, "step")
			switch {
			case hasStep && step != 0: // a step wins, as in set_volume
				level = ringPercent(int(d.ringLevel.Load())) + step
			case !hasLevel:
				return "Error: give level or step"
			}
			return fmt.Sprintf("Ring brightness is now %d %%.", d.setRing(level))
		},
	})
}
