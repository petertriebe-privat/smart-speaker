package memory

import (
	"context"
	"fmt"
	"log"
	"strings"
	"time"

	"echo-assistant/internal/openai"
	"echo-assistant/internal/prompts"
	"echo-assistant/internal/textutil"
	"echo-assistant/internal/tools"
)

// The rulebook (#82, step 5): each behaviour rule belongs to an area, so the rules come grouped
// into the instructions (and later only the areas a conversation needs). A rule without an area
// (said before, made by the consolidation, the dream or the M5, which does not know areas) is
// sorted in the background on the primary with one Decisions question each. The area is an
// optional key of the rule in core.json: the M5 keeps it as it is.

// RuleAreas: the areas, with what belongs there (for the sorting and the tool).
// German is the heading in the household's Markdown view (rulebook_md.go).
var RuleAreas = []struct{ Name, Title, German, About string }{
	{"address", "Address and language", "Ansprache und Sprache", "how the assistant talks and behaves in conversation: forms of address, tone, humour and jokes, fillers and pauses, length, language, numbers and units"},
	{"hints", "Hints and initiative", "Hinweise und Initiative", "unasked hints, reminders, suggestions, briefings, how often and when the assistant speaks up itself"},
	{"media", "Music and video", "Musik und Video", "music, radio, podcasts, TV and video"},
	{"phone", "Calls and messages", "Anrufe und Nachrichten", "phone calls, Telegram, WhatsApp, e-mail the assistant sends"},
	{"home", "Home", "Zuhause", "lights, devices, Home Assistant, the speakers and rooms"},
	{"plans", "Calendar, tasks and mail", "Kalender, Aufgaben und Mail", "appointments, to-dos, tasks, the watched mailboxes and matters"},
	{"other", "Other", "Sonstiges", "everything else"},
}

// ruleChars: the longest rule in bytes on the Echo. The M5's RULE_CHARS (240) is for its RAM and
// prompt; the Echo has room for three or four sentences (the owner's wish, 25.10.2026: a 310-character
// rule did not fit). The M5 still reads longer rules from the core.
func ruleChars() int { return prompts.Int("RULE_CHARS_ECHO", 600) }

func ruleAreaNames() []string {
	out := make([]string, len(RuleAreas))
	for i, a := range RuleAreas {
		out[i] = a.Name
	}
	return out
}

// ruleAreaHelp: "address = how Jarvis speaks ...; hints = ..." for save_rule's description.
func ruleAreaHelp() string {
	parts := make([]string, len(RuleAreas))
	for i, a := range RuleAreas {
		parts[i] = a.Name + " = " + a.About
	}
	return strings.Join(parts, "; ")
}

func ruleArea(name string) (title string, ok bool) {
	for _, a := range RuleAreas {
		if a.Name == name {
			return a.Title, true
		}
	}
	return "", false
}

// rulesByAreaLocked: the lines of the rules that hold for `with`, grouped by area in RuleAreas'
// order; unsorted ones under "Other". line formats one rule.
func (m *Memory) rulesByAreaLocked(keep func(Rule) bool, line func(Rule) string) string {
	groups := map[string][]string{}
	for _, r := range m.core.Rules {
		if !keep(r) {
			continue
		}
		area := r.Area
		if _, ok := ruleArea(area); !ok {
			area = "other"
		}
		groups[area] = append(groups[area], line(r))
	}
	var b strings.Builder
	for _, a := range RuleAreas {
		if lines := groups[a.Name]; len(lines) > 0 {
			b.WriteString("## " + a.Title + "\n" + strings.Join(lines, ""))
		}
	}
	return b.String()
}

// tentativeRulesLocked: the learned rules not confirmed yet that hold for the reader, as their
// own section: not binding, Jarvis asks once whether to keep them; "" without any.
func (m *Memory) tentativeRulesLocked(holds func(Rule) bool) string {
	var b strings.Builder
	for _, r := range m.core.Rules {
		if !r.Tentative() || !holds(r) {
			continue
		}
		fmt.Fprintf(&b, "- [%d] %s", r.ID, r.Text)
		if r.Evidence != "" {
			b.WriteString(" (because: " + r.Evidence + ")")
		}
		b.WriteString("\n")
	}
	if b.Len() == 0 {
		return ""
	}
	return "\n" + prompts.Text("RULES_TENTATIVE") + "\n" + b.String()
}

// LearnRule: a rule the assistant derived itself (the dream, from the user's reactions and
// corrections), tentative until the user confirms it; its id, 0 when the rulebook is full or the
// text is empty.
func (m *Memory) LearnRule(person, area, text, evidence string) int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.learnRuleLocked(person, area, text, evidence)
}

func (m *Memory) learnRuleLocked(person, area, text, evidence string) int {
	text = strings.TrimSpace(text)
	if text == "" || len(text) > ruleChars() {
		return 0
	}
	for _, r := range m.core.Rules { // the same rule once (a dream may find it again)
		if r.Person == person && strings.EqualFold(r.Text, text) {
			return 0
		}
	}
	id := m.setRuleLocked(person, text, 0)
	if id == 0 {
		return 0
	}
	r := m.findRuleLocked(id)
	r.Origin, r.State, r.Evidence = "learned", "tentative", textutil.Prefix(evidence, 200)
	if _, ok := ruleArea(area); ok {
		r.Area = area
	}
	m.saveLocked()
	log.Printf("[Rules] learned %d (tentative): %s", id, text)
	return id
}

// confirmRule: the user keeps a learned rule: it becomes binding.
func (m *Memory) confirmRule(id int, ctx tools.Context) string {
	m.mu.Lock()
	defer m.mu.Unlock()
	r := m.findRuleLocked(id)
	if r == nil {
		return fmt.Sprintf("Error: no rule %d", id)
	}
	if r.Person != m.speaker(ctx.Person) && !m.mayRuleAll(ctx.Person) {
		return fmt.Sprintf("Error: rule %d belongs to someone else", id)
	}
	if !r.Tentative() {
		return fmt.Sprintf("Rule %d is binding already.", id)
	}
	r.State, r.Date = "", today()
	m.saveLocked()
	log.Printf("[Rules] confirmed %d: %s", id, r.Text)
	return "Rule confirmed, binding from now on: " + r.Text
}

// unsortedRuleLocked: the first rule without a (known) area, nil if none.
func (m *Memory) unsortedRuleLocked() *Rule {
	for i := range m.core.Rules {
		if _, ok := ruleArea(m.core.Rules[i].Area); !ok {
			return &m.core.Rules[i]
		}
	}
	return nil
}

func (m *Memory) hasUnsortedRule() bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.unsortedRuleLocked() != nil
}

// sortRules: one rule without an area gets one (Decisions API); false: the API did not answer.
func (m *Memory) sortRules() bool {
	m.mu.Lock()
	r := m.unsortedRuleLocked()
	if r == nil {
		m.mu.Unlock()
		return true
	}
	id, text := r.ID, r.Text
	m.mu.Unlock()
	area, err := classifyRule(text)
	if err != nil {
		log.Printf("[Rules] sorting %d: %v", id, err)
		return false
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if r := m.findRuleLocked(id); r != nil && r.Text == text {
		r.Area = area
		m.saveLocked()
		log.Printf("[Rules] %d in area %s", id, area)
	}
	return true
}

func classifyRule(text string) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()
	choices := make([]map[string]string, len(RuleAreas))
	for i, a := range RuleAreas {
		choices[i] = map[string]string{"value": a.Name, "description": a.About}
	}
	var resp struct {
		Answers []struct {
			Type   string `json:"type"`
			Choice string `json:"choice"`
		} `json:"answers"`
	}
	err := openai.Post(ctx, "/v1/decisions", map[string]any{
		"model": prompts.TextOr("DECISIONS_MODEL", "gpt-6-luna"),
		"input": text,
		"questions": []map[string]any{{"type": "choice", "name": "area", "choices": choices,
			"instructions": "A behaviour rule the household gave its voice assistant the assistant. Which area of its behaviour is it about?"}},
	}, &resp)
	if err != nil {
		return "", err
	}
	if len(resp.Answers) == 0 || resp.Answers[0].Type != "choice" {
		return "", fmt.Errorf("no answer")
	}
	if _, ok := ruleArea(resp.Answers[0].Choice); !ok {
		return "other", nil
	}
	return resp.Answers[0].Choice, nil
}
