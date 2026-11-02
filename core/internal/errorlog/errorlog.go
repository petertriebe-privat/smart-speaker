// Package errorlog: errors worth reading later, kept on the data partition with the time, as
// devices/atoms3r/src/system/error_log.cpp. The runtime log lives in RAM and is gone after a reboot
// (and the device once hung without anyone knowing why). Failed sessions, broken-off calls,
// hardware that does not answer, every start. "jarvis ctl errors" prints it. The file is cut to
// its newer half beyond maxBytes.
package errorlog

import (
	"fmt"
	"log"
	"os"
	"path/filepath"
	"sync"
	"time"
)

const maxBytes = 64 * 1024

var (
	mu   sync.Mutex
	path string
	last string
)

// Last: the newest line added since the start ("" if none), for HA's diagnostics (#65).
func Last() string {
	mu.Lock()
	defer mu.Unlock()
	return last
}

// Open sets the directory (the state directory); before that Add only logs.
func Open(dir string) {
	mu.Lock()
	path = filepath.Join(dir, "errors.log")
	mu.Unlock()
}

// Add logs the line and appends it to the file.
func Add(format string, args ...any) {
	line := fmt.Sprintf(format, args...)
	log.Printf("[Error log] %s", line)
	mu.Lock()
	defer mu.Unlock()
	last = time.Now().Format("02.01. 15:04") + " " + line
	if path == "" {
		return
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return
	}
	fmt.Fprintf(f, "%s %s\n", time.Now().Format("2006-01-02 15:04:05"), line)
	size, _ := f.Seek(0, 2)
	f.Close()
	if size > maxBytes {
		cut()
	}
}

// cut keeps the newer half, from a line start. Called with mu held.
func cut() {
	data, err := os.ReadFile(path)
	if err != nil {
		return
	}
	half := data[len(data)/2:]
	for i, b := range half {
		if b == '\n' {
			half = half[i+1:]
			break
		}
	}
	os.WriteFile(path, half, 0o600)
}

// Read returns the file ("" if none).
func Read() string {
	mu.Lock()
	defer mu.Unlock()
	if path == "" {
		return ""
	}
	data, _ := os.ReadFile(path)
	return string(data)
}
