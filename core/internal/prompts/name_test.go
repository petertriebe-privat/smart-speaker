package prompts

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestSlug(t *testing.T) {
	for in, want := range map[string]string{
		"Jarvis": "jarvis", "Frau Müller": "frau_mueller", "HAL 9000": "hal_9000", " !! ": "assistant", "R2-D2": "r2_d2",
	} {
		if got := slug(in); got != want {
			t.Errorf("slug(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestNameFilledIn(t *testing.T) {
	defer SetName(Name())
	SetName("Friday")
	text := Text("DELEGATION_INSTRUCTIONS")
	if strings.Contains(text, placeholder) || !strings.Contains(text, "Friday") {
		t.Fatalf("name not filled in: %.120q", text)
	}
	for _, level := range List("PERSONA_LEVELS") {
		if strings.Contains(level, placeholder) {
			t.Fatal("placeholder left in PERSONA_LEVELS")
		}
	}
	if !strings.Contains(Text("PERSONA_CORE"), "You are Friday") {
		t.Fatal("persona without the name")
	}
}

func TestLoadPersona(t *testing.T) {
	saved := Text("PERSONA_CORE")
	defer func() { values["PERSONA_CORE"] = saved }()
	dir := t.TempDir()
	if err := LoadPersona(filepath.Join(dir, "missing.json")); err != nil {
		t.Fatalf("missing file: %v", err)
	}
	path := filepath.Join(dir, "persona.json")
	os.WriteFile(path, []byte(`{"PERSONA_CORE": "You are {{ASSISTANT_NAME}}, our own."}`), 0o644)
	if err := LoadPersona(path); err != nil {
		t.Fatal(err)
	}
	if got := Text("PERSONA_CORE"); got != "You are "+Name()+", our own." {
		t.Fatalf("PERSONA_CORE = %q", got)
	}
	os.WriteFile(path, []byte(`{"LIVE_INSTRUCTIONS": "x"}`), 0o644)
	if err := LoadPersona(path); err == nil {
		t.Fatal("a non-persona key was accepted")
	}
}
