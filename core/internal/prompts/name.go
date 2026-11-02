package prompts

import (
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"sync"
)

// The assistant's name as users say and hear it (#29): ASSISTANT_NAME in the household's secrets,
// "Jarvis" by default. The texts from shared/prompts carry it as a placeholder.
const placeholder = "{{ASSISTANT_NAME}}"

var (
	nameMu sync.RWMutex
	name   = "Jarvis"
)

// SetName sets the assistant's name; "" keeps the default. Called once at start.
func SetName(n string) {
	if n = strings.TrimSpace(n); n != "" {
		nameMu.Lock()
		name = n
		nameMu.Unlock()
	}
}

// Name is the assistant's name.
func Name() string {
	nameMu.RLock()
	defer nameMu.RUnlock()
	return name
}

// Slug is the name as an identifier: lower case, a-z, 0-9 and _ ("Jarvis" -> "jarvis"). It is the
// prefix of the Home Assistant events and entities (jarvis_announce, binary_sensor.jarvis_presence).
func Slug() string {
	return slug(Name())
}

func slug(s string) string {
	s = strings.NewReplacer("ä", "ae", "ö", "oe", "ü", "ue", "ß", "ss", "Ä", "ae", "Ö", "oe", "Ü", "ue").Replace(s)
	var b strings.Builder
	for _, r := range strings.ToLower(s) {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9':
			b.WriteRune(r)
		case b.Len() > 0 && !strings.HasSuffix(b.String(), "_"):
			b.WriteByte('_')
		}
	}
	if out := strings.TrimSuffix(b.String(), "_"); out != "" {
		return out
	}
	return "assistant"
}

func fill(s string) string {
	if !strings.Contains(s, placeholder) {
		return s
	}
	return strings.ReplaceAll(s, placeholder, Name())
}

// LoadPersona replaces the built-in persona (PERSONA_* of shared/prompts/persona.h) with the
// household's own, written by tools/extract_prompts.py --persona. A missing file is no error.
func LoadPersona(path string) error {
	data, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return nil
	} else if err != nil {
		return err
	}
	var persona map[string]any
	if err := json.Unmarshal(data, &persona); err != nil {
		return fmt.Errorf("%s: %v", path, err)
	}
	for key := range persona {
		if !strings.HasPrefix(key, "PERSONA_") {
			return fmt.Errorf("%s: %s is not part of the persona", path, key)
		}
	}
	valuesMu.Lock()
	defer valuesMu.Unlock()
	for key, v := range persona {
		values[key] = v
	}
	return nil
}

// Genitive is the name in German genitive: "Jarvis'", "Fridays".
func Genitive() string {
	n := Name()
	if strings.HasSuffix(n, "s") || strings.HasSuffix(n, "ß") || strings.HasSuffix(n, "x") || strings.HasSuffix(n, "z") {
		return n + "'"
	}
	return n + "s"
}

// IsName: a spoken word is the assistant's name (or a word of it) or the built-in wake word's
// "jarvis", so speech filters drop it like a filler.
func IsName(word string) bool {
	word = strings.ToLower(strings.Trim(word, ".,!?;:"))
	if word == "" {
		return false
	}
	if word == "jarvis" {
		return true
	}
	for _, w := range strings.Fields(strings.ToLower(Name())) {
		if w == word {
			return true
		}
	}
	return false
}
