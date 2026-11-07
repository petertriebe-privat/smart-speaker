package main

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"echo-assistant/internal/audio"
	"echo-assistant/internal/live"
	"echo-assistant/internal/openai"
	"echo-assistant/internal/store"
)

// No words in the first utterance after the wake word (music or noise set it off): the device
// listens on and ends unnoticed, GPT Live is not opened (it greeted every false start in the car,
// #75).
func TestNoWordsAfterWakeWordListensOn(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"text": ""}`))
	}))
	defer srv.Close()
	openai.SetBase(srv.URL)
	defer openai.SetBase("https://api.openai.com")
	dir := store.Dir
	store.Dir = t.TempDir()
	defer func() { store.Dir = dir }()
	defer quickWrites.Wait() // the clip is written in the background: before TempDir is removed

	d := &device{}
	d.capture.active, d.capture.req, d.capture.gen, d.capture.deciding = true, live.Request{Wake: true}, 1, 1
	d.transcribeCommand(make([]int16, audio.MicRate), 1)
	if !d.capture.active || d.capture.gen != 1 {
		t.Fatalf("the start was decided (active %v, gen %d); want it listening on", d.capture.active, d.capture.gen)
	}
	if d.capture.deciding != 0 {
		t.Fatalf("deciding = %d, want 0", d.capture.deciding)
	}
}
