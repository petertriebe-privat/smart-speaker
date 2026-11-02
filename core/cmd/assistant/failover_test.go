package main

import (
	"os"
	"path/filepath"
	"testing"

	"echo-assistant/internal/config"
)

// The mirror carries the shared state, not what belongs to one device, and a takeover writes it
// back into the state dir.
func TestStateFilesAndTheirWayBack(t *testing.T) {
	dir := t.TempDir()
	for name, text := range map[string]string{
		"memory/core.json": "{}", "tasks.json": "[]", "google.json": `{"refresh_token":"r"}`,
		"errors.log": "x", "wake/a.wav": "x", "voice/ack.raw": "x", config.RoleFile: "{}", "mirror/tasks.json": "old",
		"voices/anna/near-1/micsout.wav": "x", "raw/A/mics.wav": "x",
	} {
		path := filepath.Join(dir, filepath.FromSlash(name))
		os.MkdirAll(filepath.Dir(path), 0o700)
		os.WriteFile(path, []byte(text), 0o600)
	}
	d := &device{cfg: &config.Config{Settings: config.Settings{StateDir: dir}}}
	files := d.stateFiles()
	for _, want := range []string{"memory/core.json", "tasks.json", "google.json"} {
		if _, ok := files[want]; !ok {
			t.Errorf("%s missing from the mirror", want)
		}
	}
	for _, not := range []string{"errors.log", "wake/a.wav", "voice/ack.raw", config.RoleFile, "mirror/tasks.json",
		"voices/anna/near-1/micsout.wav", "raw/A/mics.wav"} {
		if _, ok := files[not]; ok {
			t.Errorf("%s must not be mirrored", not)
		}
	}
	other := t.TempDir()
	if err := writeFiles(other, files); err != nil {
		t.Fatal(err)
	}
	if data, _ := os.ReadFile(filepath.Join(other, "memory", "core.json")); string(data) != "{}" {
		t.Errorf("core.json written back as %q", data)
	}
	if err := writeFiles(other, map[string][]byte{"../escape": nil}); err == nil {
		t.Error("a path out of the state dir accepted")
	}
}
