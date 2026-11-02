// Package config loads the private household values (generated from the M5 project's
// secrets.h by tools/make_secrets.py) and the device settings.
package config

import (
	"echo-assistant/internal/prompts"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"

	"echo-assistant/internal/board"
)

// Secrets: the values of secrets.h by name (OPENAI_API_KEY, HA_URL, ...).
type Secrets map[string]any

func (s Secrets) String(name string) string {
	v, _ := s[name].(string)
	return v
}

// Settings of this device; defaults are used for missing fields.
type Settings struct {
	Volume    int    `json:"volume"`     // MainVolume 0..100 at start
	LedLevel  int    `json:"led_level"`  // brightness of the ring 0..255
	LedOffset int    `json:"led_offset"` // ring position of the first LED (rotation)
	StateDir  string `json:"state_dir"`  // persistent data (memory, usage)
	RunDir    string `json:"run_dir"`    // RAM: control socket, logs
	EchoGate  bool   `json:"echo_gate"`  // mute the microphone while the assistant speaks (no barge-in)
	Primary   bool   `json:"primary"`    // the household's assistant: backups, archive, pollers, phone; true only in one Echo's device.json
	// SpotifyName: the Echo's name as a Spotify Connect speaker (librespot); "" = no speaker.
	SpotifyName string `json:"spotify_name"`
	// VoicePeak: microphone peak that counts as the user speaking (the DSP's output is quieter
	// than the M5's microphone, whose VOICE_PEAK is 1000). 300 until 20.10.2026: a command spoken
	// from a few metres peaks at 130-400 in BMicsOut (the guided recordings; the room 10-60), was
	// taken for silence and dropped before GPT Live heard it ("Jarvis, spiel meine Musik" -> a
	// greeting).
	VoicePeak int `json:"voice_peak"`
	// WakeCutoff: the wake word's probability threshold 0..1 (the M5: 0.6). 0.8 missed many real
	// "Hey Jarvis" at 0.68-0.80 (17.10.2026, #61); 0.65 still needed many tries, real attempts at
	// 0.33-0.35 (19.10.2026): 0.45, at the cost of more false starts.
	WakeCutoff float64 `json:"wake_cutoff"`
	// WakeCutoffMusic: the threshold while music plays here. The DSP's echo cancellation then
	// takes much of the user's voice too: "Hey Jarvis" reached only 0.45 (16.10.2026).
	WakeCutoffMusic float64 `json:"wake_cutoff_music"`
	// MusicRing: while music plays here and nobody talks, the ring shows its spectrum.
	MusicRing bool `json:"music_ring"`
	// WakeWord: listen for "Hey Jarvis". Off on a second Echo in earshot of the first until the
	// devices agree who answers (#62); the button still works.
	WakeWord bool `json:"wake_word"`
	// WakeModel: a microWakeWord model (.tflite, with its .json manifest next to it for the
	// sliding window), absolute or relative to state_dir; "" = the built-in "Hey Jarvis" (#61).
	// A model that does not load falls back to the built-in one.
	WakeModel string `json:"wake_model"`
	// MemoryLimitMB: The assistant ends itself above this resident size, or above half of it while the
	// system runs low, before the Echo starves and SSH stops answering (internal/memguard).
	// Normal: ~18 MB.
	MemoryLimitMB int `json:"memory_limit_mb"`
	// RealtimeModel: the Realtime API with this model instead of GPT Live (push to talk on the Fire
	// TV: gpt-realtime-2.1, gpt-realtime-mini); "" = GPT Live. HA's select can change it.
	RealtimeModel string `json:"realtime_model"`
	// FireTVAlexa: on the Fire TV keep Alexa's voice app (the remote's key opens Alexa); false =
	// The assistant switches it off at every start, Fire OS switches it on again after a restart
	// (cmd/assistant/tvkeep.go).
	FireTVAlexa bool `json:"fire_tv_alexa"`
	// Room and HAArea: where this Echo stands (several Echos, #58); "" = not set.
	Room   string `json:"room"`
	HAArea string `json:"ha_area"`
	// Name on the peer link ("" = the Spotify name), Priority (ties of the wake word, later the
	// leader), Peers: addresses of the other Echos ("" = the LAN's broadcast address finds them).
	Name     string   `json:"name"`
	Priority int      `json:"priority"`
	Peers    []string `json:"peers"`
	// Epoch of the primary role: raised by every takeover; the higher one stays primary.
	Epoch int `json:"epoch"`
	// NoLead: this device never takes the primary role over when the primary is silent (left out
	// of the election, as the M5). 20.10.2026 the Echo Dot 2 missed the others' heartbeats in
	// bursts, took over from a primary that was fine, and the Echos followed it.
	NoLead bool `json:"no_lead"`
	// MicGainDB: the Echo Dot 2's software gain on its centre microphone (no DSP there; at 0 dB
	// its level is about the Echo's BMicsOut). Unused on the Echo Gen 1.
	MicGainDB float64 `json:"mic_gain_db"`
	// SpeakerGainDB: the Echo Dot 2's speaker driver (HP Driver Gain, 1 dB steps over Fire OS's
	// 0 dB). At 0 dB the Dot was too quiet at full volume; a test tone rose evenly up to +12 dB
	// and music at full volume still sounded clean (21.10.2026). Unused on the Echo Gen 1.
	SpeakerGainDB float64 `json:"speaker_gain_db"`
	// BTSpeaker: the Echo Dot 2 plays through this Bluetooth speaker (its address, paired once
	// with btspeaker) while it is in reach, else through its own (#74). "" = its own only.
	BTSpeaker string `json:"bt_speaker"`
	// Set from Home Assistant (#65, ha.json): WakeSensitivity "niedrig" / "normal" / "hoch" shifts
	// both wake cutoffs; Night: the night mode HA switches (volume at most NightVolume, the ring
	// dimmed).
	WakeSensitivity string `json:"wake_sensitivity"`
	Night           bool   `json:"night"`
	NightVolume     int    `json:"night_volume"`
}

// RoleFile: the role as the devices agreed on it (state_dir; never touched by a deploy, so the
// role stays where a takeover moved it). It overrides device.json.
const RoleFile = "role.json"

// SaveRole writes this device's role for the next start.
func SaveRole(stateDir string, primary bool, epoch int) error {
	data, _ := json.Marshal(map[string]any{"primary": primary, "epoch": epoch})
	tmp := filepath.Join(stateDir, RoleFile+".new")
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, filepath.Join(stateDir, RoleFile))
}

// HAFile: what the owner set in Home Assistant's UI (#65): the device's name and area, and the
// speaker's settings there; in state_dir, over device.json, under role.json. Without HA
// device.json stays in force.
const HAFile = "ha.json"

// HAValues: what HA decides; empty / nil leaves device.json's value.
type HAValues struct {
	Name            string  `json:"name,omitempty"`
	SpotifyName     string  `json:"spotify_name,omitempty"`
	Room            string  `json:"room,omitempty"`
	HAArea          string  `json:"ha_area,omitempty"`
	WakeSensitivity string  `json:"wake_sensitivity,omitempty"`
	NoLead          *bool   `json:"no_lead,omitempty"`
	LedLevel        *int    `json:"led_level,omitempty"`
	Night           *bool   `json:"night,omitempty"`
	NightVolume     int     `json:"night_volume,omitempty"`
	RealtimeModel   *string `json:"realtime_model,omitempty"` // "" = GPT Live (the Fire TV's select)
}

// LoadHA reads ha.json (empty values when there is none).
func LoadHA(stateDir string) HAValues {
	var v HAValues
	if data, err := os.ReadFile(filepath.Join(stateDir, HAFile)); err == nil {
		json.Unmarshal(data, &v)
	}
	return v
}

// UpdateHA changes ha.json with change (read, change, write; one writer at a time).
func UpdateHA(stateDir string, change func(*HAValues)) error {
	haMu.Lock()
	defer haMu.Unlock()
	v := LoadHA(stateDir)
	change(&v)
	return SaveHA(stateDir, v)
}

var haMu sync.Mutex

// SaveHA writes HA's values for the next start.
func SaveHA(stateDir string, v HAValues) error {
	data, _ := json.Marshal(v)
	tmp := filepath.Join(stateDir, HAFile+".new")
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, filepath.Join(stateDir, HAFile))
}

// DeviceFile: the per-device settings next to settings.json (room, role, Spotify name, ...),
// written once per device and never overwritten by a deploy unless the PC has a newer copy
// (devices/echo/local/devices/<host>.json). They override settings.json.
const DeviceFile = "device.json"

// DefaultLedLevel: the ring's brightness when nothing set another (0..255).
const DefaultLedLevel = 120

type Config struct {
	Secrets  Secrets
	Settings Settings
}

func Load(secretsPath, settingsPath string) (*Config, error) {
	c := &Config{Settings: Settings{
		Volume: 40, LedLevel: DefaultLedLevel,
		StateDir: board.Path("/mnt/data/jarvis", "/data/jarvis"), RunDir: board.Path("/mnt/jarvis", "/run/jarvis"),
		EchoGate: true, SpotifyName: prompts.Name(),
		VoicePeak: 120, WakeCutoff: 0.45, WakeCutoffMusic: 0.4, MusicRing: true, WakeWord: true, MemoryLimitMB: 96,
		SpeakerGainDB: 12, NightVolume: 25,
	}}
	data, err := os.ReadFile(secretsPath)
	if err != nil {
		return nil, fmt.Errorf("secrets: %w", err)
	}
	if err := json.Unmarshal(data, &c.Secrets); err != nil {
		return nil, fmt.Errorf("secrets: %w", err)
	}
	for _, path := range []string{settingsPath, filepath.Join(filepath.Dir(settingsPath), DeviceFile)} {
		if data, err := os.ReadFile(path); err == nil {
			if err := json.Unmarshal(data, &c.Settings); err != nil {
				return nil, fmt.Errorf("settings %s: %w", filepath.Base(path), err)
			}
		}
	}
	for _, name := range []string{HAFile, RoleFile} {
		if data, err := os.ReadFile(filepath.Join(c.Settings.StateDir, name)); err == nil {
			if err := json.Unmarshal(data, &c.Settings); err != nil {
				return nil, fmt.Errorf("settings %s: %w", name, err)
			}
		}
	}
	if c.Secrets.String("OPENAI_API_KEY") == "" {
		return nil, fmt.Errorf("secrets: OPENAI_API_KEY missing")
	}
	return c, nil
}
