package wakeword

import (
	"os"
	"path/filepath"
	"testing"
)

func TestLoadFileWithManifest(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "jarvis.tflite")
	os.WriteFile(path, Builtin(), 0o600)
	os.WriteFile(filepath.Join(dir, "jarvis.json"), []byte(`{"micro":{"sliding_window_size":3}}`), 0o600)
	model, window, err := LoadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(model) != len(Builtin()) || window != 3 {
		t.Fatalf("model %d bytes, window %d", len(model), window)
	}
}

func TestLoadFileWithoutManifest(t *testing.T) {
	path := filepath.Join(t.TempDir(), "m.tflite")
	os.WriteFile(path, Builtin(), 0o600)
	if _, window, err := LoadFile(path); err != nil || window != 0 {
		t.Fatalf("window %d, err %v", window, err)
	}
}

func TestLoadFileRejectsBrokenModel(t *testing.T) {
	path := filepath.Join(t.TempDir(), "broken.tflite")
	os.WriteFile(path, Builtin()[:1000], 0o600)
	if _, _, err := LoadFile(path); err == nil {
		t.Fatal("a truncated model loaded")
	}
	if _, _, err := LoadFile(filepath.Join(t.TempDir(), "missing.tflite")); err == nil {
		t.Fatal("a missing model loaded")
	}
}
