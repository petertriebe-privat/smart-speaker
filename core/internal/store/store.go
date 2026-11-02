// Package store: The assistant's persistent files on the Echo's data partition (state_dir, default
// /mnt/data/jarvis). Writes are atomic (temporary file, fsync, rename), so a power cut leaves the
// old or the new version, never half a file.
package store

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"sync"
)

var (
	Dir = "/mnt/data/jarvis"
	mu  sync.Mutex // serializes writes of the same file from several goroutines
)

func Path(name string) string { return filepath.Join(Dir, name) }

// Write replaces a file atomically.
func Write(name string, data []byte) error {
	mu.Lock()
	defer mu.Unlock()
	path := Path(name)
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return err
	}
	tmp := path + ".tmp"
	f, err := os.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0600)
	if err != nil {
		return err
	}
	if _, err := f.Write(data); err != nil {
		f.Close()
		return err
	}
	if err := f.Sync(); err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

func Read(name string) ([]byte, error) { return os.ReadFile(Path(name)) }

func Exists(name string) bool {
	_, err := os.Stat(Path(name))
	return err == nil
}

func Remove(name string) error { return os.Remove(Path(name)) }

// SaveJSON writes v as indented JSON.
func SaveJSON(name string, v any) error {
	data, err := json.MarshalIndent(v, "", " ")
	if err != nil {
		return err
	}
	return Write(name, data)
}

// LoadJSON reads a JSON file into v; false if it does not exist or is broken.
func LoadJSON(name string, v any) bool {
	data, err := Read(name)
	if err != nil {
		return false
	}
	return json.Unmarshal(data, v) == nil
}

// Append adds a line to a file (logs, index).
func Append(name string, line []byte) error {
	mu.Lock()
	defer mu.Unlock()
	path := Path(name)
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return err
	}
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_APPEND, 0600)
	if err != nil {
		return err
	}
	defer f.Close()
	if _, err := f.Write(append(line, '\n')); err != nil {
		return err
	}
	return f.Sync()
}

// List: names of the files in a directory, sorted (oldest first for time-stamped names).
func List(dir string) []string {
	entries, err := os.ReadDir(Path(dir))
	if err != nil {
		return nil
	}
	var names []string
	for _, e := range entries {
		if !e.IsDir() {
			names = append(names, e.Name())
		}
	}
	sort.Strings(names)
	return names
}

// Marshal and Unmarshal: JSON as the store writes it (for backups of the same files).
func Marshal(v any) ([]byte, error)      { return json.MarshalIndent(v, "", " ") }
func Unmarshal(data []byte, v any) error { return json.Unmarshal(data, v) }
