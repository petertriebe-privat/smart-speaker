package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"echo-assistant/internal/live"
	"echo-assistant/internal/store"
)

// hitClip: a detection saved into a fresh store; returns its path once the clip is written.
func hitClip(t *testing.T, w *wakeClips) string {
	t.Helper()
	store.Dir = t.TempDir()
	w.feed(make([]int16, 16000))
	w.save("hit", 0.913, 0.85, false)
	path := w.lastHit
	for i := 0; i < 100; i++ {
		if _, err := os.Stat(path); err == nil {
			return path
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("clip not written")
	return ""
}

func readRecord(t *testing.T, clip string) wakeRecord {
	t.Helper()
	data, err := os.ReadFile(clip[:len(clip)-len(".wav")] + ".json")
	if err != nil {
		t.Fatal(err)
	}
	var r wakeRecord
	if err := json.Unmarshal(data, &r); err != nil {
		t.Fatal(err)
	}
	return r
}

func TestWakeRecordFalseStart(t *testing.T) {
	var w wakeClips
	clip := hitClip(t, &w)
	w.note(func(r *wakeRecord) { r.Quick = "Das war eine sehr gute Auswahl." })
	w.conversationOver(live.Ended{Request: live.Request{Wake: true}, Seconds: 8, Silent: true})
	silent := clip[:len(clip)-len(".wav")] + "_silent.wav"
	if _, err := os.Stat(silent); err != nil {
		t.Fatalf("clip not marked silent: %v", err)
	}
	r := readRecord(t, silent)
	if r.Kind != "hit" || r.Score != 0.91 || r.Cutoff != 0.85 || r.Answered != "here" || !r.Silent ||
		r.HeardUser || r.Seconds != 8 || r.Quick == "" {
		t.Fatalf("record %+v", r)
	}
	if w.record != nil || w.lastHit != "" {
		t.Fatal("the record outlived its conversation")
	}
}

func TestWakeRecordRequest(t *testing.T) {
	var w wakeClips
	clip := hitClip(t, &w)
	w.note(func(r *wakeRecord) { r.Peer = 0.88 })
	// "Wie bitte, Sir?": the first conversation keeps the record for the second
	w.note(func(r *wakeRecord) { r.AskedAgain = true })
	w.conversationOver(live.Ended{Request: live.Request{Wake: true}, Seconds: 3})
	if w.record == nil {
		t.Fatal("the record ended with the first conversation")
	}
	w.conversationOver(live.Ended{Request: live.Request{Wake: true, Again: true}, Seconds: 12, HeardUser: true,
		Tools: "control_home {…}\nspeak {…}"})
	r := readRecord(t, clip)
	if !r.HeardUser || r.Tools != 2 || r.Seconds != 15 || r.Peer != 0.88 || r.Silent {
		t.Fatalf("record %+v", r)
	}
	if matches, _ := filepath.Glob(filepath.Join(store.Dir, "wake", "*_silent*")); len(matches) > 0 {
		t.Fatalf("marked silent: %v", matches)
	}
}

func TestWakeRecordOtherEcho(t *testing.T) {
	var w wakeClips
	clip := hitClip(t, &w)
	w.note(func(r *wakeRecord) { r.Answered = "other" })
	w.conversationOver(live.Ended{Request: live.Request{Wake: true}})
	if r := readRecord(t, clip); r.Answered != "other" {
		t.Fatalf("record %+v", r)
	}
}

func TestPruneKeepsRecords(t *testing.T) {
	dir := t.TempDir()
	for _, name := range []string{"20261022-000001_hit_90_quiet.wav", "20261022-000001_hit_90_quiet.json",
		"20261022-000002_near_50_quiet.wav", "20261022-000003_hit_91_quiet.wav", "20261022-000003_hit_91_quiet.json"} {
		os.WriteFile(filepath.Join(dir, name), nil, 0o600)
	}
	prune(dir, 2)
	left, _ := os.ReadDir(dir)
	if len(left) != 3 {
		t.Fatalf("left %d files, want the newest 2 clips and 1 record", len(left))
	}
	if _, err := os.Stat(filepath.Join(dir, "20261022-000001_hit_90_quiet.json")); err == nil {
		t.Fatal("the oldest clip's record was kept")
	}
}

func TestSecondChanceFollowsCutoff(t *testing.T) {
	// the own model at cutoff 0.85: room noise at 0.60 and 0.54 must not open a conversation
	var w wakeClips
	if w.nearMiss(0.60, 0.85) || w.nearMiss(0.54, 0.85) || w.takeSecond() {
		t.Fatal("near misses far under the cutoff gave the second chance")
	}
	if w.nearMiss(0.78, 0.85) || !w.nearMiss(0.80, 0.85) || !w.takeSecond() {
		t.Fatal("two near misses just under the cutoff did not give the second chance")
	}
	// the stock model at cutoff 0.45: the floor of 0.3 still holds
	var s wakeClips
	if s.nearMiss(0.33, 0.45) || s.nearMiss(0.36, 0.45) || !s.nearMiss(0.36, 0.45) {
		t.Fatal("stock model: the second chance just over 0.35 was lost")
	}
	var f wakeClips
	if f.nearMiss(0.25, 0.3) || f.nearMiss(0.29, 0.3) {
		t.Fatal("near misses under the floor of 0.3 gave the second chance")
	}
}
