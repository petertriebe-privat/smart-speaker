// Package acknowledge: The assistant answers the action button right away with a short clip in its own
// voice ("Ja, Sir?"), while the connection to GPT Live is still being built and GPT Live would need
// a few seconds more for a greeting. As devices/atoms3r/src/voice/acknowledge.cpp: the clips are made
// once with OpenAI's TTS (the voice of the live session), kept in the state directory and loaded
// into RAM. Without them the button only beeps.
package acknowledge

import (
	"context"
	"encoding/binary"
	"fmt"
	"log"
	"math/rand"
	"os"
	"path/filepath"
	"sync"
	"time"

	"echo-assistant/internal/audio"
	"echo-assistant/internal/openai"
)

// Changing a phrase: raise the version, the old files are then made again. The last one is not
// an acknowledgement: The assistant asks again when nobody understood the user (Repeat).
var phrases = []string{"Ja, Sir?", "Ich höre, Sir.", "Ja?", "Wie bitte, Sir?"}

const acks = 3 // the first three: Pick

const (
	version = 1
	silence = 400 // TTS starts and ends with silence: trimmed away
	rate    = audio.SpeakerRate
)

// Clips holds the clips; Play is safe from any goroutine.
type Clips struct {
	dir string

	mu    sync.Mutex
	clips [][]int16
}

func New(stateDir string) *Clips { return &Clips{dir: filepath.Join(stateDir, "voice")} }

func (c *Clips) path(i int) string {
	return filepath.Join(c.dir, fmt.Sprintf("ack%d_%d.pcm", version, i))
}

// Ready: all clips are loaded.
func (c *Clips) Ready() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.clips) == len(phrases)
}

// Pick: a random clip, nil while they are not ready.
func (c *Clips) Pick() []int16 {
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(c.clips) != len(phrases) {
		return nil
	}
	i := rand.Intn(acks)
	log.Printf("[Ack] %s", phrases[i])
	return c.clips[i]
}

// Repeat: "Wie bitte, Sir?", nil while the clips are not ready.
func (c *Clips) Repeat() []int16 {
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(c.clips) != len(phrases) {
		return nil
	}
	return c.clips[len(phrases)-1]
}

// Run loads the clips and makes missing ones (once; retried a minute later on failure).
func (c *Clips) Run(ctx context.Context) {
	for {
		if c.prepare(ctx) {
			return
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(time.Minute):
		}
	}
}

func (c *Clips) prepare(ctx context.Context) bool {
	var clips [][]int16
	for i, phrase := range phrases {
		pcm, err := load(c.path(i))
		if err != nil {
			if pcm, err = c.make(ctx, i); err != nil {
				log.Printf("[Ack] \"%s\": %v", phrase, err)
				return false
			}
			log.Printf("[Ack] \"%s\" made (%.2f s)", phrase, float64(len(pcm))/rate)
		}
		clips = append(clips, pcm)
	}
	c.mu.Lock()
	c.clips = clips
	c.mu.Unlock()
	return true
}

func load(path string) ([]int16, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	if len(data) < 2 {
		return nil, fmt.Errorf("empty")
	}
	pcm := make([]int16, len(data)/2)
	for i := range pcm {
		pcm[i] = int16(binary.LittleEndian.Uint16(data[2*i:]))
	}
	return pcm, nil
}

func (c *Clips) make(ctx context.Context, i int) ([]int16, error) {
	tctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	data, err := openai.Speech(tctx, phrases[i], "pcm")
	if err != nil {
		return nil, err
	}
	pcm := make([]int16, len(data)/2)
	for j := range pcm {
		pcm[j] = int16(binary.LittleEndian.Uint16(data[2*j:]))
	}
	pcm = trim(pcm)
	if len(pcm) == 0 {
		return nil, fmt.Errorf("no speech in the clip")
	}
	out := make([]byte, 2*len(pcm))
	for j, v := range pcm {
		binary.LittleEndian.PutUint16(out[2*j:], uint16(v))
	}
	if err := os.MkdirAll(c.dir, 0o755); err != nil {
		return nil, err
	}
	if err := os.WriteFile(c.path(i), out, 0o644); err != nil {
		return nil, err
	}
	return pcm, nil
}

// trim cuts the silence before and after the speech, keeps 30 ms around it.
func trim(pcm []int16) []int16 {
	loud := func(v int16) bool { return v >= silence || v <= -silence }
	first, last := 0, len(pcm)
	for first < last && !loud(pcm[first]) {
		first++
	}
	for last > first && !loud(pcm[last-1]) {
		last--
	}
	margin := rate * 30 / 1000
	return pcm[max(0, first-margin):min(len(pcm), last+margin)]
}
