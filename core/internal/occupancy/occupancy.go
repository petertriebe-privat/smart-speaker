// Package occupancy: the presence sensor, as devices/atoms3r/src/system/occupancy.cpp. On the M5 it
// is an STHS34PF80 (M5 Unit TMOS, I2C 0x5A) on the pyramid's Grove port; infrared, so it also
// notices someone sitting still, not only movement. The room counts as occupied from the first
// motion or (trusted) presence flag; empty after ROOM_EMPTY_AFTER_MS without one once someone
// walked out, else ROOM_STILL_EMPTY_MS: that is the assistant's own view. Home Assistant gets what the
// sensor sees now as binary_sensor.jarvis_presence (state set over REST, at most every
// ROOM_REPORT_MIN_MS): holding times belong in HA's automations ("for:").
//
// The Echo has no such sensor wired up: the chip is reached through the Bus interface, which the
// caller implements if an I2C device is available (e.g. /dev/i2c-N via ioctl). Without a Bus
// the tool answers "Error: no presence sensor connected." as the M5 without the chip does.
package occupancy

import (
	"fmt"
	"log"
	"math"
	"sync"
	"time"

	"echo-assistant/internal/homeassistant"
	"echo-assistant/internal/prompts"
	"echo-assistant/internal/tools"
)

// Entity is the HA entity the state is reported to, named after the assistant
// (binary_sensor.jarvis_presence by default).
func Entity() string { return "binary_sensor." + prompts.Slug() + "_presence" }

// STHS34PF80 registers (ST's sths34pf80_reg.h).
const (
	address    = 0x5A // 7-bit I2C address
	whoAmI     = 0x0F
	ctrl1      = 0x20
	funcStatus = 0x25
	tAmbientL  = 0x28
	tPresenceL = 0x3A
	chipID     = 0xD3
	// 4 Hz with the default averaging (128). The chip's filters run at the data rate: 1 Hz with
	// 1024 samples was tried (04.10.) and let the empty room drift up to presence 775, flag on
	// for good.
	bdu, odr4Hz  = 0x10, 0x05
	presenceFlag = 0x04
	motionFlag   = 0x02

	pollInterval        = 250 * time.Millisecond
	temperatureInterval = 10 * time.Second
	reportRetry         = 30 * time.Second
)

// Address is the sensor's 7-bit I2C address, for the Bus implementation.
const Address = address

// Bus reads and writes the sensor's registers (register auto-increment on reads).
type Bus interface {
	ReadReg(reg byte, n int) ([]byte, error)
	WriteReg(reg, value byte) error
}

// Sensor is the presence sensor with its room logic.
type Sensor struct {
	// ReportAllowed, if set, is asked before a report to HA (the M5 reports only when
	// update(reportAllowed) says REST may block, e.g. not during a conversation). nil = always.
	ReportAllowed func() bool
	// OnChange, if set, is called (in the Run goroutine) when the room becomes occupied or empty.
	OnChange func(occupied bool)

	ha  *homeassistant.HA
	bus Bus

	emptyAfter, stillEmpty, presenceOnly, reportEvery time.Duration
	leaveMotion, comeInMotion                         int

	mu             sync.Mutex
	found, debug   bool
	occupied       bool // The assistant's own, held
	detected       bool // what the sensor sees now: to HA
	changedAt      time.Time
	lastMotionAt   time.Time
	temperature    float64 // NaN = unknown; the sensor's own, at the device
	lastSeen       time.Time
	lastMotionFlag time.Time
	trusted        bool // presence flag counts until someone walked out (or since the start)
	reportPending  bool
	lastReport     time.Time
	retryAt        time.Time
	lastDebug      time.Time
}

// New creates the sensor; bus may be nil (no sensor, the tool then reports that).
func New(ha *homeassistant.HA, bus Bus) *Sensor {
	return &Sensor{
		ha: ha, bus: bus, temperature: math.NaN(),
		emptyAfter:   time.Duration(prompts.Int("ROOM_EMPTY_AFTER_MS", 60000)) * time.Millisecond,
		stillEmpty:   time.Duration(prompts.Int("ROOM_STILL_EMPTY_MS", 10*60000)) * time.Millisecond,
		presenceOnly: time.Duration(prompts.Int("ROOM_PRESENCE_ONLY_MS", 30*60000)) * time.Millisecond,
		reportEvery:  time.Duration(prompts.Int("ROOM_REPORT_MS", 5*60000)) * time.Millisecond,
		leaveMotion:  prompts.Int("ROOM_LEAVE_MOTION", 1000),
		comeInMotion: prompts.Int("ROOM_COME_IN_MOTION", 400),
		trusted:      false,
	}
}

// Available says whether the sensor was found.
func (s *Sensor) Available() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.found
}

// SomeoneThere: the room counts as occupied (false without the sensor).
func (s *Sensor) SomeoneThere() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.found && s.occupied
}

// ToggleDebug switches the raw presence and motion values in the log (once a second).
func (s *Sensor) ToggleDebug() {
	s.mu.Lock()
	s.debug = !s.debug
	on := s.debug
	s.mu.Unlock()
	state := "off"
	if on {
		state = "on"
	}
	log.Printf("[Room] raw values %s", state)
}

// begin probes the chip and starts it (the M5's begin()).
func (s *Sensor) begin() {
	found := false
	if s.bus != nil {
		id, err := s.bus.ReadReg(whoAmI, 1)
		found = err == nil && len(id) == 1 && id[0] == chipID && s.bus.WriteReg(ctrl1, bdu|odr4Hz) == nil
	}
	state := "not found"
	if found {
		state = "ready"
	}
	log.Printf("[Room] presence sensor %s", state)
	s.mu.Lock()
	s.found = found
	if found {
		s.changedAt = time.Now()
	}
	s.mu.Unlock()
}

// Run probes the sensor and then polls it every 250 ms (the M5's update(), POLL_MS), reads the
// temperature every 10 s and reports to HA on a change and every ROOM_REPORT_MS (default 5 min).
// Start it as a goroutine; it returns at once without a sensor.
func (s *Sensor) Run() {
	s.begin()
	if !s.Available() {
		return
	}
	var lastTemperature time.Time
	for range time.Tick(pollInterval) {
		now := time.Now()
		flags, okFlags := s.readByte(funcStatus)
		raw, okValues := s.readBytes(tPresenceL, 4)
		presence, motion := 0, 0
		if okValues {
			presence = int(int16(uint16(raw[0]) | uint16(raw[1])<<8))
			motion = int(int16(uint16(raw[2]) | uint16(raw[3])<<8))
		}
		s.tick(now, okFlags, flags, okValues, presence, motion)
		if now.Sub(lastTemperature) >= temperatureInterval {
			lastTemperature = now
			if t, ok := s.readBytes(tAmbientL, 2); ok {
				s.mu.Lock()
				s.temperature = float64(int16(uint16(t[0])|uint16(t[1])<<8)) / 100
				s.mu.Unlock()
			}
		}
		if (s.ReportAllowed == nil || s.ReportAllowed()) && s.ha != nil && s.ha.Configured() {
			s.maybeReport(now)
		}
	}
}

func (s *Sensor) readBytes(reg byte, n int) ([]byte, bool) {
	data, err := s.bus.ReadReg(reg, n)
	return data, err == nil && len(data) == n
}

func (s *Sensor) readByte(reg byte) (byte, bool) {
	data, ok := s.readBytes(reg, 1)
	if !ok {
		return 0, false
	}
	return data[0], true
}

// tick is the room logic of one poll: flags from FUNC_STATUS, presence and motion values from the
// sensor. Pure apart from the state, so it can be tested.
func (s *Sensor) tick(now time.Time, okFlags bool, flags byte, okValues bool, presence, motion int) {
	s.mu.Lock()
	if s.debug && okValues && now.Sub(s.lastDebug) >= time.Second {
		s.lastDebug = now
		note := ""
		if !s.trusted {
			note = " (presence ignored)"
		}
		log.Printf("[Room] flags %d%d presence %6d motion %6d%s", b2i(flags&presenceFlag != 0),
			b2i(flags&motionFlag != 0), presence, motion, note)
	}
	moved := okFlags && flags&motionFlag != 0
	if moved {
		s.lastMotionFlag = now
		s.lastMotionAt = time.Now()
	}
	// A warm body leaving the view drives motion far negative, one coming in positive.
	if okValues && motion <= -s.leaveMotion {
		s.trusted = false
	} else if moved && motion >= s.comeInMotion {
		s.trusted = true
	}
	present := okFlags && flags&presenceFlag != 0 && s.trusted && now.Sub(s.lastMotionFlag) < s.presenceOnly
	changed, now2 := false, s.occupied
	if moved || present {
		s.lastSeen = now
		if !s.occupied {
			s.setOccupied(true, now)
			changed, now2 = true, true
		}
	} else if s.occupied {
		limit := s.emptyAfter
		if s.trusted {
			limit = s.stillEmpty
		}
		// Silence alone is someone sitting still as often as an empty room.
		if now.Sub(s.lastSeen) > limit {
			s.setOccupied(false, now)
			changed, now2 = true, false
		}
	}
	// HA gets what the sensor sees now, cleaned only of its quirks; the holding time is HA's (an
	// automation's "for:"), as the M5 (the owner's wish, 25.10.2026). occupied stays the assistant's own.
	if seen := moved || present; seen != s.detected {
		s.detected = seen
		s.reportPending = true
	}
	s.mu.Unlock()
	if changed && s.OnChange != nil {
		s.OnChange(now2)
	}
}

// setOccupied changes the state; s.mu is held.
func (s *Sensor) setOccupied(occupied bool, now time.Time) {
	s.occupied = occupied
	s.changedAt = time.Now()
	what := "empty"
	if occupied {
		what = "someone came in"
	}
	log.Printf("[Room] %s", what)
}

func b2i(b bool) int {
	if b {
		return 1
	}
	return 0
}

// maybeReport sends the state at once after a change (a failed report again after 30 s) and
// again every ROOM_REPORT_MS, because HA forgets the state on a restart.
func (s *Sensor) maybeReport(now time.Time) {
	s.mu.Lock()
	// a change at once but at most every ROOM_REPORT_MIN_MS (the motion flag pulses)
	minGap := time.Duration(prompts.Int("ROOM_REPORT_MIN_MS", 5000)) * time.Millisecond
	due := (s.reportPending && !now.Before(s.retryAt) && now.Sub(s.lastReport) >= minGap) || s.lastReport.IsZero() ||
		now.Sub(s.lastReport) > s.reportEvery
	detected, occupied, temperature := s.detected, s.occupied, s.temperature
	s.mu.Unlock()
	if !due {
		return
	}
	state := "off"
	if detected {
		state = "on"
	}
	attributes := map[string]any{"friendly_name": prompts.Name() + " Anwesenheit", "device_class": "occupancy",
		prompts.Slug() + "_occupied": occupied}
	if !math.IsNaN(temperature) {
		attributes["sensor_temperature"] = math.Round(temperature*10) / 10
	}
	status, body, err := s.ha.Rest("POST", "/api/states/"+Entity(), map[string]any{"state": state, "attributes": attributes})
	s.mu.Lock()
	s.lastReport = now
	s.retryAt = now.Add(reportRetry)
	if err == nil && (status == 200 || status == 201) {
		s.reportPending = false
	}
	s.mu.Unlock()
	if err != nil || (status != 200 && status != 201) {
		log.Printf("[Room] report to Home Assistant: %s", homeassistant.Failure(status, body, err))
	}
}

func clock(t time.Time) string {
	if t.IsZero() {
		return "unknown"
	}
	return t.Local().Format("02.01. 15:04")
}

func (s *Sensor) describe() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.found {
		return "Error: no presence sensor connected."
	}
	var text string
	if s.occupied {
		text = "Someone is in the room at the speaker (since " + clock(s.changedAt) + ")."
	} else {
		text = "Nobody is in the room at the speaker (empty since " + clock(s.changedAt) + ")."
	}
	text += " Last movement: " + clock(s.lastMotionAt) + "."
	if !math.IsNaN(s.temperature) {
		text += fmt.Sprintf(" Sensor temperature %.1f °C (inside the device, warmer than the room).", s.temperature)
	}
	text += fmt.Sprintf(" Home Assistant entity for automations: %s (on = someone came in, off = nobody there: %d s after someone walked out, else after %d min without any sign).",
		Entity(), int(s.emptyAfter/time.Second), int(s.stillEmpty/time.Minute))
	return text
}

// AddTools registers room_sensor.
func (s *Sensor) AddTools(r *tools.Registry) {
	r.Add(tools.Tool{
		Name: "room_sensor",
		Description: "The presence sensor at the speaker (infrared: also notices people sitting still): whether someone is " +
			"in the room right now, since when, and the last movement. Automations that react to it ('Licht an, " +
			"wenn jemand reinkommt und es dunkel ist', 'begrüße mich, wenn ich reinkomme') are " +
			"create_home_automation with a state trigger on " + Entity() + "; it changes as soon as the " +
			"sensor sees someone or nobody, so 'when nobody is there' needs 'for:' in the trigger (e.g. 10 minutes).",
		Parameters: `{"type": "object", "additionalProperties": false, "properties": {}, "required": []}`,
		Channels:   tools.Everywhere,
		Run:        func(map[string]any, tools.Context) string { return s.describe() },
	})
}
