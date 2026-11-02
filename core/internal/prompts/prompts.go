// Package prompts gives the persona, instructions and settings shared with the M5
// (shared/prompts/*.h, devices/atoms3r/include/config.h), extracted by tools/extract_prompts.py
// into prompts.json.
package prompts

import (
	_ "embed"
	"encoding/json"
	"fmt"
	"sync"
)

//go:embed prompts.json
var raw []byte

var (
	valuesMu sync.RWMutex
	values   map[string]any
)

func get(name string) any {
	valuesMu.RLock()
	defer valuesMu.RUnlock()
	return values[name]
}

func init() {
	if err := json.Unmarshal(raw, &values); err != nil {
		panic(fmt.Sprintf("prompts: prompts.json: %v", err))
	}
}

// Text returns a string constant; panics if missing, so a renamed constant fails at start.
func Text(name string) string {
	v, ok := get(name).(string)
	if !ok {
		panic("prompts: missing text " + name)
	}
	return fill(v)
}

// List returns a string array constant (PERSONA_LEVELS).
func List(name string) []string {
	items, ok := get(name).([]any)
	if !ok {
		panic("prompts: missing list " + name)
	}
	out := make([]string, len(items))
	for i, item := range items {
		s, _ := item.(string)
		out[i] = fill(s)
	}
	return out
}

// TextOr returns a text constant, or def if it is missing.
func TextOr(name, def string) string {
	if v, ok := get(name).(string); ok && v != "" {
		return fill(v)
	}
	return def
}

// Int returns a numeric constant (milliseconds, levels), or def if missing.
func Int(name string, def int) int {
	if v, ok := get(name).(float64); ok {
		return int(v)
	}
	return def
}

// Float returns a numeric constant with its fraction (thresholds), or def if missing.
func Float(name string, def float64) float64 {
	if v, ok := get(name).(float64); ok {
		return v
	}
	return def
}

// Bool returns a boolean constant, or def if missing.
func Bool(name string, def bool) bool {
	if v, ok := get(name).(bool); ok {
		return v
	}
	return def
}
