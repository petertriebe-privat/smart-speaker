package main

import (
	"encoding/json"
	"fmt"
	"log"
	"math"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"echo-assistant/internal/audio"
	"echo-assistant/internal/live"
	"echo-assistant/internal/store"
)

// Wake word clips (#61): the last seconds of microphone audio before every detection and every
// near miss are kept on the device, the training data for a wake word made for the household's
// voices. They never leave the device by themselves and never go into the repository.
//
// File names: <time>_<hit|near|second>_<peak>_<music|quiet>[_silent].wav; "_silent" is added when
// nobody spoke in the conversation a detection opened (likely a false detection). Next to every
// hit and second chance a <clip>.json (wakeRecord) says what followed it, for
// tools/wakeword/harvest.py to label the clip from facts (#78).

const (
	wakeClipLength = 3 * audio.MicRate // samples
	wakeClipsKept  = 400               // ~38 MB
	wakeClipMin    = 0.3               // near misses below this are not kept
	// second chance: two near misses within wakeSecondWithin, each at least wakeSecondBelow under
	// the cutoff (never under wakeSecondMin), count as a detection - the user repeating "Hey
	// The assistant" after it was missed (0.68-0.80 at cutoff 0.80, 17.10.2026; 0.33-0.35 at cutoff 0.65,
	// 19.10.2026, the stock model). The household's own model scores room noise at 0.5-0.77 (mic
	// peak 12-16): a fixed 0.3 started conversations nobody wanted (20.10.2026).
	wakeSecondMin    = 0.3
	wakeSecondBelow  = 0.1
	wakeSecondWithin = 4 * time.Second
)

type wakeClips struct {
	mu       sync.Mutex
	ring     []int16 // the last wakeClipLength samples, oldest first once full
	at       int
	full     bool
	lastNear time.Time
	second   bool        // the second chance fired: the next Feed counts as a detection
	lastHit  string      // the clip of the detection that opened the running conversation
	record   *wakeRecord // what followed it, written when the conversation is over
}

// wakeRecord: what followed a detection (<clip>.json). Only facts the device knows; harvest.py
// decides the label (a request done -> the word was real; nobody spoke and no other Echo heard it
// -> a false detection; the rest by hand).
type wakeRecord struct {
	Kind   string  `json:"kind"` // hit | second
	Score  float64 `json:"score"`
	Cutoff float64 `json:"cutoff"`
	Music  bool    `json:"music"`
	// Peer: the highest score another Echo claimed the same wake word with (0 = none heard it).
	Peer float64 `json:"peer"`
	// Answered: here | other (another Echo answered) | phone (it picked up a ringing phone) |
	// call (it joined a call on the speaker)
	Answered   string `json:"answered"`
	AskedAgain bool   `json:"asked_again,omitempty"` // "Wie bitte, Sir?": the outcome is the second conversation's
	HeardUser  bool   `json:"heard_user"`            // GPT Live transcribed the user
	Quick      string `json:"quick,omitempty"`       // the device's own transcription of the first words
	Done       string `json:"done,omitempty"`        // the quick command that ran on the device
	Tools      int    `json:"tools"`                 // tools GPT Live ran
	Seconds    int    `json:"seconds"`               // GPT Live billed
	Silent     bool   `json:"silent"`                // ended without a word, no greeting
}

func (w *wakeClips) feed(pcm []int16) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.ring == nil {
		w.ring = make([]int16, wakeClipLength)
	}
	for _, v := range pcm {
		w.ring[w.at] = v
		if w.at++; w.at == len(w.ring) {
			w.at, w.full = 0, true
		}
	}
}

// snapshot: the ring in order.
func (w *wakeClips) snapshot() []int16 {
	w.mu.Lock()
	defer w.mu.Unlock()
	if !w.full {
		return append([]int16(nil), w.ring[:w.at]...)
	}
	return append(append([]int16(nil), w.ring[w.at:]...), w.ring[:w.at]...)
}

// nearMiss: called by the detector; true when it gives the second chance.
func (w *wakeClips) nearMiss(peak, cutoff float64) bool {
	w.mu.Lock()
	defer w.mu.Unlock()
	if peak < max(wakeSecondMin, cutoff-wakeSecondBelow) {
		return false
	}
	now := time.Now()
	if !w.lastNear.IsZero() && now.Sub(w.lastNear) < wakeSecondWithin {
		w.lastNear, w.second = time.Time{}, true
		return true
	}
	w.lastNear = now
	return false
}

// takeSecond: the second chance fired since the last call.
func (w *wakeClips) takeSecond() bool {
	w.mu.Lock()
	defer w.mu.Unlock()
	s := w.second
	w.second = false
	return s
}

// save writes the clip in the background; hit = it opens a conversation.
func (w *wakeClips) save(kind string, peak, cutoff float64, music bool) {
	pcm := w.snapshot()
	if len(pcm) < audio.MicRate/2 {
		return
	}
	place := "quiet"
	if music {
		place = "music"
	}
	dir := filepath.Join(store.Dir, "wake")
	name := fmt.Sprintf("%s_%s_%02d_%s.wav", time.Now().Format("20060102-150405"), kind, int(peak*100), place)
	path := filepath.Join(dir, name)
	if kind != "near" {
		w.mu.Lock()
		if w.record != nil { // its conversation never ended here: kept as far as known
			writeRecord(w.lastHit, w.record)
		}
		w.lastHit = path
		w.record = &wakeRecord{Kind: kind, Score: round2(peak), Cutoff: round2(cutoff), Music: music, Answered: "here"}
		w.mu.Unlock()
	}
	go func() {
		os.MkdirAll(dir, 0o700)
		if err := os.WriteFile(path, wav(pcm, audio.MicRate), 0o600); err != nil {
			log.Printf("[Wake word] clip: %v", err)
			return
		}
		prune(dir, wakeClipsKept)
	}()
}

// note changes the record of the running detection (nothing when there is none).
func (w *wakeClips) note(change func(r *wakeRecord)) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.record != nil {
		change(w.record)
	}
}

// handled: the detection did not open a conversation (it picked up the phone, joined a call).
func (w *wakeClips) handled(answered string) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.record != nil {
		w.record.Answered = answered
		writeRecord(w.lastHit, w.record)
	}
	w.lastHit, w.record = "", nil
}

// conversationOver: the conversation a detection opened ended (every one, also without a word):
// its record is written; nobody spoke -> the clip is marked.
func (w *wakeClips) conversationOver(e live.Ended) {
	w.mu.Lock()
	path, r := w.lastHit, w.record
	if r != nil && r.AskedAgain && !e.Request.Again {
		r.Seconds += e.Seconds
		w.mu.Unlock() // "Wie bitte, Sir?": the conversation after it tells what was said
		return
	}
	w.lastHit, w.record = "", nil
	w.mu.Unlock()
	if path == "" || r == nil {
		return
	}
	r.HeardUser = r.HeardUser || e.HeardUser
	r.Silent = e.Silent
	r.Seconds += e.Seconds
	if e.Tools != "" {
		r.Tools += strings.Count(e.Tools, "\n") + 1
	}
	if !r.HeardUser && r.Done == "" && r.Answered == "here" {
		if err := os.Rename(path, strings.TrimSuffix(path, ".wav")+"_silent.wav"); err == nil {
			path = strings.TrimSuffix(path, ".wav") + "_silent.wav"
			log.Printf("[Wake word] nobody spoke after %s: marked as a likely false detection", filepath.Base(path))
		}
	}
	writeRecord(path, r)
}

func writeRecord(clip string, r *wakeRecord) {
	data, _ := json.Marshal(r)
	if err := os.WriteFile(strings.TrimSuffix(clip, ".wav")+".json", data, 0o600); err != nil {
		log.Printf("[Wake word] record: %v", err)
	}
}

func round2(v float64) float64 { return math.Round(v*100) / 100 }

// prune keeps the newest n clips of dir (the names start with the time), each with its record.
func prune(dir string, n int) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	var clips []string
	for _, e := range entries {
		if strings.HasSuffix(e.Name(), ".wav") {
			clips = append(clips, e.Name())
		}
	}
	if len(clips) <= n {
		return
	}
	sort.Strings(clips)
	for _, name := range clips[:len(clips)-n] {
		os.Remove(filepath.Join(dir, name))
		os.Remove(filepath.Join(dir, strings.TrimSuffix(name, ".wav")+".json"))
	}
}
