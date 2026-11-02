package wakeword

import (
	"encoding/binary"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

// TestReference compares the port with TFLite Micro's frontend and the TFLite runtime: set
// WAKEWORD_REF to a directory with stream.raw, features.txt and probs.txt (made by a reference
// script with pymicro_features and pymicro_wakeword). Features must match exactly; the model may
// differ now and then (see below).
func TestReference(t *testing.T) {
	dir := os.Getenv("WAKEWORD_REF")
	if dir == "" {
		t.Skip("WAKEWORD_REF not set")
	}
	raw, err := os.ReadFile(filepath.Join(dir, "stream.raw"))
	if err != nil {
		t.Fatal(err)
	}
	samples := make([]int16, len(raw)/2)
	for i := range samples {
		samples[i] = int16(binary.LittleEndian.Uint16(raw[2*i:]))
	}
	featText, _ := os.ReadFile(filepath.Join(dir, "features.txt"))
	var want [][]int
	for _, line := range strings.Split(strings.TrimSpace(string(featText)), "\n") {
		var row []int
		for _, f := range strings.Fields(line) {
			v, _ := strconv.Atoi(f)
			row = append(row, v)
		}
		want = append(want, row)
	}
	probText, _ := os.ReadFile(filepath.Join(dir, "probs.txt"))
	var wantProbs []int
	for _, f := range strings.Fields(string(probText)) {
		v, _ := strconv.Atoi(f)
		wantProbs = append(wantProbs, v)
	}

	fe := NewFrontend()
	var got [][]int
	for i := 0; i+160 <= len(samples); i += 160 {
		fe.Process(samples[i:i+160], func(v []uint16) {
			row := make([]int, len(v))
			for k, x := range v {
				row[k] = int(x)
			}
			got = append(got, row)
		})
	}
	if len(got) != len(want) {
		t.Fatalf("frames: got %d, want %d", len(got), len(want))
	}
	bad := 0
	for i := range want {
		for k := range want[i] {
			if got[i][k] != want[i][k] {
				if bad < 10 {
					t.Errorf("frame %d channel %d: got %d, want %d", i, k, got[i][k], want[i][k])
				}
				bad++
			}
		}
	}
	if bad > 0 {
		t.Fatalf("%d feature values differ", bad)
	}

	modelBytes := builtin
	if p := os.Getenv("WAKEWORD_MODEL"); p != "" { // a trained model (tools/wakeword/reference.py)
		if modelBytes, err = os.ReadFile(p); err != nil {
			t.Fatal(err)
		}
	}
	model, err := LoadModel(modelBytes)
	if err != nil {
		t.Fatal(err)
	}
	in := model.Input()
	// TFLite Micro (and this port) computes the requantization scales in float, the full TFLite
	// runtime in double: the last layer's logit then differs by 1 LSB now and then, which the
	// sigmoid turns into up to ~10/255 where it is steep. Rare, and never at 0 or 255. Off by
	// exactly 1 is the output's own rounding: a trained model of 20.10.2026 had it in 17 % of the
	// inferences (never more than 1), so only larger differences count against the 5 %.
	maxDiff, differing, beyondOne, started := 0, 0, 0, time.Now()
	for i := 0; i*3+2 < len(got) && i < len(wantProbs); i++ {
		for k := 0; k < 3; k++ {
			for c, v := range got[i*3+k] {
				in.i8[k*features+c] = int8(clamp((int32(v)*256+333)/666-128, -128, 127))
			}
		}
		p := int(model.Invoke())
		d := p - wantProbs[i]
		if d < 0 {
			d = -d
		}
		if d > 0 {
			differing++
		}
		if d > 1 {
			beyondOne++
		}
		maxDiff = max(maxDiff, d)
	}
	t.Logf("%d inferences in %v: %d differ, by up to %d", len(wantProbs), time.Since(started), differing, maxDiff)
	if maxDiff > 16 || beyondOne > len(wantProbs)/20 {
		t.Errorf("probabilities differ too much (%d of %d, up to %d)", differing, len(wantProbs), maxDiff)
	}

	// The detector decides as the reference's probabilities would.
	wantHeard := detections(wantProbs, 0.85, 5)
	det, _ := New(modelBytes, 0.85, 5)
	var heard []float64
	for i := 0; i+160 <= len(samples); i += 160 {
		if det.Feed(samples[i : i+160]) {
			heard = append(heard, float64(i)/sampleRate)
		}
	}
	t.Logf("detections at %v s (reference: %d)", heard, wantHeard)
	if len(heard) != wantHeard || wantHeard == 0 && os.Getenv("WAKEWORD_MODEL") == "" {
		t.Errorf("want %d detections, got %d", wantHeard, len(heard))
	}
}

// detections: what Detector.infer decides on a sequence of probabilities.
func detections(probs []int, cutoff float64, window int) int {
	history, pos, runs, count := make([]int, window), 0, 0, 0
	for _, p := range probs {
		history[pos] = p
		pos = (pos + 1) % window
		sum := 0
		for _, h := range history {
			sum += h
		}
		if runs++; runs < warmupInferences {
			continue
		}
		if sum/window > int(cutoff*255) {
			count++
			clear(history)
			runs = 0
		}
	}
	return count
}
