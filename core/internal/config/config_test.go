package config

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func quote(s string) string {
	b, _ := json.Marshal(s)
	return string(b)
}

func write(t *testing.T, path, text string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(text), 0o600); err != nil {
		t.Fatal(err)
	}
}

// device.json next to settings.json overrides it; fields it does not name keep their values.
func TestDeviceFileOverrides(t *testing.T) {
	dir := t.TempDir()
	secrets := filepath.Join(dir, "secrets.json")
	settings := filepath.Join(dir, "settings.json")
	write(t, secrets, `{"OPENAI_API_KEY": "k"}`)
	write(t, settings, `{"volume": 40, "primary": false}`)

	c, err := Load(secrets, settings)
	if err != nil {
		t.Fatal(err)
	}
	if c.Settings.Primary || !c.Settings.WakeWord || c.Settings.SpotifyName != "Jarvis" {
		t.Fatalf("without device.json: %+v", c.Settings)
	}

	write(t, filepath.Join(dir, DeviceFile), `{"primary": true, "wake_word": false, "spotify_name": "Jarvis 2", "room": "Kitchen"}`)
	c, err = Load(secrets, settings)
	if err != nil {
		t.Fatal(err)
	}
	s := c.Settings
	if !s.Primary || s.WakeWord || s.SpotifyName != "Jarvis 2" || s.Room != "Kitchen" || s.Volume != 40 {
		t.Fatalf("with device.json: %+v", s)
	}

	// a takeover's role.json in the state dir wins over device.json
	write(t, settings, `{"volume": 40, "primary": false, "state_dir": `+quote(dir)+`}`)
	if err := SaveRole(dir, false, 3); err != nil {
		t.Fatal(err)
	}
	if c, err = Load(secrets, settings); err != nil || c.Settings.Primary || c.Settings.Epoch != 3 || c.Settings.SpotifyName != "Jarvis 2" {
		t.Fatalf("with role.json: %+v %v", c.Settings, err)
	}

	// HA's name and room win over device.json; what HA leaves empty stays
	if err := SaveHA(dir, HAValues{Name: "Jarvis Bad", Room: "Bad", HAArea: "Bad"}); err != nil {
		t.Fatal(err)
	}
	if c, err = Load(secrets, settings); err != nil || c.Settings.Name != "Jarvis Bad" || c.Settings.Room != "Bad" ||
		c.Settings.SpotifyName != "Jarvis 2" || c.Settings.Epoch != 3 {
		t.Fatalf("with ha.json: %+v %v", c.Settings, err)
	}

	write(t, filepath.Join(dir, DeviceFile), `{"primary": tru`)
	if _, err := Load(secrets, settings); err == nil {
		t.Fatal("a broken device.json must not be ignored")
	}
}
