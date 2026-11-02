// Package board: which hardware the assistant runs on. One binary serves the Echo Gen 1 (DSP chain,
// PulseAudio, LP5523 ring) and the Echo Dot 2 (biscuit, MT8163: raw ALSA codecs, IS31FL3236
// ring, privacy switch), told apart at start by what the kernel offers.
package board

import "os"

// The Dot's privacy (mute) driver: no such directory on the Echo Gen 1.
const dotPrivacy = "/sys/devices/soc/10010000.keypad/amz_privacy"

// Dot2: running on an Echo Dot 2 with our own Linux (devices/echo/hardware/dot2/docs/21-echo-dot-linux-ssh.md).
var Dot2 = present(dotPrivacy)

// FireTV: running inside the assistant app on a Fire TV stick (devices/android/firetv), which starts
// The assistant with JARVIS_BOARD=firetv: no ring, no buttons, microphone and speaker are the app's
// (internal/audio/tv.go), the remote's microphone key is push to talk.
var FireTV = os.Getenv("JARVIS_BOARD") == "firetv"

// Phone: running inside the assistant app on an Android phone (devices/android/phone, #75), started
// with JARVIS_BOARD=phone: microphone and speaker are the app's as on the Fire TV, its the assistant
// button talks (tap: a conversation, held: push to talk; cmd/assistant/phoneboard.go).
var Phone = os.Getenv("JARVIS_BOARD") == "phone"

// Android: inside one of the Android apps (FireTV or Phone): no ring, no buttons, no Bluetooth
// stack, the clock is Android's, and Android's seccomp filter forbids some system calls.
var Android = FireTV || Phone

// Name for logs and status.
func Name() string {
	switch {
	case FireTV:
		return "Fire TV"
	case Phone:
		return "Phone"
	case Dot2:
		return "Echo Dot 2"
	}
	return "Echo Gen 1"
}

// Path picks the Echo's or the Dot's default of a file or directory.
func Path(echo, dot string) string {
	if Dot2 {
		return dot
	}
	return echo
}

// PrivacyFile: a file of the Dot's privacy driver (privacy_state: 1 = microphones cut).
func PrivacyFile(name string) string { return dotPrivacy + "/" + name }

func present(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}
