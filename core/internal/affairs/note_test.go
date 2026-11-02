package affairs

import (
	"strings"
	"testing"
)

// A mail settles a return: its to-do and its appointment are named in the note.
func TestSettledNote(t *testing.T) {
	b := book("2026-10-31 12:00")
	id, _ := b.Observe("p", Source{Kind: "calendar", Ref: "cal/1"}, "appointment", "Kühlschrank zurücksenden", "2026-11-02")
	b.Observe("p", Source{Kind: "task", Ref: "list/7"}, "todo", "Kühlschrank zurücksenden", "2026-11-02")
	got := b.Intake("p", Candidate{Match: id, Kind: "todo", State: "done", Source: mailSource("9")}, "mail")
	if got == nil || got.State != "done" {
		t.Fatalf("not settled: %+v", got)
	}
	note := b.SettledNote(*got)
	if !strings.Contains(note, "Kühlschrank zurücksenden") || !strings.Contains(note, "02.11.") ||
		!strings.Contains(note, "soll ich es abhaken?") {
		t.Fatalf("hide: %q", note)
	}
	b.SetTrust("p", "act")
	if note := b.SettledNote(*got); !strings.Contains(note, "To-do habe ich abgehakt") {
		t.Fatalf("act: %q", note)
	}
	open := Affair{Title: "x", State: "open"}
	if b.SettledNote(open) != "" {
		t.Fatal("a note for an open affair")
	}
}
