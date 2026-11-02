package audio

import (
	"bufio"
	"fmt"
	"log"
	"math"
	"os"
	"os/exec"
	"strings"
	"sync/atomic"
	"time"
)

// The Echo Dot 2's Bluetooth speaker (#74): btspeaker (devices/echo/hardware/dot2/tools/btspeaker, BTstack on
// MediaTek's /dev/stpbt) streams the playback mix as A2DP to a better speaker nearby, an Alexa
// Echo paired once. The link stays up with silence in between: connecting takes seconds, too
// long for an answer. The playback loop keeps its pace on the DAC and sends each period to
// btspeaker while the stream runs, the DAC getting silence; when the link drops the Dot's own
// speaker plays again with the next period.

// dotBTLatency: what the Bluetooth path adds when the sink reports no delay of its own (SBC
// frames, the sink's buffer); to be measured.
const dotBTLatency = 250 * time.Millisecond

type dotBT struct {
	active  atomic.Bool
	delay   atomic.Int64 // the sink's reported delay (ns), 0 = none
	periods chan []byte
	volume  atomic.Int32            // the assistant's volume 0..100, -1 = not set yet
	control atomic.Pointer[os.File] // btspeaker's command pipe while it runs
}

// OutputGain: the gain to put on the samples for a volume's gain (level², the Dot's curve).
// While the Bluetooth speaker plays it is the flatter level (40 % = -8 dB instead of -16 dB):
// the speaker's own volume comes on top, and both with the Dot's curve made the assistant far too quiet
// there. The volume does not go to the speaker as AVRCP absolute volume: an Alexa Echo dropped
// the link ~15 s after it every time (20.10.2026).
func OutputGain(volumeGain float64) float64 {
	if dot != nil {
		if b := dot.bt.Load(); b != nil && b.active.Load() {
			return math.Sqrt(volumeGain)
		}
	}
	return volumeGain
}

// SetDotBluetoothVolume hands the assistant's volume (0..100) to the Bluetooth speaker.
func SetDotBluetoothVolume(level int) {
	if dot == nil {
		return
	}
	if b := dot.bt.Load(); b != nil {
		b.volume.Store(int32(level))
		b.sendVolume()
	}
}

func (b *dotBT) sendVolume() {
	f, v := b.control.Load(), b.volume.Load()
	if f == nil || v < 0 {
		return
	}
	fmt.Fprintf(f, "VOLUME %d\n", v)
}

// StartDotBluetooth keeps `bin -d keyDir -n name play addr` running and plays through it once its
// stream is up; name is what the speaker lists. Without StartDot first it does nothing.
func StartDotBluetooth(bin, keyDir, addr, name string) {
	if dot == nil {
		return
	}
	b := &dotBT{periods: make(chan []byte, 8)}
	b.volume.Store(-1)
	dot.bt.Store(b)
	go b.keep(bin, "-d", keyDir, "-n", name, "play", addr)
}

func (b *dotBT) keep(bin string, args ...string) {
	wait := 5 * time.Second
	for {
		started := time.Now()
		err := b.run(bin, args...)
		b.active.Store(false)
		if time.Since(started) > time.Minute {
			wait = 5 * time.Second
		} else {
			wait = min(2*wait, 2*time.Minute) // the Echo is off or out of reach
		}
		log.Printf("[Bluetooth] speaker link ended (%v); again in %v", err, wait)
		time.Sleep(wait)
	}
}

func (b *dotBT) run(bin string, args ...string) error {
	cmd := exec.Command(bin, args...)
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return err
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return err
	}
	cmd.Stderr = cmd.Stdout
	// fd 3: commands to btspeaker (VOLUME n)
	controlR, controlW, err := os.Pipe()
	if err != nil {
		return err
	}
	cmd.ExtraFiles = []*os.File{controlR}
	if err := cmd.Start(); err != nil {
		controlR.Close()
		controlW.Close()
		return err
	}
	controlR.Close()
	b.control.Store(controlW)
	defer func() {
		b.control.Store(nil)
		controlW.Close()
	}()
	done := make(chan struct{})
	go func() {
		for {
			select {
			case p := <-b.periods:
				if _, err := stdin.Write(p); err != nil {
					return
				}
			case <-done:
				return
			}
		}
	}()
	lines := bufio.NewScanner(stdout)
	for lines.Scan() {
		line := lines.Text()
		switch {
		case strings.HasPrefix(line, "STREAMING"):
			rate := 0
			fmt.Sscanf(line, "STREAMING rate=%d", &rate)
			if rate != dotOutRate {
				log.Printf("[Bluetooth] the speaker took %d Hz, not %d: the Dot keeps playing", rate, dotOutRate)
				continue
			}
			b.active.Store(true)
			b.sendVolume()
			log.Printf("[Bluetooth] playing through the speaker (%s)", line)
		case strings.HasPrefix(line, "DELAY"):
			ms := 0
			fmt.Sscanf(line, "DELAY ms=%d", &ms)
			b.delay.Store(int64(time.Duration(ms) * time.Millisecond))
			log.Printf("[Bluetooth] the speaker reports %d ms delay", ms)
		case strings.HasPrefix(line, "DISCONNECTED"), strings.HasPrefix(line, "SUSPENDED"):
			b.active.Store(false)
			log.Printf("[Bluetooth] %s: the Dot plays again", line)
		default:
			log.Printf("[Bluetooth] %s", line)
		}
	}
	b.active.Store(false)
	close(done)
	stdin.Close()
	return cmd.Wait()
}

// send hands one period to the speaker; false when Bluetooth does not play it. A full queue
// drops the period rather than holding up the DAC.
func (b *dotBT) send(p []byte) bool {
	if !b.active.Load() {
		return false
	}
	select {
	case b.periods <- append([]byte(nil), p...):
	default:
	}
	return true
}

// dotExtraLatency: what Bluetooth adds to the speaker's latency right now (0 without it).
func dotExtraLatency() time.Duration {
	if dot == nil {
		return 0
	}
	b := dot.bt.Load()
	if b == nil || !b.active.Load() {
		return 0
	}
	if d := time.Duration(b.delay.Load()); d > 0 {
		return d
	}
	return dotBTLatency
}
