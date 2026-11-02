package main

import (
	"cmp"
	"echo-assistant/internal/prompts"
	"encoding/hex"
	"fmt"
	"log"
	"net"
	"os"
	"strconv"
	"strings"
	"time"

	"echo-assistant/internal/board"
	"echo-assistant/internal/config"
	"echo-assistant/internal/errorlog"
	"echo-assistant/internal/hamqtt"
	"echo-assistant/internal/memguard"
	"echo-assistant/internal/mqtt"
	"echo-assistant/internal/tools"
)

// This speaker as a Home Assistant device (#65): MQTT discovery puts it into HA with its state,
// volume, diagnostics, events and an announcement entity; the owner names it and puts it into an
// area in HA's UI, and the speaker takes that name (peer link, Spotify Connect, SIP display name)
// and room (ha.json in state_dir, a restart once idle). device.json stays the fallback.

// haName: the HA device's name: the Bluetooth one (speech.go), else "Jarvis".
func (d *device) haName() string {
	return cmp.Or(d.speakerName(), prompts.Name())
}

// haDeviceID: stable across renames and unique: the WiFi's MAC address. Not the host name: both
// Echo Gen 1 report "(none)" and became one device in HA (27.10.2026).
// The Fire TV app passes its Android id in ASSISTANT_ID: Android hides the MAC address from apps.
func haDeviceID() string {
	if id := mqtt.TopicID(os.Getenv("ASSISTANT_ID")); id != "" {
		return prompts.Slug() + "_tv_" + id
	}
	return prompts.Slug() + "_" + cmp.Or(macID(interfaces()), "speaker")
}

func interfaces() []net.Interface {
	list, _ := net.Interfaces()
	return list
}

// macID: wlan0's hardware address as hex, else the first other one with an address.
func macID(list []net.Interface) string {
	best := ""
	for _, i := range list {
		if len(i.HardwareAddr) != 6 || i.Flags&net.FlagLoopback != 0 {
			continue
		}
		id := hex.EncodeToString(i.HardwareAddr)
		if i.Name == "wlan0" {
			return id
		}
		if best == "" {
			best = id
		}
	}
	return best
}

const haEvents = "events"

func (d *device) setupHADevice() {
	sec := d.cfg.Secrets
	url := sec.String("MQTT_URL")
	if url == "" {
		log.Print("[HA device] off (MQTT_URL missing)")
		return
	}
	started := time.Now()
	ha := &hamqtt.Device{ID: haDeviceID(), Name: d.haName(), Model: board.Name(), Version: revision(),
		SuggestedArea: cmp.Or(d.cfg.Settings.HAArea, d.cfg.Settings.Room)}
	ha.Entities = []hamqtt.Entity{
		{Component: "sensor", Key: "state", State: d.peerState, Config: map[string]any{"name": "Status",
			"icon": "mdi:account-voice", "device_class": "enum",
			"options": []string{"starting", "idle", "conversation", "intercom", "call", "phone", "music"}}},
		{Component: "number", Key: "volume", State: d.volumeText, Command: d.volumeFromHA, Config: map[string]any{
			"name": "Lautstärke", "icon": "mdi:volume-high", "min": minVolume, "max": 100, "step": 1,
			"mode": "slider", "unit_of_measurement": "%"}},
		// The Echo's mute LED is switched by its button alone: a mute from HA would leave it dark,
		// so HA only shows it.
		{Component: "binary_sensor", Key: "muted", State: func() string { return hamqtt.OnOff(d.muted.Load()) },
			Config: map[string]any{"name": "Mikrofon stumm", "icon": "mdi:microphone-off"}},
		{Component: "sensor", Key: "next_timer", State: d.nextTimerText, Config: map[string]any{
			"name": "Nächster Timer", "device_class": "timestamp", "icon": "mdi:timer-outline"}},
		{Component: "button", Key: "stop", Command: func(string) { d.stopFromHA() }, Config: map[string]any{
			"name": "Stopp", "icon": "mdi:stop-circle-outline", "payload_press": "PRESS"}},
		{Component: "notify", Key: "say", Command: d.sayFromHA, Config: map[string]any{
			"name": "Ansage", "icon": "mdi:bullhorn"}},
		{Component: "event", Key: haEvents, Config: map[string]any{"name": "Ereignis", "icon": "mdi:gesture-tap-button",
			"event_types": []string{"wake_word", "button", "conversation_started", "conversation_ended", "timer_ringing",
				"restarting"}}},
		// Diagnostics: is it well, before SSH would stop answering (memory guard).
		{Component: "binary_sensor", Key: "primary", State: func() string { return hamqtt.OnOff(d.isPrimary()) },
			Config: map[string]any{"name": "Hauptgerät", "icon": "mdi:crown", "entity_category": "diagnostic"}},
		{Component: "sensor", Key: "started", State: func() string { return started.Format(time.RFC3339) },
			Config: map[string]any{"name": "Gestartet", "device_class": "timestamp", "entity_category": "diagnostic"}},
		{Component: "sensor", Key: "memory", State: func() string { rss, _ := memguard.Resident(); return mb(rss) },
			Config: map[string]any{"name": "Speicher " + prompts.Name(), "unit_of_measurement": "MB", "icon": "mdi:memory",
				"state_class": "measurement", "entity_category": "diagnostic"}},
		{Component: "sensor", Key: "memory_free", State: func() string { _, free := memguard.Resident(); return mb(free) },
			Config: map[string]any{"name": "Speicher frei", "unit_of_measurement": "MB", "icon": "mdi:memory",
				"state_class": "measurement", "entity_category": "diagnostic"}},
		{Component: "sensor", Key: "wifi", State: wifiSignal, Config: map[string]any{"name": "WLAN-Signal",
			"device_class": "signal_strength", "unit_of_measurement": "dBm", "state_class": "measurement",
			"entity_category": "diagnostic"}},
		{Component: "sensor", Key: "version", State: revision, Config: map[string]any{"name": "Version",
			"icon": "mdi:source-commit", "entity_category": "diagnostic"}},
		{Component: "sensor", Key: "last_error", State: lastError, Config: map[string]any{"name": "Letzter Fehler",
			"icon": "mdi:alert-circle-outline", "entity_category": "diagnostic"}},
	}
	ha.Entities = append(ha.Entities, d.haControls()...)
	ha.Retired = []string{"text/" + ha.ID + "/conversation", // a button since 27.10.2026
		"light/" + ha.ID + "/ring"} // the setting ring_brightness since 29.10.2026 (hacontrols.go)
	var retired []string
	ha.Entities, retired = tvEntities(ha.ID, ha.Entities) // the stick lacks ring, mute, wake word (tvboard.go)
	ha.Retired = append(ha.Retired, retired...)
	if board.FireTV {
		ha.Entities = append(ha.Entities, d.realtimeEntity()) // GPT Live or Realtime (tvboard.go)
	}
	if err := ha.Start(url, sec.String("MQTT_USER"), sec.String("MQTT_PASSWORD")); err != nil {
		errorlog.Add("HA device: %v", err)
		return
	}
	d.ha = ha
	d.setupHAAssistant(url, sec.String("MQTT_USER"), sec.String("MQTT_PASSWORD"))
	if haURL, token := sec.String("HA_URL"), sec.String("HA_TOKEN"); haURL != "" && token != "" {
		go hamqtt.WatchRegistry(haURL, token, ha.ID, d.placedInHA)
	}
}

func mb(bytes int64) string {
	if bytes <= 0 {
		return "None"
	}
	return strconv.FormatInt(bytes>>20, 10)
}

// isPrimary: a change of the role restarts the assistant (failover.go), so the settings hold it.
func (d *device) isPrimary() bool {
	return d.cfg.Settings.Primary
}

func (d *device) volumeText() string {
	d.volMu.Lock()
	defer d.volMu.Unlock()
	return strconv.Itoa(d.volApplied)
}

func (d *device) volumeFromHA(payload string) {
	v, err := strconv.ParseFloat(payload, 64)
	if err != nil {
		return
	}
	d.volMu.Lock()
	d.volume = max(minVolume, min(100, int(v+0.5)))
	d.volMu.Unlock()
	d.kickVolume()
}

func (d *device) nextTimerText() string {
	if d.timers == nil {
		return "None"
	}
	if list := d.timers.List(); len(list) > 0 {
		return list[0].At.Format(time.RFC3339)
	}
	return "None"
}

// stopFromHA: what the action button would end: a ringing timer, the conversation, music here.
func (d *device) stopFromHA() {
	if d.stopTimer("Home Assistant") {
		return
	}
	if d.session.Open() {
		d.session.Close()
		return
	}
	if d.music != nil && d.music.Playing() {
		d.registry.Call("music_control", fmt.Sprintf(`{"action": "pause", "speaker": %q}`, d.cfg.Settings.SpotifyName),
			tools.Context{Channel: tools.Console, Origin: "Home Assistant"})
	}
}

// sayFromHA: notify.send_message to this speaker says the text here (as jarvis_announce, but in
// this room).
func (d *device) sayFromHA(text string) {
	if text = strings.TrimSpace(text); !haPlaceholder(text) {
		log.Printf("[HA device] say: %s", d.svc.announcer.Speak(text))
	}
}

// haFire: an event for HA's automations (nil-safe before the device is up).
func (d *device) haFire(eventType string, attrs map[string]any) {
	if d.ha != nil {
		d.ha.Fire(haEvents, eventType, attrs)
	}
}

// wifiSignal from /proc/net/wireless: "wlan0: 0000   54.  -56.  -256 ...".
func wifiSignal() string {
	data, _ := os.ReadFile("/proc/net/wireless")
	return parseWireless(string(data))
}

func parseWireless(text string) string {
	for _, line := range strings.Split(text, "\n") {
		name, rest, ok := strings.Cut(line, ":")
		if !ok || strings.Contains(name, "|") {
			continue
		}
		f := strings.Fields(rest)
		if len(f) < 3 {
			continue
		}
		v, err := strconv.ParseFloat(strings.TrimSuffix(f[2], "."), 64)
		if err == nil && v > 0 {
			v -= 256 // the 8-bit form of older drivers: the Dot's 199 is -57 dBm
		}
		if err == nil && v < 0 && v > -120 {
			return strconv.Itoa(int(v))
		}
	}
	return "None"
}

func lastError() string {
	text := errorlog.Last()
	if text == "" {
		return "–"
	}
	if r := []rune(text); len(r) > 250 { // HA keeps 255 characters of a state
		text = string(r[:250]) + "…"
	}
	return text
}

// placedInHA: the owner named the speaker or moved it to an area in HA's UI. A change is kept in
// ha.json and taken over by a restart once nothing runs (the name is in librespot, the SIP
// registration and the peer link; a restart takes it everywhere at once).
func (d *device) placedInHA(p hamqtt.Placement) {
	s := d.cfg.Settings
	v, changed := haPlacement(s, d.haName(), p)
	log.Printf("[HA device] in HA: name %q, area %q (changed %v)", p.Name, p.Area, changed)
	if !changed {
		return
	}
	err := config.UpdateHA(s.StateDir, func(h *config.HAValues) {
		h.Name, h.SpotifyName, h.Room, h.HAArea = v.Name, v.SpotifyName, v.Room, v.HAArea
	})
	if err != nil {
		errorlog.Add("HA device: %s: %v", config.HAFile, err)
		return
	}
	go d.restart(fmt.Sprintf("Home Assistant: name %q, room %q", cmp.Or(v.Name, d.haName()), v.Room))
}

// haPlacement: the values for ha.json and whether they differ from what runs now (name: the
// name shown now). The area HA got as suggested_area (ha_area, else room) is no change; what HA
// does not set keeps the running value, so a new name does not lose a room set earlier.
func haPlacement(s config.Settings, name string, p hamqtt.Placement) (config.HAValues, bool) {
	v := config.HAValues{Name: s.Name, SpotifyName: s.SpotifyName, Room: s.Room, HAArea: s.HAArea}
	changed := false
	if p.Name != "" && (p.Name != name || s.SpotifyName != "" && s.SpotifyName != p.Name) {
		v.Name, changed = p.Name, true
		if s.SpotifyName != "" { // "" = no Spotify speaker here: stays so
			v.SpotifyName = p.Name
		}
	}
	if p.Area != "" && p.Area != cmp.Or(s.HAArea, s.Room) {
		v.Room, v.HAArea, changed = p.Area, p.Area, true
	}
	return v, changed
}
