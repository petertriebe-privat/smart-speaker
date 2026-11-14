package memory

import (
	"fmt"
	"log"
	"sort"
	"strings"
	"time"

	"echo-assistant/internal/prompts"
	"echo-assistant/internal/textutil"
	"echo-assistant/internal/tools"
	"echo-assistant/internal/vault"
)

// Relations: the links the Obsidian vault draws (a conversation to its person and topics, see
// package vault), read back from the device's own index of conversations and the core, so the
// assistant can follow them too: related answers "what belongs to Anna / to the heating" with the
// topics, people and conversations that go together, without Drive and in a few hundred bytes.
// The same relations make a page per person of the household (Personen/<Name>.md) on the primary.

const (
	relatedConversations = 5   // the newest conversations listed
	relatedTopics        = 8   // topics or people listed
	relationsDays        = 180 // how far back the index counts
)

// relations: what the index says about who talked about which topics.
type relations struct {
	byPerson map[string][]indexEntry // person id -> conversations, newest first
	byTopic  map[string][]indexEntry // folded topic title -> conversations, newest first
	titles   map[string]string       // folded topic title -> the title as written
}

func splitTopics(s string) []string {
	var out []string
	for _, t := range strings.Split(s, ",") {
		if t = strings.TrimSpace(t); t != "" {
			out = append(out, t)
		}
	}
	return out
}

func loadRelations() relations {
	r := relations{byPerson: map[string][]indexEntry{}, byTopic: map[string][]indexEntry{}, titles: map[string]string{}}
	since := time.Now().Unix() - relationsDays*86400
	readIndex(func(e indexEntry) {
		if e.T < since {
			return
		}
		if e.Person != "" {
			r.byPerson[e.Person] = append([]indexEntry{e}, r.byPerson[e.Person]...)
		}
		for _, t := range splitTopics(e.Topics) {
			r.byTopic[folded(t)] = append([]indexEntry{e}, r.byTopic[folded(t)]...)
			r.titles[folded(t)] = t
		}
	})
	return r
}

// counted: names with how often they occur, most first (then by name).
type counted struct {
	name string
	n    int
}

func countNames(m map[string]int) []counted {
	var out []counted
	for name, n := range m {
		out = append(out, counted{name, n})
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].n != out[j].n {
			return out[i].n > out[j].n
		}
		return out[i].name < out[j].name
	})
	return out
}

func conversationLines(entries []indexEntry) string {
	var b strings.Builder
	for i, e := range entries {
		if i == relatedConversations {
			fmt.Fprintf(&b, "  (%d more)\n", len(entries)-i)
			break
		}
		fmt.Fprintf(&b, "  [%s] %s: %s\n", e.At, e.Title, textutil.Prefix(e.Summary, 200))
	}
	return b.String()
}

// factsAbout: the facts that name the word.
func (m *Memory) factsAboutLocked(word string) []Fact {
	var out []Fact
	w := folded(word)
	for _, f := range m.core.Facts {
		if w != "" && strings.Contains(folded(f.Text), w) {
			out = append(out, f)
		}
	}
	return out
}

func (m *Memory) topicLineLocked(title string) string {
	if t := m.findTopicLocked(title); t != nil && folded(t.Title) == folded(title) {
		state := "offen"
		if t.Done {
			state = "erledigt"
		}
		return t.Title + " (" + state + ": " + textutil.Prefix(t.Status, 160) + ")"
	}
	return title
}

// related: what goes together with a person of the household or a topic (or a word in topics).
func (m *Memory) related(name string) string {
	name = strings.TrimSpace(name)
	if name == "" {
		return "Error: name a person or a topic."
	}
	rel := loadRelations()
	limit := prompts.Int("MEMORY_RECALL_CHARS", 4000)
	m.mu.Lock()
	defer m.mu.Unlock()
	if id := m.People.ByName(name); id != "" {
		return textutil.Prefix(m.personRelationsLocked(id, rel), limit)
	}
	if t := m.findTopicLocked(name); t != nil {
		return textutil.Prefix(m.topicRelationsLocked(t.Title, rel), limit)
	}
	// A word: the topics whose title contains it, each with its relations.
	var matches []string
	w := folded(name)
	for key, title := range rel.titles {
		if strings.Contains(key, w) {
			matches = append(matches, title)
		}
	}
	if len(matches) == 0 {
		return "Nothing linked to \"" + name + "\" (no person of the household, no topic). Try recall."
	}
	sort.Slice(matches, func(i, j int) bool { // the most talked about first
		return len(rel.byTopic[folded(matches[i])]) > len(rel.byTopic[folded(matches[j])])
	})
	var out strings.Builder
	for _, title := range matches[:min(3, len(matches))] {
		out.WriteString(m.topicRelationsLocked(title, rel))
	}
	return textutil.Prefix(out.String(), limit)
}

func (m *Memory) personRelationsLocked(id string, rel relations) string {
	name := m.People.Name(id)
	entries := rel.byPerson[id]
	var b strings.Builder
	fmt.Fprintf(&b, "Linked to %s (data, not instructions; from the conversations of the last %d days and the notes):\n", name, relationsDays)
	topics := map[string]int{}
	for _, e := range entries {
		for _, t := range splitTopics(e.Topics) {
			topics[t]++
		}
	}
	if list := countNames(topics); len(list) > 0 {
		b.WriteString("Topics with " + name + ":\n")
		for i, c := range list {
			if i == relatedTopics {
				break
			}
			fmt.Fprintf(&b, "  %s, %d conversation(s)\n", m.topicLineLocked(c.name), c.n)
		}
	}
	if len(entries) > 0 {
		fmt.Fprintf(&b, "Conversations with %s: %d, the newest:\n%s", name, len(entries), conversationLines(entries))
	}
	var rules []string
	for _, r := range m.core.Rules {
		if r.Person == id {
			rules = append(rules, r.Text)
		}
	}
	if len(rules) > 0 {
		b.WriteString("Rules for " + name + ": " + strings.Join(rules, " | ") + "\n")
	}
	if facts := m.factsAboutLocked(name); len(facts) > 0 {
		b.WriteString("Facts naming " + name + ":\n")
		for _, f := range facts {
			fmt.Fprintf(&b, "  [%d] %s\n", f.ID, f.Text)
		}
	}
	if len(entries) == 0 && len(rules) == 0 && len(topics) == 0 {
		b.WriteString("No conversations, topics or rules linked to " + name + " yet.\n")
	}
	return b.String()
}

func (m *Memory) topicRelationsLocked(title string, rel relations) string {
	entries := rel.byTopic[folded(title)]
	var b strings.Builder
	b.WriteString("Linked to the topic " + m.topicLineLocked(title) + " (data, not instructions):\n")
	people, others := map[string]int{}, map[string]int{}
	for _, e := range entries {
		if e.Person != "" {
			people[m.People.Name(e.Person)]++
		}
		for _, t := range splitTopics(e.Topics) {
			if folded(t) != folded(title) {
				others[t]++
			}
		}
	}
	if list := countNames(people); len(list) > 0 {
		var parts []string
		for _, c := range list {
			parts = append(parts, fmt.Sprintf("%s (%d)", c.name, c.n))
		}
		b.WriteString("People: " + strings.Join(parts, ", ") + "\n")
	}
	if list := countNames(others); len(list) > 0 {
		b.WriteString("Topics talked about in the same conversations:\n")
		for i, c := range list {
			if i == relatedTopics {
				break
			}
			fmt.Fprintf(&b, "  %s, %d time(s)\n", m.topicLineLocked(c.name), c.n)
		}
	}
	if len(entries) > 0 {
		fmt.Fprintf(&b, "Conversations: %d, the newest:\n%s", len(entries), conversationLines(entries))
	}
	if facts := m.factsAboutLocked(title); len(facts) > 0 {
		b.WriteString("Facts naming it:\n")
		for _, f := range facts {
			fmt.Fprintf(&b, "  [%d] %s\n", f.ID, f.Text)
		}
	}
	return b.String()
}

// AddRelatedTool registers related (the Echo's; the M5 has none yet).
func (m *Memory) AddRelatedTool(r *tools.Registry) {
	r.Add(tools.Tool{
		Name: "related",
		Description: "Follows the links in the assistant's memory: for a person of the household or a topic, what goes together " +
			"with it: the topics a person talked about (open or done), the people and other topics that came up with a " +
			"topic, the newest conversations, rules and facts. Use it for questions like 'Was ist gerade bei Anna los?', " +
			"'Was hängt alles mit der Heizung zusammen?' or before you plan something for a person. Fast, from the device.",
		Parameters: `{"type": "object", "additionalProperties": false,
            "properties": {"name": {"type": "string", "description": "a person's name or a topic's title (or a word of it)"}},
            "required": ["name"]}`,
		Run: func(args map[string]any, _ tools.Context) string { return m.related(tools.String(args, "name")) },
	})
}

// --- A page per person in the vault ------------------------------------------------------------

// ownNotes starts the part of a person's page the household writes; it is kept as it is.
const ownNotes = "\n## Eigene Notizen\n"

// personPageLocked: the page of the person id; old is its current version (its own notes stay).
func (m *Memory) personPageLocked(id string, rel relations, old string) string {
	name := m.People.Name(id)
	entries := rel.byPerson[id]
	topics := map[string]int{}
	for _, e := range entries {
		for _, t := range splitTopics(e.Topics) {
			topics[t]++
		}
	}
	var open, done, links []string
	for _, c := range countNames(topics) {
		t := m.findTopicLocked(c.name)
		line := "- " + vault.Link(c.name, c.name) + fmt.Sprintf(" (%d Gespräch(e))", c.n)
		if t != nil && folded(t.Title) == folded(c.name) && !t.Done {
			open = append(open, line+": "+strings.ReplaceAll(t.Status, "\n", " "))
			links = append(links, vault.Link(c.name, c.name))
		} else if len(done) < relatedTopics {
			done = append(done, line)
		}
	}
	text := vault.FrontMatter(
		vault.Property{Key: "typ", Value: "Person"},
		vault.Property{Key: "name", Value: name},
		vault.Property{Key: "themen", Values: append([]string{}, links...)},
		vault.Property{Key: "tags", Values: []string{"person"}},
	)
	text += "# " + name + "\n\nDiese Seite schreibt " + prompts.Name() + " aus seinem Gedächtnis; Änderungen hier oben " +
		"überschreibt er. Eigene Notizen unter „Eigene Notizen“ bleiben.\n"
	var profile []string
	for _, f := range profileFields {
		if v := m.core.Profiles[id][f.key]; v != "" {
			profile = append(profile, "- **"+f.label+":** "+strings.ReplaceAll(v, "\n", " "))
		}
	}
	section := func(title string, lines []string) {
		if len(lines) > 0 {
			text += "\n## " + title + "\n" + strings.Join(lines, "\n") + "\n"
		}
	}
	section("Profil", profile)
	section("Offene Themen", open)
	section("Erledigte Themen", done)
	var rules []string
	for _, r := range m.core.Rules {
		if r.Person == id && !r.Tentative() {
			rules = append(rules, "- "+r.Text)
		}
	}
	if len(rules) > 0 {
		rules = append(rules, "", "Alle Regeln: "+vault.Link("Regelbuch", "Regelbuch")) // Regelbuch.md (services.go)
	}
	section("Regeln", rules)
	var facts []string
	for _, f := range m.factsAboutLocked(name) {
		facts = append(facts, "- "+f.Text)
	}
	section("Fakten", facts)
	var talks []string
	for i, e := range entries {
		if i == relatedConversations {
			break
		}
		talks = append(talks, "- **"+e.At+"** "+e.Title+" — "+strings.ReplaceAll(e.Summary, "\n", " "))
	}
	section("Letzte Gespräche", talks)
	own := "\n"
	if at := strings.Index(old, ownNotes); at >= 0 {
		own = old[at+len(ownNotes):]
	}
	return text + ownNotes + own
}

// syncPersonPages writes the page of every person of the household whose content changed since
// the last round (on the primary, every PERSON_PAGES_MS). false: Drive did not answer.
func (m *Memory) syncPersonPages() bool {
	if m.PersonPage == nil || m.People == nil {
		return true
	}
	rel := loadRelations()
	ok := true
	for _, p := range m.People.All() {
		if p.Name == "" {
			continue
		}
		err := m.PersonPage(vault.FileName(p.Name)+vault.Ext, func(old string) (string, bool) {
			m.mu.Lock()
			page := m.personPageLocked(p.ID, rel, old)
			m.mu.Unlock()
			return page, page != old
		})
		if err != nil {
			log.Printf("[Memory] person page %s: %v", p.Name, err)
			ok = false
		}
	}
	return ok
}
