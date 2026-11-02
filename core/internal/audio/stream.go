package audio

import (
	"fmt"
	"io"
	"os/exec"
	"strconv"
	"time"
)

// NewStream opens a playback stream of s16le PCM next to the others: on the Echo a pacat client
// of the PulseAudio server, on the Dot a source of the playback mix (StartDot). Write blocks
// while its buffer is full; Close stops it at once and drops what is still queued.
func NewStream(rate, channels int, latency time.Duration, name string) (io.WriteCloser, error) {
	if OnTV() {
		return tvStream(rate, channels, name)
	}
	if dot != nil {
		// As much queued as on the Echo before Write blocks: pacat's buffer and a pipe of 64 KiB
		// (voice 1.6 s, music 0.7 s: the music's volume and ducking act that much later).
		pipe := time.Duration(65536/(2*channels)) * time.Second / time.Duration(rate)
		return dot.newStream(rate, channels, latency, latency+pipe), nil
	}
	cmd := exec.Command("/usr/bin/pacat", "--playback", "--raw", "--format=s16le",
		"--rate="+strconv.Itoa(rate), "--channels="+strconv.Itoa(channels),
		"--latency-msec="+strconv.Itoa(int(latency/time.Millisecond)),
		"--client-name=assistant", "--stream-name="+name)
	cmd.Env = PulseEnv()
	pipe, err := cmd.StdinPipe()
	if err != nil {
		return nil, err
	}
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("pacat: %w", err)
	}
	return &pacat{cmd: cmd, pipe: pipe}, nil
}

type pacat struct {
	cmd  *exec.Cmd
	pipe io.WriteCloser
}

func (p *pacat) Write(b []byte) (int, error) { return p.pipe.Write(b) }

// Close kills pacat: closing its input alone would play the rest of its buffer.
func (p *pacat) Close() error {
	p.pipe.Close()
	p.cmd.Process.Kill()
	return p.cmd.Wait()
}
