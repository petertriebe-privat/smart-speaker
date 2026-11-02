// Package backup: copies of the assistant's files (core memory, people, tasks) outside the device, as
// devices/atoms3r/src/assistant/backup.cpp: in Google Drive when set up, else in the assistant's mailbox.
// On a new device an empty file is restored from there.
//
// While the M5 is still the primary device (settings "primary": false) the Echo reads backups
// but never writes them: its fresh files must not replace the M5's.
package backup

import "errors"

// Target is one place for backups (Drive, mailbox).
type Target interface {
	Available() bool
	// Store saves doc (JSON) as name (Drive) or under subject with a readable summary (mail).
	Store(name, subject, summary string, doc []byte) error
	// Load returns the backup, nil without error if there is none.
	Load(name, subject string) ([]byte, error)
}

var (
	// Targets in order of preference; the first available one is used.
	Targets []Target
	// Primary: this device may write backups.
	Primary bool
)

var ErrNone = errors.New("Error: no backup configured")

func target() Target {
	for _, t := range Targets {
		if t.Available() {
			return t
		}
	}
	return nil
}

func Available() bool { return target() != nil }

// Store writes a backup; a no-op while this device is not primary.
func Store(name, subject, summary string, doc []byte) error {
	if !Primary {
		return nil
	}
	t := target()
	if t == nil {
		return ErrNone
	}
	return t.Store(name, subject, summary, doc)
}

// Load reads a backup (nil, nil if there is none).
func Load(name, subject string) ([]byte, error) {
	t := target()
	if t == nil {
		return nil, ErrNone
	}
	return t.Load(name, subject)
}
