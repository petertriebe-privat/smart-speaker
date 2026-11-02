// Package music: the Echo as a Spotify Connect speaker. librespot (a separate static binary, see
// devices/echo/docs/16-assistant.md) runs as a child process with its pipe backend; its 44.1 kHz stereo
// PCM goes through a playback stream of its own (pacat on the Echo), next to the assistant's voice. The assistant sets the level
// itself: the device volume (the ring) times the duck factor while he talks, ramped so it does
// not click. Spotify's own volume (app slider, Web API) is librespot's software mixer, before
// that. The assistant's Spotify tools find the speaker under its name like any Connect device.
//
// Login: librespot announces itself by zeroconf; picking it once in a Spotify app on the home
// network logs it in, and the credentials stay in the cache directory.
package music

import (
	"bufio"
	"context"
	"encoding/binary"
	"io"
	"log"
	"math"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"time"

	"echo-assistant/internal/audio"
)

const (
	rate     = 44100
	channels = 2
	chunk    = rate * channels * 2 / 50 // 20 ms of S16LE stereo
)

// Player runs librespot and plays what it streams.
type Player struct {
	Bin      string // librespot binary
	Name     string // Spotify Connect name
	CacheDir string // credentials (and nothing else: no audio cache)
	Env      []string
	// OnEvent: librespot's --onevent command line ("<jarvis> event"): the volume set in a
	// Spotify app becomes the device volume. librespot itself plays at full level (fixed).
	OnEvent string

	mu       sync.Mutex
	volume   float64 // device volume 0..1 (already squared like the voice)
	duck     float64 // factor while the assistant talks (1 = not ducked)
	ducked   bool
	stop     context.CancelFunc // ends the running librespot
	restart  bool               // Restart asked for it: start again at once
	hushed   time.Time          // silent until then (HushFor)
	current  float64            // the gain applied now, ramping towards the target
	lastData time.Time
	spec     *audio.Spectrum // the ring as a music display
}

func New(bin, name, cacheDir string, env []string) *Player {
	return &Player{Bin: bin, Name: name, CacheDir: cacheDir, Env: env, volume: 0.16, duck: 0.2, current: 0.16, spec: audio.NewSpectrum(rate, audio.MusicBands)}
}

// SetVolume: the device volume 0..100 (the ring), same curve as the voice.
func (p *Player) SetVolume(level int) {
	f := float64(max(0, min(100, level))) / 100
	p.mu.Lock()
	p.volume = f * f
	p.mu.Unlock()
}

// SetDuckLevel: the music's level while the assistant talks, in percent of the normal level.
func (p *Player) SetDuckLevel(percent int) {
	p.mu.Lock()
	p.duck = float64(max(0, min(100, percent))) / 100
	p.mu.Unlock()
}

// Duck: true while a conversation runs at the speaker.
func (p *Player) Duck(on bool) {
	p.mu.Lock()
	changed := p.ducked != on
	p.ducked = on
	playing := time.Since(p.lastData) < time.Second
	p.mu.Unlock()
	if changed && playing {
		log.Printf("[Music] ducked %v", on)
	}
}

// HushFor: the music silent for d (0 ends it), as Alexa does while it hears a command: ducked to
// 20 % the DSP's echo cancellation still took the user's "Pause" with the music (18.10.2026, #59).
func (p *Player) HushFor(d time.Duration) {
	p.mu.Lock()
	p.hushed = time.Now().Add(d)
	p.mu.Unlock()
}

// monoOf: one block of S16LE stereo as mono samples, for the spectrum.
func monoOf(buf []byte) []int16 {
	frames := len(buf) / 4
	out := make([]int16, frames)
	for i := 0; i < frames; i++ {
		l := int32(int16(binary.LittleEndian.Uint16(buf[i*4:])))
		r := int32(int16(binary.LittleEndian.Uint16(buf[i*4+2:])))
		out[i] = int16((l + r) / 2)
	}
	return out
}

// Playing: librespot delivered audio within the last second.
func (p *Player) Playing() bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	return time.Since(p.lastData) < time.Second
}

// Spectrum: the music's bands 0..1, bass first (all 0 while nothing plays): for the ring.
func (p *Player) Spectrum() [audio.SpectrumBands]float64 {
	if !p.Playing() {
		return [audio.SpectrumBands]float64{}
	}
	return p.spec.Levels()
}

func (p *Player) target() float64 {
	if time.Now().Before(p.hushed) {
		return 0
	}
	if p.ducked {
		return audio.OutputGain(p.volume) * p.duck
	}
	return audio.OutputGain(p.volume)
}

// Restart starts librespot anew: Spotify stops listing it once playback moved to another
// speaker ("device became inactive"), and nothing can be moved back to a speaker it does not
// list (18.10.2026). A new start announces it again.
func (p *Player) Restart() {
	p.mu.Lock()
	stop := p.stop
	p.restart = stop != nil
	p.mu.Unlock()
	if stop != nil {
		log.Print("[Music] restarting librespot")
		stop()
	}
}

// Run keeps librespot running until ctx ends (restarts after a crash, with a growing pause).
func (p *Player) Run(ctx context.Context) {
	if _, err := os.Stat(p.Bin); err != nil {
		log.Printf("[Music] %s missing: no Spotify Connect speaker", p.Bin)
		return
	}
	os.MkdirAll(p.CacheDir, 0o700)
	killStale(p.Bin)
	pause := 5 * time.Second
	for ctx.Err() == nil {
		started := time.Now()
		run, stop := context.WithCancel(ctx)
		p.mu.Lock()
		p.stop = stop
		p.mu.Unlock()
		err := p.runOnce(run)
		stop()
		log.Printf("[Music] librespot ended: %v", err)
		p.mu.Lock()
		wanted := p.restart
		p.restart = false
		p.mu.Unlock()
		if wanted {
			continue // Restart: at once, it is no crash
		}
		if time.Since(started) > 5*time.Minute {
			pause = 5 * time.Second
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(pause):
		}
		pause = min(pause*2, 5*time.Minute)
	}
}

func (p *Player) runOnce(ctx context.Context) error {
	p.mu.Lock()
	level := int(math.Round(math.Sqrt(p.volume) * 100))
	p.mu.Unlock()
	args := []string{
		"--name", p.Name,
		"--device-type", "speaker",
		"--backend", "pipe",
		// The PCM gets a pipe of its own (fd 3), not stdout: programs librespot starts
		// (--onevent) inherit its stdout, and a single stray byte in the stream shifts every
		// sample after it into full-scale noise (16.10.2026).
		"--device", "/proc/self/fd/3",
		"--format", "S16",
		"--bitrate", "160",
		"--cache", p.CacheDir,
		"--disable-audio-cache",
		"--initial-volume", strconv.Itoa(level),
		"--volume-ctrl", "fixed", // the level is the assistant's (one volume for voice and music)
		"--autoplay", "on", // after an album or playlist similar music, as in the Spotify app
		// No loudness normalisation: it turned music well below the assistant's voice at the same
		// volume (16.10.2026).
	}
	if p.OnEvent != "" {
		args = append(args, "--onevent", p.OnEvent)
	}
	cmd := exec.CommandContext(ctx, p.Bin, args...)
	bindToParent(cmd)
	out, audioW, err := os.Pipe()
	if err != nil {
		return err
	}
	defer out.Close()
	cmd.ExtraFiles = []*os.File{audioW} // fd 3
	stderr, err := cmd.StderrPipe()
	if err != nil {
		audioW.Close()
		return err
	}
	cmd.Stdout = nil // /dev/null
	err = cmd.Start()
	audioW.Close() // the child has its own copy; EOF once librespot ends
	if err != nil {
		return err
	}
	go logLines(stderr)
	defer cmd.Wait()
	return p.pump(out)
}

// logLines passes librespot's log on, without the per-packet noise.
func logLines(r io.Reader) {
	sc := bufio.NewScanner(r)
	for sc.Scan() {
		line := sc.Text()
		if strings.Contains(line, " DEBUG ") || strings.Contains(line, " TRACE ") {
			continue
		}
		// mDNS noise from other devices: three of four lines of a day's log, which the 2 MiB in
		// RAM then lost (31.10.2026)
		if strings.Contains(line, "libmdns") && (strings.Contains(line, "dropping truncated packet") ||
			strings.Contains(line, "Failed to register IPv6 receiver")) {
			continue
		}
		log.Printf("[Music] %s", line)
	}
}

// pump copies librespot's PCM to a playback stream (audio.NewStream), opened on the first audio and closed after a
// minute of silence (paused or stopped), so an idle speaker costs nothing.
func (p *Player) pump(r io.Reader) error {
	var sink io.WriteCloser
	closeSink := func() {
		if sink != nil {
			sink.Close()
			sink = nil
		}
	}
	defer closeSink()
	data := make(chan []byte, 8)
	errs := make(chan error, 1)
	go func() {
		for {
			buf := make([]byte, chunk)
			n, err := io.ReadFull(r, buf)
			if n > 0 {
				data <- buf[:n&^3]
			}
			if err != nil {
				errs <- err
				close(data)
				return
			}
		}
	}()
	idle := time.NewTimer(time.Minute)
	defer idle.Stop()
	// After a failed stream the next one waits: PulseAudio refused new clients ("Connection
	// terminated") and a new pacat per 100 ms chunk ran for minutes until the Echo hung
	// (28.10.2026). The music meanwhile is dropped; a growing pause, logged once per try.
	var retryAt time.Time
	retry := time.Second
	for {
		select {
		case buf, ok := <-data:
			if !ok {
				return <-errs
			}
			p.mu.Lock()
			p.lastData = time.Now()
			p.mu.Unlock()
			if sink == nil && time.Now().Before(retryAt) {
				continue
			}
			if sink == nil {
				var err error
				if sink, err = audio.NewStream(44100, 2, 300*time.Millisecond, "music"); err != nil {
					return err
				}
				log.Print("[Music] playing")
			}
			p.spec.Feed(monoOf(buf)) // before the volume: the display does not depend on it
			p.scale(buf)
			if _, err := sink.Write(buf); err != nil {
				log.Printf("[Music] stream: %v (next try in %v)", err, retry)
				closeSink()
				retryAt = time.Now().Add(retry)
				retry = min(retry*2, 30*time.Second)
				continue
			}
			retry = time.Second
			idle.Reset(time.Minute)
		case <-idle.C:
			if sink != nil {
				log.Print("[Music] idle, stream closed")
				closeSink()
			}
			idle.Reset(time.Minute)
		}
	}
}

// scale applies the gain in place. Each 20 ms block moves 15 % of the way to the target (95 %
// after ~400 ms), linearly within the block, so ducking and the ring do not click.
func (p *Player) scale(buf []byte) {
	p.mu.Lock()
	target, g := p.target(), p.current
	p.mu.Unlock()
	// down fast (The assistant speaks), up slowly: after a long conversation at 20 % the music
	// coming back within 0.4 s seemed louder than before (16.10.2026); ~2 s now
	rate := 0.15
	if target > g {
		rate = 0.03
	}
	end := g + (target-g)*rate
	if math.Abs(target-end) < 1e-3 {
		end = target
	}
	frames := len(buf) / 4
	step := 0.0
	if frames > 0 {
		step = (end - g) / float64(frames)
	}
	// Integer gain (Q16) on the samples: float per sample cost ~15 % CPU on the Cortex-A8.
	gi := int64(g * 65536)
	stepi := int64(step * 65536)
	for i := 0; i < frames; i++ {
		gi += stepi
		for at := i * 4; at < i*4+4; at += 2 {
			v := int64(int16(binary.LittleEndian.Uint16(buf[at:]))) * gi >> 16
			binary.LittleEndian.PutUint16(buf[at:], uint16(int16(max(-32768, min(32767, v)))))
		}
	}
	p.mu.Lock()
	p.current = end
	p.mu.Unlock()
}
