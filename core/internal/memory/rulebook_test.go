package memory

import (
	"encoding/json"
	"strings"
	"testing"
)

// Rules come grouped by area in RuleAreas' order; unknown or missing areas under Other.
func TestRulesByArea(t *testing.T) {
	m := &Memory{}
	m.core.Rules = []Rule{
		{ID: 1, Text: "Keine Paketmeldungen.", Area: "hints"},
		{ID: 2, Text: "Sprich mich mit Sir an.", Area: "address"},
		{ID: 3, Text: "Etwas Altes."},
		{ID: 4, Text: "Nur für Anna.", Person: "anna", Area: "media"},
	}
	got := m.rulesByAreaLocked(func(r Rule) bool { return r.Person == "" }, func(r Rule) string { return "- " + r.Text + "\n" })
	want := "## Address and language\n- Sprich mich mit Sir an.\n## Hints and initiative\n- Keine Paketmeldungen.\n## Other\n- Etwas Altes.\n"
	if got != want {
		t.Fatalf("got:\n%s\nwant:\n%s", got, want)
	}
	if r := m.unsortedRuleLocked(); r == nil || r.ID != 3 {
		t.Fatalf("unsorted: %+v", r)
	}
}

// A learned rule is tentative and in its own section until confirmed; then it is binding.
func TestLearnedRule(t *testing.T) {
	m := &Memory{}
	m.core.NextID = 1
	id := m.LearnRule("", "hints", "Paketmeldungen nur, wenn etwas schiefgeht.", "3 von 3 Paketmeldungen ignoriert")
	if id == 0 {
		t.Fatal("not learned")
	}
	all := func(Rule) bool { return true }
	if got := m.tentativeRulesLocked(all); !strings.Contains(got, "[1] Paketmeldungen nur") ||
		!strings.Contains(got, "because: 3 von 3") {
		t.Fatalf("tentative section: %s", got)
	}
	binding := m.rulesByAreaLocked(func(r Rule) bool { return !r.Tentative() }, func(r Rule) string { return r.Text })
	if binding != "" {
		t.Fatalf("a tentative rule is binding: %s", binding)
	}
	if m.LearnRule("", "hints", "paketmeldungen nur, wenn etwas schiefgeht.", "again") != 0 {
		t.Fatal("the same rule learned twice")
	}
	m.core.Rules[0].State = "" // what confirm_rule does
	if m.tentativeRulesLocked(all) != "" || m.core.Rules[0].Origin != "learned" {
		t.Fatalf("confirmed: %+v", m.core.Rules[0])
	}
}

// The dream's schema with people and areas filled in is valid JSON and has learned_rules.
func TestDreamSchemaFilled(t *testing.T) {
	filled := strings.NewReplacer("PEOPLE", `"p1", "p2"`, "AREAS", quotedList(ruleAreaNames())).Replace(dreamSchema)
	var schema map[string]any
	if err := json.Unmarshal([]byte(filled), &schema); err != nil {
		t.Fatal(err)
	}
	if _, ok := schema["properties"].(map[string]any)["learned_rules"]; !ok || !strings.Contains(filled, `"learned_rules"]`) {
		t.Fatal("learned_rules missing or not required")
	}
}

// A changed text is sorted again; the same text keeps its area.
func TestChangedRuleIsSortedAgain(t *testing.T) {
	m := &Memory{}
	m.core.NextID = 2
	m.core.Rules = []Rule{{ID: 1, Text: "Keine Paketmeldungen.", Area: "hints"}}
	m.setRuleLocked("", "Keine Paketmeldungen.", 1)
	if m.core.Rules[0].Area != "hints" {
		t.Fatal("the same text lost its area")
	}
	m.setRuleLocked("", "Musik abends leiser.", 1)
	if m.core.Rules[0].Area != "" || !strings.Contains(m.core.Rules[0].Text, "leiser") {
		t.Fatalf("changed rule: %+v", m.core.Rules[0])
	}
}
