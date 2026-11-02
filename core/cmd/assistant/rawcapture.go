package main

import (
	"encoding/binary"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"time"

	"echo-assistant/internal/audio"
)

// assistant rawcapture <seconds> <dir>: records the DSP's raw capture (internal/audio/ring.go) and
// The assistant's own input side by side as WAV files, all starting at the same moment:
//
//	mics.wav     the 7 raw microphones (16 kHz, 7 channels)
//	proc.wav     the DSP's processed signal (16 kHz mono)
//	ref.wav      the speaker reference (16 kHz stereo; stayed empty while Spotify played)
//	micsout.wav  BMicsOut, what the wake word hears today
//
// For the wake word (#61): which signal suits it best (cmd/wakeeval on the PC), and recordings in
// the Echo's own sound (the enrollment, enroll.go, records the same way). Household voices: the
// files stay on the device or the PC, never in the repository.

const (
	lipcSet        = "/usr/local/bin/lipc-set-prop"
	captureForever = "268435455" // 0x0FFFFFFF: audioproxyd's continuous capture
)

type rawRing struct{ file, shm string }

var (
	ringMics    = rawRing{"mics.wav", "/dev/shm/BCaptureMics.shm"}
	ringProc    = rawRing{"proc.wav", "/dev/shm/BCaptureProc.shm"}
	ringRef     = rawRing{"ref.wav", "/dev/shm/BCaptureRef.shm"}
	ringMicsOut = rawRing{"micsout.wav", "/dev/shm/BMicsOut.shm"}
)

func rawCapture(args []string) int {
	if len(args) != 2 {
		fmt.Fprintln(os.Stderr, "usage: assistant rawcapture <seconds> <dir>")
		return 2
	}
	seconds, err := strconv.Atoi(args[0])
	if err != nil || seconds < 1 || seconds > 600 {
		fmt.Fprintln(os.Stderr, "rawcapture: seconds 1..600")
		return 2
	}
	fmt.Printf("recording %d s into %s\n", seconds, args[1])
	stop := make(chan struct{})
	time.AfterFunc(time.Duration(seconds)*time.Second, func() { close(stop) })
	rec, err := recordRaw(args[1], []rawRing{ringMics, ringProc, ringRef, ringMicsOut}, stop, nil)
	if err != nil {
		fmt.Fprintln(os.Stderr, "rawcapture:", err)
		return 1
	}
	fmt.Print(rec)
	return 0
}

// rawRecording: what recordRaw wrote.
type rawRecording []struct {
	file     string
	seconds  float64
	channels int
	lost     uint64
}

func (r rawRecording) String() string {
	s := ""
	for _, t := range r {
		s += fmt.Sprintf("%s: %.1f s, %d channel(s)", t.file, t.seconds, t.channels)
		if t.lost > 0 {
			s += fmt.Sprintf(", %d frames lost", t.lost)
		}
		s += "\n"
	}
	return s
}

// recordRaw writes the rings into dir until stop closes; started (if not nil) is called at the
// files' first sample. It starts the DSP's raw capture if it is not running and stops it again
// at the end if it started it.
func recordRaw(dir string, rings []rawRing, stop <-chan struct{}, started func()) (rawRecording, error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	if !captureRunning() {
		if err := setCapture(captureForever); err != nil {
			return nil, fmt.Errorf("start: %w", err)
		}
		defer setCapture("0")
		time.Sleep(500 * time.Millisecond)
	}
	type track struct {
		ring *audio.Ring
		out  *wavWriter
		last uint64
		lost uint64
	}
	var tracks []*track
	defer func() {
		for _, t := range tracks {
			t.ring.Close()
		}
	}()
	for _, r := range rings {
		ring, err := audio.OpenRing(r.shm)
		if err != nil {
			return nil, err
		}
		out, err := newWavWriter(filepath.Join(dir, r.file), audio.MicRate, ring.Frame/2)
		if err != nil {
			ring.Close()
			return nil, err
		}
		tracks = append(tracks, &track{ring: ring, out: out})
	}
	for _, t := range tracks {
		t.last = t.ring.Written()
	}
	if started != nil {
		started()
	}
	buf := make([]byte, 0, 64*1024)
	var werr error
	for running := true; running && werr == nil; {
		select {
		case <-stop:
			running = false
		case <-time.After(20 * time.Millisecond):
		}
		for _, t := range tracks {
			now := t.ring.Written()
			if now-t.last > t.ring.Capacity() {
				t.lost += now - t.last - t.ring.Capacity()
				t.last = now - t.ring.Capacity()
			}
			buf = t.ring.Append(buf[:0], t.last, now)
			t.last = now
			if err := t.out.write(buf); err != nil && werr == nil {
				werr = err
			}
		}
	}
	var rec rawRecording
	for i, t := range tracks {
		if err := t.out.close(); err != nil && werr == nil {
			werr = err
		}
		rec = append(rec, struct {
			file     string
			seconds  float64
			channels int
			lost     uint64
		}{rings[i].file, float64(t.out.frames) / audio.MicRate, t.out.channels, t.lost})
	}
	return rec, werr
}

// captureRunning: the continuous capture's microphone ring exists and moves.
func captureRunning() bool {
	r, err := audio.OpenRing(ringMics.shm)
	if err != nil {
		return false
	}
	defer r.Close()
	before := r.Written()
	time.Sleep(200 * time.Millisecond)
	return r.Written() != before
}

func setCapture(value string) error {
	cmd := exec.Command(lipcSet, "-i", "com.doppler.audio", "capture", value)
	cmd.Env = append(os.Environ(), "LD_LIBRARY_PATH=/usr/local/lib:/usr/lib:/lib")
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("%v: %s", err, out)
	}
	return nil
}

// wavWriter streams 16-bit PCM into a WAV file; the sizes are filled in by close.
type wavWriter struct {
	f        *os.File
	channels int
	frames   int64
}

func newWavWriter(path string, rate, channels int) (*wavWriter, error) {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o600)
	if err != nil {
		return nil, err
	}
	w := &wavWriter{f: f, channels: channels}
	return w, w.header(rate)
}

func (w *wavWriter) header(rate int) error {
	if _, err := w.f.Seek(0, 0); err != nil {
		return err
	}
	size := uint32(w.frames) * uint32(w.channels) * 2
	h := make([]byte, 0, 44)
	h = append(h, "RIFF"...)
	h = binary.LittleEndian.AppendUint32(h, 36+size)
	h = append(h, "WAVEfmt "...)
	h = binary.LittleEndian.AppendUint32(h, 16)
	h = binary.LittleEndian.AppendUint16(h, 1) // PCM
	h = binary.LittleEndian.AppendUint16(h, uint16(w.channels))
	h = binary.LittleEndian.AppendUint32(h, uint32(rate))
	h = binary.LittleEndian.AppendUint32(h, uint32(rate*w.channels*2))
	h = binary.LittleEndian.AppendUint16(h, uint16(w.channels*2))
	h = binary.LittleEndian.AppendUint16(h, 16)
	h = append(h, "data"...)
	h = binary.LittleEndian.AppendUint32(h, size)
	_, err := w.f.Write(h)
	return err
}

func (w *wavWriter) write(pcm []byte) error {
	if len(pcm) == 0 {
		return nil
	}
	if _, err := w.f.Write(pcm); err != nil {
		return err
	}
	w.frames += int64(len(pcm) / (2 * w.channels))
	return nil
}

func (w *wavWriter) close() error {
	err := w.header(audio.MicRate)
	if cerr := w.f.Close(); err == nil {
		err = cerr
	}
	return err
}
