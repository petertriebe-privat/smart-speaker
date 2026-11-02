// Package leds drives the light ring (the Dot's: dot.go). The Echo's: 12 RGB LEDs on four LP5523 chips (three LEDs, nine
// channels each), written directly over I2C.
package leds

import (
	"log"
	"math"
	"sync"
	"time"

	"echo-assistant/internal/board"
)

const (
	Count = 12
	fps   = 30
	pwm   = 0x16 // first PWM register (D1)
)

// The ring as seen from above with the mute button on the left (9 o'clock): position 0 is
// 12 o'clock, clockwise. Each chip drives three neighbouring LEDs; per LED k (0..2) green is
// channel 2k, blue 2k+1 and red 6+k. Found 16.10.2026 by lighting single channels and photos;
// 0x34 is wired out of order.
var (
	chips = [4]byte{0x32, 0x33, 0x34, 0x35}
	hours = [4][3]int{{3, 2, 1}, {9, 8, 7}, {5, 6, 4}, {12, 11, 10}} // o'clock of each chip's LED k
)

type RGB struct{ R, G, B uint8 }

var (
	Off    = RGB{}
	Blue   = RGB{0, 40, 255}
	Cyan   = RGB{0, 180, 255}
	Teal   = RGB{0, 210, 230} // the turquoise of Amazon's ring
	Purple = RGB{150, 0, 255}
	Green  = RGB{0, 255, 60}
	Yellow = RGB{255, 170, 0}
	Red    = RGB{255, 0, 0}
	White  = RGB{255, 255, 255}
)

func (c RGB) Scale(f float64) RGB {
	f = math.Max(0, math.Min(1, f))
	return RGB{uint8(float64(c.R) * f), uint8(float64(c.G) * f), uint8(float64(c.B) * f)}
}

// Pattern gives the ring's colours (index 0 = 12 o'clock, clockwise) at time t since it started.
type Pattern func(t time.Duration) [Count]RGB

type Ring struct {
	bus     *bus
	frame   *dotFrame // the Dot's IS31FL3236 instead of the LP5523 chips
	mu      sync.Mutex
	pattern Pattern
	since   time.Time
	overlay Pattern // on top until overEnd (volume level, calibration)
	overAt  time.Time
	overEnd time.Time
	failed  bool // an I2C error was logged
	offset  int
	level   float64 // the run goroutine's copy of want
	want    float64 // SetLevel's, under mu
	last    [4][9]byte
	written bool
}

// Open: offset rotates the ring (positions), level 0..255 caps the brightness.
func Open(offset, level int) (*Ring, error) {
	if board.Dot2 {
		f, err := openDotFrame()
		if err != nil {
			return nil, err
		}
		r := &Ring{frame: f, offset: offset, want: float64(level) / 255, pattern: Solid(Off), since: time.Now()}
		go r.run()
		return r, nil
	}
	b, err := openBus()
	if err != nil {
		return nil, err
	}
	r := &Ring{bus: b, offset: offset, want: float64(level) / 255, pattern: Solid(Off), since: time.Now()}
	// Stop the boot animation's engines, chip on: the ring is ours.
	for _, addr := range chips {
		b.write(addr, []byte{0x01, 0x00}) // OP_MODE: all engines disabled
		b.write(addr, []byte{0x00, 0x40}) // ENABLE: chip on, engines hold
	}
	go r.run()
	return r, nil
}

// SetLevel caps the brightness anew (0..255; Home Assistant's ring light and night mode, #65).
func (r *Ring) SetLevel(level int) {
	r.mu.Lock()
	r.want = float64(max(0, min(255, level))) / 255
	r.mu.Unlock()
}

func (r *Ring) Set(p Pattern) {
	r.mu.Lock()
	r.pattern, r.since = p, time.Now()
	r.mu.Unlock()
}

// Show puts p on top of the current pattern for d (volume display).
func (r *Ring) Show(p Pattern, d time.Duration) {
	r.mu.Lock()
	if r.overlay == nil || time.Now().After(r.overEnd) {
		r.overAt = time.Now()
	}
	r.overlay, r.overEnd = p, time.Now().Add(d)
	r.mu.Unlock()
}

func (r *Ring) run() {
	ticker := time.NewTicker(time.Second / fps)
	for range ticker.C {
		r.mu.Lock()
		p, t := r.pattern, time.Since(r.since)
		if r.overlay != nil && time.Now().Before(r.overEnd) {
			p, t = r.overlay, time.Since(r.overAt)
		}
		r.level = r.want
		r.mu.Unlock()
		r.write(p(t))
	}
}

func (r *Ring) write(colors [Count]RGB) {
	if r.frame != nil {
		if err := r.frame.write(colors, r.offset, r.level); err != nil && !r.failed {
			r.failed = true
			log.Printf("[LED] %v", err)
		}
		return
	}
	var channels [4][9]byte
	for chip := range chips {
		for k := 0; k < 3; k++ {
			pos := ((hours[chip][k]+r.offset)%Count + Count) % Count
			c := colors[pos]
			channels[chip][2*k] = byte(float64(c.G) * r.level)
			channels[chip][2*k+1] = byte(float64(c.B) * r.level)
			channels[chip][6+k] = byte(float64(c.R) * r.level)
		}
	}
	// One register per write: multi-byte writes with auto-increment lost some of the bytes
	// (16.10.2026). Only changed channels; a failed one is tried again in the next frame.
	for chip, addr := range chips {
		for ch, v := range channels[chip] {
			if r.written && v == r.last[chip][ch] {
				continue
			}
			if err := r.bus.write(addr, []byte{pwm + byte(ch), v}); err != nil {
				if !r.failed {
					r.failed = true
					log.Printf("[LED] chip %#x: %v", addr, err)
				}
				continue
			}
			r.last[chip][ch] = v
		}
	}
	r.written = true
}

// --- Patterns ------------------------------------------------------------------

func Solid(c RGB) Pattern {
	return func(time.Duration) (out [Count]RGB) {
		for i := range out {
			out[i] = c
		}
		return
	}
}

// Spinner: a bright segment running round clockwise (connecting, thinking).
func Spinner(c, background RGB, period time.Duration) Pattern {
	return func(t time.Duration) (out [Count]RGB) {
		head := float64(Count) * math.Mod(t.Seconds()/period.Seconds(), 1)
		for i := range out {
			d := math.Mod(head-float64(i)+Count, Count) // distance behind the head
			out[i] = mix(background, c, math.Max(0, 1-d/4))
		}
		return
	}
}

// Pulse: the whole ring breathing (speaking).
func Pulse(c RGB, low float64, period time.Duration) Pattern {
	return func(t time.Duration) (out [Count]RGB) {
		f := low + (1-low)*(0.5-0.5*math.Cos(2*math.Pi*t.Seconds()/period.Seconds()))
		for i := range out {
			out[i] = c.Scale(f)
		}
		return
	}
}

// Live: listening ring whose brightness follows a level 0..1 (the voice).
func Live(c RGB, level func() float64) Pattern {
	return func(time.Duration) (out [Count]RGB) {
		f := 0.35 + 0.65*math.Max(0, math.Min(1, level()))
		for i := range out {
			out[i] = c.Scale(f)
		}
		return
	}
}

// Voice: the ring follows a level 0..1 (The assistant's voice): quiet c, loud towards white.
func Voice(c RGB, level func() float64) Pattern {
	smooth := 0.0
	return func(time.Duration) (out [Count]RGB) {
		l := math.Max(0, math.Min(1, level()*1.5))
		smooth += (l - smooth) * 0.35 // a little smoothing at 30 frames/s
		col := mix(c, White, smooth*0.8).Scale(0.45 + 0.55*smooth)
		for i := range out {
			out[i] = col
		}
		return
	}
}

// Spectrum: music as a display, mirrored: bass at 6 o'clock, treble at 12 o'clock, both halves
// alike. bands (0..1, bass first) gives one level per distance from 6 o'clock (7 positions for
// 12 LEDs); colours run from purple (bass) over turquoise to light blue (treble).
func Spectrum(bands func() [7]float64) Pattern {
	palette := [7]RGB{Purple, mix(Purple, Teal, 0.5), Teal, Teal, mix(Teal, Cyan, 0.5), Cyan, mix(Cyan, White, 0.5)}
	var held [Count]RGB
	frame := 0
	return func(time.Duration) (out [Count]RGB) {
		// 15 frames per second and brightness in 12 steps: every changed channel is an I2C
		// write, and at 30 frames the ring alone cost ~15 % CPU (16.10.2026)
		if frame++; frame%2 == 0 {
			return held
		}
		levels := bands()
		for b := range levels {
			levels[b] = math.Round(levels[b]*12) / 12
		}
		for i := range out {
			d := i - Count/2 // 6 o'clock = index 6
			if d < 0 {
				d = -d
			}
			b := Count/2 - d // 0 at 12 o'clock … 6 at 6 o'clock
			band := 6 - b    // bass (0) at the bottom
			l := levels[band]
			out[i] = palette[band].Scale(l * l) // squared: quiet parts stay dark
		}
		held = out
		return
	}
}

// Level: the ring filled clockwise from 12 o'clock to fraction (volume), the rest off.
func Level(c RGB, fraction float64) Pattern {
	return func(time.Duration) (out [Count]RGB) {
		fill := math.Max(0, math.Min(1, fraction)) * Count
		for i := range out {
			out[i] = c.Scale(math.Max(0, math.Min(1, fill-float64(i))))
		}
		return
	}
}

// Flash: blinking (problems).
func Flash(c RGB, period time.Duration) Pattern {
	return func(t time.Duration) (out [Count]RGB) {
		if math.Mod(t.Seconds(), period.Seconds()) < period.Seconds()/2 {
			for i := range out {
				out[i] = c
			}
		}
		return
	}
}

// Single lights one position (calibration).
func Single(index int, c RGB) Pattern {
	return func(time.Duration) (out [Count]RGB) {
		out[((index%Count)+Count)%Count] = c
		return
	}
}

// Chase: one LED running round clockwise, one position per step (calibration).
func Chase(c RGB, step time.Duration) Pattern {
	return func(t time.Duration) (out [Count]RGB) {
		out[int(t/step)%Count] = c
		return
	}
}

func mix(a, b RGB, f float64) RGB {
	l := func(x, y uint8) uint8 { return uint8(float64(x)*(1-f) + float64(y)*f) }
	return RGB{l(a.R, b.R), l(a.G, b.G), l(a.B, b.B)}
}
