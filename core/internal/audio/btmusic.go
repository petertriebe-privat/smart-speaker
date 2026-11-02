package audio

import (
	"bufio"
	"context"
	"echo-assistant/internal/prompts"
	"log"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"time"
)

// BTMusic: a phone's music over Bluetooth (A2DP) follows the assistant's volume, as its own music does.
// PulseAudio plays it itself (module-bluetooth-policy loads a loopback from the phone's source),
// so nothing of the assistant touched it: 22.10.2026 it played at 100 % whatever the ring said, and
// Amazon's audiod adds a gain of 1.18 for the role Music-BT on top ("Stream gain Music-BT ...
// out 77332"): it clipped, and even the phone's lowest volume was loud.
//
// It listens to PulseAudio's events (pactl subscribe, no polling) and sets every loopback stream
// to the level: the ring's level squared (the music's curve), divided by that gain, times the duck
// factor while the assistant talks. The phone's own slider still scales what it sends.
type BTMusic struct {
	mu      sync.Mutex
	level   float64 // 0..1, already squared
	ducked  bool
	duck    float64     // factor while the assistant talks
	applied map[int]int // sink input -> percent set
	kick    chan struct{}
}

// btGain: what audiod multiplies the role Music-BT with.
const btGain = 1.18

func NewBTMusic(level, duckPercent int) *BTMusic {
	b := &BTMusic{applied: map[int]int{}, kick: make(chan struct{}, 1)}
	b.SetVolume(level)
	b.duck = float64(max(0, min(100, duckPercent))) / 100
	return b
}

// SetVolume: the ring's level 0..100.
func (b *BTMusic) SetVolume(level int) {
	f := float64(max(0, min(100, level))) / 100
	b.mu.Lock()
	b.level = f * f
	b.mu.Unlock()
	b.poke()
}

// Duck: lower while the assistant talks, back after.
func (b *BTMusic) Duck(on bool) {
	b.mu.Lock()
	changed := b.ducked != on
	b.ducked = on
	b.mu.Unlock()
	if changed {
		b.poke()
	}
}

func (b *BTMusic) poke() {
	select {
	case b.kick <- struct{}{}:
	default:
	}
}

// percent: the stream volume for the current level.
func (b *BTMusic) percent() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	v := b.level / btGain
	if b.ducked {
		v *= b.duck
	}
	return int(v*100 + 0.5)
}

// Run: until ctx ends. Restarts pactl subscribe when it dies.
func (b *BTMusic) Run(ctx context.Context) {
	events := make(chan struct{}, 1)
	go func() {
		for ctx.Err() == nil {
			b.subscribe(ctx, events)
			select {
			case <-ctx.Done():
			case <-time.After(5 * time.Second):
			}
		}
	}()
	b.apply(true)
	for {
		select {
		case <-ctx.Done():
			return
		case <-events:
			b.apply(false)
		case <-b.kick:
			b.apply(true)
		}
	}
}

// subscribe: one pactl subscribe; a new or changed sink input pokes apply.
func (b *BTMusic) subscribe(ctx context.Context, events chan<- struct{}) {
	cmd := exec.CommandContext(ctx, "/usr/bin/pactl", "subscribe")
	cmd.Env = PulseEnv()
	out, err := cmd.StdoutPipe()
	if err != nil || cmd.Start() != nil {
		return
	}
	scan := bufio.NewScanner(out)
	for scan.Scan() {
		if line := scan.Text(); strings.Contains(line, "'new' on sink-input") {
			select {
			case events <- struct{}{}:
			default:
			}
		}
	}
	cmd.Wait()
}

// apply sets every Bluetooth loopback stream to the level; all = also those set before (the
// level changed), else only new ones.
func (b *BTMusic) apply(all bool) {
	cmdEnv := PulseEnv()
	query := exec.Command("/usr/bin/pactl", "list", "short", "sink-inputs")
	query.Env = cmdEnv
	list, err := query.Output()
	if err != nil {
		return
	}
	want := b.percent()
	seen := map[int]bool{}
	for _, line := range strings.Split(string(list), "\n") {
		f := strings.Fields(line)
		if len(f) < 4 || !strings.Contains(f[3], "loopback") {
			continue
		}
		id, err := strconv.Atoi(f[0])
		if err != nil {
			continue
		}
		seen[id] = true
		b.mu.Lock()
		done, known := b.applied[id]
		b.mu.Unlock()
		if known && (!all || done == want) {
			continue
		}
		set := exec.Command("/usr/bin/pactl", "set-sink-input-volume", f[0], strconv.Itoa(want)+"%")
		set.Env = cmdEnv
		if err := set.Run(); err != nil {
			log.Printf("[Bluetooth] music volume: %v", err)
			continue
		}
		b.mu.Lock()
		b.applied[id] = want
		b.mu.Unlock()
		if !known {
			log.Printf("[Bluetooth] phone music at %d %% ("+prompts.Name()+"' volume)", want)
		}
	}
	b.mu.Lock()
	for id := range b.applied {
		if !seen[id] {
			delete(b.applied, id)
		}
	}
	b.mu.Unlock()
}
