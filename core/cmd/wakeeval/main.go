// wakeeval runs the Echo's wake word detector (internal/wakeword, bit for bit the device's) over
// WAV files on the PC: which signal suits the wake word, and later old model against new (#61).
//
//	go run ./cmd/wakeeval [-model m.tflite] [-cutoff 0.45] [-gain 1] file.wav ...
//
// Every channel of a file is a track of its own; files with several channels get a "mix" track
// too (the channels averaged). Per track: the highest probability, the detections at the cutoff,
// and every rise above 0.3 with its time and peak.
package main

import (
	"encoding/binary"
	"errors"
	"flag"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"strings"

	"echo-assistant/internal/wakeword"
)

const (
	riseStart = 0.3 // as the device's near misses
	riseEnd   = 0.1
	chunk     = 160 // 10 ms
)

func main() {
	modelPath := flag.String("model", "", "wake word model (.tflite); default the built-in hey_jarvis")
	cutoff := flag.Float64("cutoff", 0.45, "detection threshold 0..1 (the Echo's wake_cutoff)")
	window := flag.Int("window", 5, "sliding window in inferences")
	gain := flag.Float64("gain", 1, "gain applied to the samples before the detector")
	flag.Parse()
	if flag.NArg() == 0 {
		flag.Usage()
		os.Exit(2)
	}
	model := wakeword.Builtin()
	if *modelPath != "" {
		var err error
		if model, err = os.ReadFile(*modelPath); err != nil {
			fail(err)
		}
	}
	for _, path := range flag.Args() {
		w, err := readWav(path)
		if err != nil {
			fail(fmt.Errorf("%s: %w", path, err))
		}
		fmt.Printf("%s (%d Hz, %d ch, %.1f s)\n", filepath.Base(path), w.rate, len(w.channels), w.seconds())
		if w.rate != 16000 {
			fmt.Println("  skipped: the detector needs 16 kHz")
			continue
		}
		tracks := w.tracks()
		for _, name := range trackOrder(tracks) {
			r, err := evaluate(model, *cutoff, *window, scale(tracks[name], *gain))
			if err != nil {
				fail(err)
			}
			fmt.Printf("  %-5s %s\n", name, r)
		}
	}
}

func fail(err error) {
	fmt.Fprintln(os.Stderr, "wakeeval:", err)
	os.Exit(1)
}

type wav struct {
	rate     int
	channels [][]int16
}

func (w *wav) seconds() float64 {
	if len(w.channels) == 0 {
		return 0
	}
	return float64(len(w.channels[0])) / float64(w.rate)
}

func readWav(path string) (*wav, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	return parseWav(data)
}

func parseWav(data []byte) (*wav, error) {
	if len(data) < 12 || string(data[:4]) != "RIFF" || string(data[8:12]) != "WAVE" {
		return nil, errors.New("not a WAV file")
	}
	var w wav
	nch := 0
	for i := 12; i+8 <= len(data); {
		id, size := string(data[i:i+4]), int(binary.LittleEndian.Uint32(data[i+4:]))
		body := data[i+8:]
		switch id {
		case "fmt ":
			if size < 16 || len(body) < 16 {
				return nil, errors.New("short fmt chunk")
			}
			if tag, bits := binary.LittleEndian.Uint16(body), binary.LittleEndian.Uint16(body[14:]); tag != 1 || bits != 16 {
				return nil, fmt.Errorf("only 16-bit PCM (format %d, %d bits)", tag, bits)
			}
			nch = int(binary.LittleEndian.Uint16(body[2:]))
			w.rate = int(binary.LittleEndian.Uint32(body[4:]))
		case "data":
			if nch == 0 {
				return nil, errors.New("data before fmt")
			}
			size = min(size, len(body)) // a recording cut short keeps what it has
			frames := size / (2 * nch)
			w.channels = make([][]int16, nch)
			for c := range w.channels {
				w.channels[c] = make([]int16, frames)
			}
			for f := 0; f < frames; f++ {
				for c := 0; c < nch; c++ {
					w.channels[c][f] = int16(binary.LittleEndian.Uint16(body[(f*nch+c)*2:]))
				}
			}
			return &w, nil
		}
		i += 8 + size + size%2
	}
	return nil, errors.New("no data chunk")
}

// tracks: "ch0", "ch1", ... and "mix" for files with several channels ("mono" for one).
func (w *wav) tracks() map[string][]int16 {
	t := map[string][]int16{}
	if len(w.channels) == 1 {
		t["mono"] = w.channels[0]
		return t
	}
	mix := make([]int16, len(w.channels[0]))
	for i := range mix {
		sum := 0
		for _, ch := range w.channels {
			sum += int(ch[i])
		}
		mix[i] = int16(sum / len(w.channels))
	}
	t["mix"] = mix
	for c, ch := range w.channels {
		t[fmt.Sprintf("ch%d", c)] = ch
	}
	return t
}

func trackOrder(t map[string][]int16) []string {
	var names []string
	for _, n := range []string{"mono", "mix"} {
		if _, ok := t[n]; ok {
			names = append(names, n)
		}
	}
	for c := 0; ; c++ {
		n := fmt.Sprintf("ch%d", c)
		if _, ok := t[n]; !ok {
			return names
		}
		names = append(names, n)
	}
}

func scale(pcm []int16, gain float64) []int16 {
	if gain == 1 {
		return pcm
	}
	out := make([]int16, len(pcm))
	for i, v := range pcm {
		out[i] = int16(math.Max(-32768, math.Min(32767, float64(v)*gain)))
	}
	return out
}

type rise struct {
	at, peak float64
	heard    bool
}

type result struct {
	peak  float64
	rises []rise
	level int // the signal's peak sample, to see how loud a channel is
}

func (r result) String() string {
	heard := 0
	var parts []string
	for _, x := range r.rises {
		mark := ""
		if x.heard {
			heard++
			mark = "*"
		}
		parts = append(parts, fmt.Sprintf("%.1fs:%.2f%s", x.at, x.peak, mark))
	}
	return fmt.Sprintf("peak %.2f  heard %d  level %5d  rises %s", r.peak, heard, r.level, strings.Join(parts, " "))
}

func evaluate(model []byte, cutoff float64, window int, pcm []int16) (result, error) {
	d, err := wakeword.New(model, cutoff, window)
	if err != nil {
		return result{}, err
	}
	var r result
	var cur *rise
	for i := 0; i+chunk <= len(pcm); i += chunk {
		for _, v := range pcm[i : i+chunk] {
			r.level = max(r.level, abs(int(v)))
		}
		heard := d.Feed(pcm[i : i+chunk])
		p := d.Latest()
		r.peak = math.Max(r.peak, p)
		t := float64(i+chunk) / 16000
		if cur == nil && (p >= riseStart || heard) {
			r.rises = append(r.rises, rise{at: t})
			cur = &r.rises[len(r.rises)-1]
		}
		if cur != nil {
			cur.peak = math.Max(cur.peak, p)
			cur.heard = cur.heard || heard
			if p < riseEnd {
				cur = nil
			}
		}
	}
	return r, nil
}

func abs(v int) int {
	if v < 0 {
		return -v
	}
	return v
}
