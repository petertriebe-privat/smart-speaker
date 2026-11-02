package memory

import (
	"strings"
	"testing"
)

func rulebookMemory() *Memory {
	m := &Memory{}
	m.core.NextID = 10
	m.core.Rules = []Rule{
		{ID: 1, Text: "Sprich mich mit Sir an.", Area: "address"},
		{ID: 2, Text: "Keine Paketmeldungen.", Area: "hints"},
		{ID: 3, Text: "Musik abends leiser.", Area: "media"},
		{ID: 4, Text: "Abfahrt immer sagen.", Area: "hints", Origin: "learned", State: "tentative", Evidence: "3 von 3 angenommen"},
	}
	return m
}

// The view lists the areas with ids, the tentative rules with their evidence, and parses back.
func TestRulebookMarkdownRoundtrip(t *testing.T) {
	m := rulebookMemory()
	md := m.rulebookMarkdownLocked()
	for _, want := range []string{rulebookTitle(), "## Ansprache und Sprache\n- [1] Sprich mich mit Sir an.",
		"## Hinweise und Initiative\n- [2] Keine Paketmeldungen.", "## " + rulebookTentHead() + "\n- [4] Abfahrt immer sagen. — weil: 3 von 3"} {
		if !strings.Contains(md, want) {
			t.Fatalf("missing %q in:\n%s", want, md)
		}
	}
	rules, ok := parseRulebook(md)
	if !ok || len(rules) != 4 || rules[3] != (mdRule{id: 4, text: "Abfahrt immer sagen.", area: "other", tentative: true}) {
		t.Fatalf("parsed: %v %+v", ok, rules)
	}
}

// The household's edits go into the core: changed, moved, deleted, added, confirmed; The assistant's own
// change meanwhile stays.
func TestRulebookEdits(t *testing.T) {
	m := rulebookMemory()
	written := m.rulebookMarkdownLocked()
	edited := strings.NewReplacer(
		"- [1] Sprich mich mit Sir an.", "- [1] Sprich mich mit Vornamen an.", // changed
		"- [2] Keine Paketmeldungen.\n", "", // deleted
		"## Zuhause\n", "## Zuhause\n- [3] Musik abends leiser.\n- Licht im Flur nachts gedimmt.\n", // moved, added
		"- [3] Musik abends leiser.\n\n## Anrufe", "\n## Anrufe",
		"- [4] Abfahrt immer sagen. — weil: 3 von 3 angenommen\n", "", // confirmed: moved up
		"## Kalender, Aufgaben und Mail\n", "## Kalender, Aufgaben und Mail\n- [4] Abfahrt immer sagen.\n",
	).Replace(written)
	// meanwhile the assistant learned another rule
	m.core.Rules = append(m.core.Rules, Rule{ID: 5, Text: "Neu von Jarvis.", Area: "other"})
	report := m.applyRulebookEditsLocked(written, edited)
	if r := m.findRuleLocked(1); r == nil || r.Text != "Sprich mich mit Vornamen an." || r.Area != "address" {
		t.Errorf("changed: %+v", r)
	}
	if m.findRuleLocked(2) != nil {
		t.Error("the deleted rule is still there")
	}
	if r := m.findRuleLocked(3); r == nil || r.Area != "home" {
		t.Errorf("moved: %+v", r)
	}
	if r := m.findRuleLocked(4); r == nil || r.Tentative() || r.Area != "plans" {
		t.Errorf("confirmed: %+v", r)
	}
	if m.findRuleLocked(5) == nil {
		t.Error("Jarvis' own rule of meanwhile was lost")
	}
	found := false
	for _, r := range m.core.Rules {
		found = found || (r.Text == "Licht im Flur nachts gedimmt." && r.Area == "home" && r.Origin == "")
	}
	if !found {
		t.Errorf("the new line is missing: %+v", m.core.Rules)
	}
	if len(report) < 5 {
		t.Errorf("report: %v", report)
	}
}

// A long rule the household wrote is taken over, not lost (it would vanish from the file).
func TestRulebookLongLine(t *testing.T) {
	m := rulebookMemory()
	written := m.rulebookMarkdownLocked()
	long := "- " + strings.Repeat("Eine lange Regel mit vielen Wörtern. ", 9) // ~330 characters
	edited := strings.Replace(written, "## Zuhause\n", "## Zuhause\n"+long+"\n", 1)
	m.applyRulebookEditsLocked(written, edited)
	if len(m.core.Rules) != 5 || m.core.Rules[4].Area != "home" {
		t.Fatalf("the long rule is missing: %+v", m.core.Rules)
	}
}

// A file without the title line changes nothing.
func TestRulebookBrokenFile(t *testing.T) {
	m := rulebookMemory()
	written := m.rulebookMarkdownLocked()
	m.applyRulebookEditsLocked(written, "irgendwas\n- [1] anders")
	if len(m.core.Rules) != 4 || m.findRuleLocked(1).Text != "Sprich mich mit Sir an." {
		t.Fatalf("a broken file changed the rules: %+v", m.core.Rules)
	}
}
