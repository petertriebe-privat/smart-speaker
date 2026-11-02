package memory

import (
	"echo-assistant/internal/prompts"
	"fmt"
	"log"
	"regexp"
	"strconv"
	"strings"
	"time"

	"echo-assistant/internal/store"
	"echo-assistant/internal/textutil"
)

// The rulebook as a Markdown file in the household's Drive (#82, step 6b): a view the household can
// read and edit. core.json stays the one truth: the primary writes the file from it, and at each
// sync compares the file with the version it wrote last (rulebook_written.md); only what the
// household changed there (a line changed, deleted, added, moved to another area, a tentative
// rule moved into an area = confirmed) goes into the core, so the assistant's own changes meanwhile stay.
// Then the file is written anew. Without the title line nothing is taken over (a broken file);
// Drive keeps older versions. The file is in the assistant's own folder (scope drive.file): only the
// household edits it, so it counts as their words.

const (
	rulebookWritten = "rulebook_written.md"
	rulebookLineMax = 1000 // bytes: a rule line the household wrote; longer is a mistaken paste
)

var (
	ruleLineRe = regexp.MustCompile(`^\s*[-*]\s+(?:\[(\d+)\]\s*)?(.+?)\s*$`)
	onlyForRe  = regexp.MustCompile(`\s*\(nur für ([^)]+)\)`)
	learnedRe  = regexp.MustCompile(`\s*\(gelernt\)`)
	becauseRe  = regexp.MustCompile(`\s+— weil: .*$`)
)

// RulebookMarkdown: the rulebook as the household reads it.
func (m *Memory) RulebookMarkdown() string {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.rulebookMarkdownLocked()
}

func (m *Memory) personName(id string) string {
	if m.People == nil || id == "" {
		return id
	}
	return m.People.Name(id)
}

func (m *Memory) rulebookMarkdownLocked() string {
	var b strings.Builder
	b.WriteString(rulebookTitle() + "\n\n")
	b.WriteString("Stand " + time.Now().Format("02.01.2006 15:04") + ". Diese Datei schreibt " + prompts.Name() + " aus seinem Gedächtnis. " +
		"Du kannst sie bearbeiten: eine Regel ändern, löschen, in einen anderen Bereich verschieben oder unter einem " +
		"Bereich eine neue Zeile „- Text“ schreiben (für eine Person: „(nur für Name)“ dahinter). Eine vorläufige Regel " +
		"bestätigst du, indem du sie in einen Bereich verschiebst. " + prompts.Name() + " übernimmt das beim nächsten Abgleich " +
		"(alle paar Minuten). Die Nummern in eckigen Klammern bitte nicht ändern.\n")
	line := func(r Rule) string {
		text := fmt.Sprintf("- [%d] %s", r.ID, r.Text)
		if r.Person != "" {
			text += " (nur für " + m.personName(r.Person) + ")"
		}
		if r.Origin == "learned" && !r.Tentative() {
			text += " (gelernt)"
		}
		if r.Tentative() && r.Evidence != "" {
			text += " — weil: " + r.Evidence
		}
		return text + "\n"
	}
	for _, a := range RuleAreas {
		b.WriteString("\n## " + a.German + "\n")
		for _, r := range m.core.Rules {
			area := r.Area
			if _, ok := ruleArea(area); !ok {
				area = "other"
			}
			if area == a.Name && !r.Tentative() {
				b.WriteString(line(r))
			}
		}
	}
	b.WriteString("\n## " + rulebookTentHead() + "\n")
	for _, r := range m.core.Rules {
		if r.Tentative() {
			b.WriteString(line(r))
		}
	}
	return b.String()
}

// mdRule: a rule line of the Markdown view.
type mdRule struct {
	id               int // 0 = a new line
	text, area, only string
	tentative        bool
}

// parseRulebook: the rule lines of a Markdown view; ok false without the title line.
func parseRulebook(md string) (rules []mdRule, ok bool) {
	area, tentative := "", false
	for _, raw := range strings.Split(strings.ReplaceAll(md, "\r", ""), "\n") {
		trimmed := strings.TrimSpace(raw)
		if trimmed == rulebookTitle() {
			ok = true
			continue
		}
		if strings.HasPrefix(trimmed, "## ") {
			heading := strings.TrimSpace(strings.TrimPrefix(trimmed, "## "))
			area, tentative = "other", strings.HasPrefix(heading, "Vorläufig")
			for _, a := range RuleAreas {
				if strings.EqualFold(heading, a.German) || strings.EqualFold(heading, a.Title) {
					area = a.Name
				}
			}
			continue
		}
		g := ruleLineRe.FindStringSubmatch(raw)
		if g == nil || area == "" {
			continue
		}
		r := mdRule{area: area, tentative: tentative, text: g[2]}
		r.id, _ = strconv.Atoi(g[1])
		if only := onlyForRe.FindStringSubmatch(r.text); only != nil {
			r.only = strings.TrimSpace(only[1])
		}
		r.text = strings.TrimSpace(becauseRe.ReplaceAllString(learnedRe.ReplaceAllString(onlyForRe.ReplaceAllString(r.text, ""), ""), ""))
		if r.text != "" {
			rules = append(rules, r)
		}
	}
	return rules, ok
}

// applyRulebookEdits: what the household changed in edited compared with written (the version
// The assistant wrote last) goes into the core; the report lines (German, for the log and the diary).
func (m *Memory) applyRulebookEditsLocked(written, edited string) []string {
	before, okBefore := parseRulebook(written)
	after, okAfter := parseRulebook(edited)
	if !okAfter {
		return []string{"Regelbuch ohne Titelzeile: nichts übernommen"}
	}
	old := map[int]mdRule{}
	if okBefore {
		for _, r := range before {
			if r.id != 0 {
				old[r.id] = r
			}
		}
	}
	var report []string
	seen := map[int]bool{}
	for _, e := range after {
		if e.id == 0 || old[e.id] == (mdRule{}) {
			if e.id != 0 && m.findRuleLocked(e.id) != nil {
				continue // in the core but not in the written view: The assistant's own, keep as is
			}
			person := ""
			if e.only != "" && m.People != nil {
				person = m.People.ByName(e.only)
			}
			// The household's own words are taken over even when longer than RULE_CHARS (a line
			// left out would vanish when the file is written anew, 25.10.2026); the dream may
			// condense it. Only a runaway paste is refused.
			if len(e.text) > rulebookLineMax {
				report = append(report, "zu lang, nicht übernommen: "+textutil.Prefix(e.text, 80))
				continue
			}
			if id := m.setRuleLocked(person, e.text, 0); id != 0 {
				m.findRuleLocked(id).Area = e.area
				report = append(report, fmt.Sprintf("neu [%d] %s", id, e.text))
			}
			continue
		}
		seen[e.id] = true
		was, r := old[e.id], m.findRuleLocked(e.id)
		if r == nil || (was.text == e.text && was.area == e.area && was.tentative == e.tentative) {
			continue
		}
		if was.text != e.text && len(e.text) <= rulebookLineMax {
			area := r.Area
			m.setRuleLocked(r.Person, e.text, e.id)
			r = m.findRuleLocked(e.id)
			r.Area = area // the section it is under, not sorted anew
			report = append(report, fmt.Sprintf("geändert [%d] %s", e.id, e.text))
		}
		if was.area != e.area && !e.tentative {
			r.Area = e.area
			report = append(report, fmt.Sprintf("Bereich [%d] %s", e.id, e.area))
		}
		if was.tentative && !e.tentative && r.Tentative() {
			r.State, r.Date = "", today()
			report = append(report, fmt.Sprintf("bestätigt [%d] %s", e.id, r.Text))
		}
	}
	for id, r := range old {
		if !seen[id] && m.findRuleLocked(id) != nil {
			m.removeRuleLocked(id)
			report = append(report, fmt.Sprintf("gelöscht [%d] %s", id, r.text))
		}
	}
	return report
}

// syncRulebook: the household's edits in the Drive file into the core, then the file anew when it
// differs. false: Drive did not answer (try again later).
func (m *Memory) syncRulebook() bool {
	if m.RulebookLoad == nil || m.RulebookStore == nil {
		return true
	}
	edited, found, err := m.RulebookLoad()
	if err != nil {
		log.Printf("[Rulebook] loading: %v", err)
		return false
	}
	written, _ := store.Read(rulebookWritten)
	m.mu.Lock()
	if found && len(written) > 0 && string(edited) != string(written) {
		if report := m.applyRulebookEditsLocked(string(written), string(edited)); len(report) > 0 {
			m.saveLocked()
			log.Printf("[Rulebook] taken over from the file: %s", strings.Join(report, "; "))
		}
	}
	md := m.rulebookMarkdownLocked()
	m.mu.Unlock()
	if found && sameBody(string(edited), md) {
		if len(written) == 0 { // the file is there, the kept version was lost: keep it now
			store.Write(rulebookWritten, edited)
		}
		return true
	}
	if err := m.RulebookStore([]byte(md)); err != nil {
		log.Printf("[Rulebook] writing: %v", err)
		return false
	}
	if err := store.Write(rulebookWritten, []byte(md)); err != nil {
		log.Printf("[Rulebook] keeping the written version: %v", err)
	}
	return true
}

// sameBody: the same view apart from the date line.
func sameBody(a, b string) bool {
	strip := func(s string) string {
		var out []string
		for _, l := range strings.Split(strings.ReplaceAll(s, "\r", ""), "\n") {
			if !strings.HasPrefix(l, "Stand ") {
				out = append(out, l)
			}
		}
		return strings.Join(out, "\n")
	}
	return strip(a) == strip(b)
}

// The rulebook's title and the heading of the rules not confirmed yet, named after the assistant.
func rulebookTitle() string { return "# " + prompts.Genitive() + " Regelbuch" }
func rulebookTentHead() string {
	return "Vorläufig: von " + prompts.Name() + " gelernt, noch nicht bestätigt"
}
