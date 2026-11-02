package bluetooth

import "os"

// The switches of hardware/gen1/src/echo-bluetooth.sh: Bluetooth starts only once the owner
// created the first, and stays off after it disturbed the WiFi until someone removes the second.
var (
	enabledFile = "/mnt/data/bluetooth.enabled"
	blockedFile = "/mnt/data/bluetooth.blocked"
)

// NotRunning: why Bluetooth does not run on this speaker, as a tool's error for the model.
// 21.10.2026 the answer was a vague "not running right now", and Jarvis promised to "note it":
// the owner had never switched it on there.
func NotRunning() string {
	switch {
	case exists(blockedFile):
		return "Error: Bluetooth is switched off on this speaker because it disturbed the WiFi; the owner has " +
			"to look at it (" + blockedFile + "). Tell the user so; you cannot switch it on yourself."
	case !exists(enabledFile):
		return "Error: Bluetooth is not switched on for this speaker yet (the owner switches it on once per " +
			"device: " + enabledFile + "). Tell the user so; you cannot switch it on yourself."
	}
	return "Error: Bluetooth is switched on but not up yet on this speaker (it starts within a minute; if it " +
		"stays down, /mnt/data/bluetooth.log says why). Ask the user to try again in a minute."
}

func exists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}
