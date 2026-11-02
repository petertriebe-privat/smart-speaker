package memory

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"strings"
	"time"

	"echo-assistant/internal/openai"
	"echo-assistant/internal/prompts"
	"echo-assistant/internal/store"
	"echo-assistant/internal/textutil"
)

const (
	consolidationAttempts = 3
	consolidationRetry    = 60 * time.Second
)

// updateSchema: output of the consolidation request (Structured Outputs); PEOPLE becomes the ids.
const updateSchema = `{
  "type": "object", "additionalProperties": false,
  "required": ["speaker", "profile", "asked_profile", "add", "update", "remove", "rules", "title", "summary",
               "minor", "keywords", "topics", "affairs"],
  "properties": {
    "speaker": {"type": "string", "enum": [PEOPLE, ""]},
    "profile": {"type": "array", "items": {"type": "object", "additionalProperties": false,
      "required": ["person", "field", "value"],
      "properties": {"person": {"type": "string", "enum": [PEOPLE]}, "field": {"$ref": "#/$defs/field"},
                     "value": {"type": "string"}}}},
    "asked_profile": {"type": "boolean"},
    "add": {"type": "array", "items": {"type": "object", "additionalProperties": false,
      "required": ["category", "text"],
      "properties": {"category": {"$ref": "#/$defs/category"}, "text": {"type": "string"}}}},
    "update": {"type": "array", "items": {"type": "object", "additionalProperties": false,
      "required": ["id", "category", "text"],
      "properties": {"id": {"type": "integer"}, "category": {"$ref": "#/$defs/category"},
                     "text": {"type": "string"}}}},
    "remove": {"type": "array", "items": {"type": "integer"}},
    "rules": {"type": "array", "items": {"type": "object", "additionalProperties": false,
      "required": ["person", "text", "replaces"],
      "properties": {"person": {"type": "string", "enum": [PEOPLE, ""]}, "text": {"type": "string"},
                     "replaces": {"type": "integer"}}}},
    "title": {"type": "string"},
    "summary": {"type": "string"},
    "minor": {"type": "boolean"},
    "keywords": {"type": "array", "items": {"type": "string"}},
    "topics": {"type": "array", "items": {"type": "object", "additionalProperties": false,
      "required": ["title", "status", "keywords", "notes", "done"],
      "properties": {"title": {"type": "string"}, "status": {"type": "string"},
                     "keywords": {"type": "array", "items": {"type": "string"}},
                     "notes": {"type": "string"}, "done": {"type": "boolean"}}}},
    "affairs": {"type": "array", "items": {"type": "object", "additionalProperties": false,
      "required": ["id", "person", "kind", "title", "due", "state", "parent", "reason"],
      "properties": {"id": {"type": "string"}, "person": {"type": "string", "enum": [PEOPLE, ""]},
                     "kind": {"type": "string", "enum": ["appointment", "deadline", "todo", "promise", "waiting",
                                                         "delivery", "trip", "other"]},
                     "title": {"type": "string"}, "due": {"type": "string"}, "parent": {"type": "string"},
                     "state": {"type": "string", "enum": ["open", "waiting", "done", "obsolete"]},
                     "reason": {"type": "string"}}}}
  },
  "$defs": {"category": {"type": "string",
    "enum": ["Person", "Vorliebe", "Haushalt", "Termin", "Sonstiges"]},
    "field": {"type": "string",
    "enum": ["name", "home", "birthday", "family", "pets", "work", "interests", "wishes"]}}
}`

type update struct {
	Speaker string `json:"speaker"`
	Profile []struct {
		Person, Field, Value string
	} `json:"profile"`
	AskedProfile bool `json:"asked_profile"`
	Add          []struct {
		Category, Text string
	} `json:"add"`
	Update []struct {
		ID             int
		Category, Text string
	} `json:"update"`
	Remove []int `json:"remove"`
	Rules  []struct {
		Person   string
		Text     string
		Replaces int
	} `json:"rules"`
	Title    string   `json:"title"`
	Summary  string   `json:"summary"`
	Minor    bool     `json:"minor"`
	Keywords []string `json:"keywords"`
	Topics   []struct {
		Title    string
		Status   string
		Keywords []string
		Notes    string
		Done     bool
	} `json:"topics"`
	Affairs []AffairChange `json:"affairs"`
}

// AffairChange: an affair the consolidation found in a conversation (#71): ID "" = a new one.
type AffairChange struct {
	ID     string `json:"id"`
	Person string `json:"person"`
	Kind   string `json:"kind"`
	Title  string `json:"title"`
	Due    string `json:"due"`
	State  string `json:"state"`
	Parent string `json:"parent"`
	Reason string `json:"reason"`
}

type conversation struct {
	transcript string
	started    int64
	person     string // recognized for sure, "" = unknown
	channel    string
}

// Consolidate hands a finished conversation ("Du: ...\nJarvis: ...") with person over channel
// to the memory: it is kept in a file (a restart must not lose it) and consolidated between
// conversations (one backend request). A phone call without words becomes an event.
func (m *Memory) Consolidate(transcript string, started time.Time, person, channel string) {
	// Only conversations in which the user said something; a call without words is still a contact.
	if !strings.Contains(transcript, "Du") && !strings.Contains(transcript, "Gegenüber:") {
		switch channel {
		case PhoneIn, Guest:
			m.LogEvent(person, channel, "Anruf ohne Gespräch", "Niemand sagte etwas.")
		case PhoneOut:
			m.LogEvent(person, channel, "Anruf von "+prompts.Name()+" ohne Gespräch", "Niemand sagte etwas.")
		}
		return
	}
	name := fmt.Sprintf("%s/%010d", pendingDir, started.Unix())
	if person != "" {
		name += "-" + person
	}
	header := ""
	if channel != "" {
		header = channelHead + channel + "\n"
	}
	if err := store.Write(name+".txt", []byte(header+transcript)); err != nil {
		log.Printf("[Memory] conversation could not be saved: %v", err)
		return
	}
	m.kick()
}

// LogEvent: a contact without a conversation for the activity log, straight into the index and
// the recent conversations.
func (m *Memory) LogEvent(person, channel, title, summary string) {
	now := time.Now()
	at := now.Format("2006-01-02 15:04")
	m.addToIndex(indexEntry{At: at, T: now.Unix(), Title: title, Summary: summary, Person: person, Channel: channel})
	m.mu.Lock()
	e := Recent{At: at, T: now.Unix(), Title: title, Summary: summary, Channel: channel, Minor: true}
	if person != "" {
		e.With = m.People.Name(person)
	}
	m.core.Recent = append(m.core.Recent, e)
	m.pruneRecentLocked()
	m.saveLocked()
	m.mu.Unlock()
	log.Printf("[Memory] event: %s, %s: %s", person, channel, title)
}

// SetIdle: background work (consolidation, archive, backup) runs only while no conversation is
// open.
func (m *Memory) SetIdle(on bool) {
	m.mu.Lock()
	m.idle = on
	m.mu.Unlock()
	if on {
		m.kick()
	}
}

// RetryNow: what failed for want of the archive or backup (Google login) is tried again at once.
func (m *Memory) RetryNow() {
	m.mu.Lock()
	m.restoreRetry = time.Time{}
	m.mu.Unlock()
	m.kick()
}

func (m *Memory) kick() {
	select {
	case m.wake <- struct{}{}:
	default:
	}
}

// Run: the memory's background work, as the M5's memory task: restore first, then new
// memories, then the archive and the backup. Start as a goroutine.
func (m *Memory) Run() {
	retry := time.Duration(prompts.Int("MEMORY_OUTBOX_RETRY_MS", 300000)) * time.Millisecond
	var outboxRetry time.Time
	for {
		select {
		case <-m.wake:
		case <-time.After(time.Minute):
		}
		for {
			m.mu.Lock()
			idle, restore, backupDue := m.idle, m.restorePending, m.backupPending
			retryAt := m.retryAt
			m.mu.Unlock()
			if !idle {
				break
			}
			now := time.Now()
			switch {
			case restore && now.After(m.restoreRetry):
				if !m.restore() {
					m.restoreRetry = now.Add(retry)
				}
				continue
			case len(store.List(pendingDir)) > 0 && now.After(retryAt):
				m.processPending()
				continue
			// DREAM HOOK (dream.go): after the conversations, so the dream sees them consolidated.
			case m.dreamDue():
				m.dream()
				continue
			// END DREAM HOOK
			case m.archiveWritable() && m.learnReportDue(): // the primary only (learnreport.go)
				m.sendLearnReport()
				continue
			case m.archiveWritable() && now.After(m.ruleSortRetry) && m.hasUnsortedRule():
				if !m.sortRules() { // the rulebook's areas (rulebook.go)
					m.ruleSortRetry = now.Add(retry)
				}
				continue
			case m.archiveWritable() && !m.hasUnsortedRule() && now.After(m.rulebookNext):
				// the rulebook's Markdown file in Drive (rulebook_md.go), after the sorting
				m.rulebookNext = now.Add(time.Duration(prompts.Int("RULEBOOK_SYNC_MS", 600000)) * time.Millisecond)
				if !m.syncRulebook() {
					m.rulebookNext = now.Add(retry)
				}
				continue
			case len(store.List(outboxDir)) > 0 && m.archiveUsable() && m.archiveWritable() && now.After(outboxRetry):
				if !m.flushOutbox() {
					outboxRetry = now.Add(retry)
				}
				continue
			case backupDue && !restore:
				m.backupCore()
			}
			break
		}
	}
}

// ArchiveWritable: whether this device may write the archive (only as the primary device:
// the M5's dossiers must not be replaced by the Echo's while both run). Set by the caller.
var ArchiveWritable = func() bool { return false }

func (m *Memory) archiveWritable() bool { return ArchiveWritable() }

type outcome int

const (
	done outcome = iota
	retryLater
	failed
)

// processPending consolidates the oldest pending conversation.
func (m *Memory) processPending() {
	names := store.List(pendingDir)
	if len(names) == 0 {
		return
	}
	name := names[0]
	data, _ := store.Read(pendingDir + "/" + name)
	started, person := parsePending(name)
	transcript, channel := string(data), ""
	if strings.HasPrefix(transcript, channelHead) {
		head, rest, _ := strings.Cut(transcript, "\n")
		channel, transcript = head[len(channelHead):], rest
	}
	c := conversation{transcript, started, person, channel}
	result := done
	if transcript != "" {
		result = m.consolidateNow(c)
	}
	if result == retryLater {
		m.mu.Lock()
		m.attempts++
		if m.attempts < consolidationAttempts {
			m.retryAt = time.Now().Add(consolidationRetry)
			m.mu.Unlock()
			return
		}
		m.mu.Unlock()
	}
	if result != done {
		// Keep the transcript at least (in the archive, findable by full-text search).
		at := time.Unix(started, 0).Format("2006-01-02 15:04")
		m.queueNote(note{Kind: "conversation", Key: at, Subject: "Gespräch " + at + " – nicht zusammengefasst",
			Text: "Gespräch vom " + at + " (nicht zusammengefasst)\n\nTranskript:\n" + transcript})
	}
	store.Remove(pendingDir + "/" + name)
	m.mu.Lock()
	m.attempts = 0
	m.mu.Unlock()
}

// consolidateNow: one Responses request with Structured Outputs.
func (m *Memory) consolidateNow(c conversation) outcome {
	m.mu.Lock()
	existing := m.contextLocked(true, "", true)
	// All topics on the device with their status, so the model continues them by name.
	topics := ""
	for _, t := range m.core.Topics {
		topics += "- " + t.Title + ": " + t.Status + "\n"
	}
	if topics != "" {
		existing += "All known topics:\n" + topics
	}
	if rules := m.rulesListLocked(); rules != "" {
		existing += "The assistant's behaviour rules:\n" + rules
	}
	m.mu.Unlock()
	if existing == "" {
		existing = "(empty)\n"
	}
	if m.Affairs != nil && !outsider(c.channel) {
		open := m.Affairs()
		if open == "" {
			open = "(none)"
		}
		existing += "Open affairs (id: kind, title, due, [person]):\n" + open + "\n"
	}
	with := ""
	if !outsider(c.channel) {
		with = m.speaker(c.person)
	}
	partner := "unknown (at home; may become clear from the conversation)"
	switch {
	case with != "":
		partner = m.People.Name(with) + " (id " + with + ", recognized for sure)"
	case outsider(c.channel):
		partner = "someone outside the household (see the transcript)"
	}
	input := "Memory so far:\n" + existing
	if label := channelLabel(c.channel); label != "" {
		input += "\nChannel: " + label
	}
	input += "\nConversation partner: " + partner + "\nTranscript of the conversation of " +
		time.Unix(c.started, 0).Format("02.01.2006 15:04") + ":\n" + c.transcript
	var ids []string
	for _, p := range m.People.All() {
		ids = append(ids, `"`+p.ID+`"`)
	}
	var schema any
	if err := json.Unmarshal([]byte(strings.ReplaceAll(updateSchema, "PEOPLE", strings.Join(ids, ", "))), &schema); err != nil {
		log.Printf("[Memory] schema invalid: %v", err)
		return failed
	}
	timeContext := ""
	if m.TimeContext != nil {
		timeContext = m.TimeContext()
	}
	started := time.Now()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	resp, err := openai.Responses(ctx, map[string]any{
		"model":        prompts.Text("DELEGATION_MODEL"),
		"instructions": prompts.Text("MEMORY_INSTRUCTIONS") + "\n" + timeContext,
		"input":        textutil.Repair(input),
		"store":        false,
		"text": map[string]any{"format": map[string]any{"type": "json_schema", "name": "memory_update",
			"strict": true, "schema": schema}},
	})
	if err != nil {
		log.Printf("[Memory] summarizing failed: %v", err)
		msg := err.Error()
		if strings.HasPrefix(msg, "HTTP 4") && !strings.HasPrefix(msg, "HTTP 429") {
			return failed // a bad request stays bad
		}
		return retryLater
	}
	var u update
	if err := json.Unmarshal([]byte(resp.Text()), &u); err != nil {
		log.Print("[Memory] answer not readable")
		return failed
	}
	log.Printf("[Memory] %d ms, tokens in %d, out %d", time.Since(started).Milliseconds(),
		resp.Usage.InputTokens, resp.Usage.OutputTokens)
	m.apply(u, c)
	return done
}

func (m *Memory) applyTopicLocked(title, status string, keywords []string, notes string, isDone bool, at, heading string, touch bool) {
	if title == "" {
		return
	}
	t := m.findTopicLocked(title)
	if t == nil {
		base := textutil.Slug(title, 40)
		if base == "" {
			base = "thema"
		}
		key := base
		for n := 2; ; n++ {
			taken := false
			for _, o := range m.core.Topics {
				taken = taken || o.Key == key
			}
			if !taken {
				break
			}
			key = fmt.Sprintf("%s-%d", base, n)
		}
		m.core.Topics = append(m.core.Topics, Topic{Key: key, Title: title, Created: today()})
		t = &m.core.Topics[len(m.core.Topics)-1]
	}
	if touch || status != t.Status {
		t.Updated = today()
	}
	t.Status = status
	t.Keywords = strings.Join(nonEmpty(keywords), ", ")
	if touch {
		t.T = time.Now().Unix() // recency decides which topics the prompt shows with their status
	}
	t.Done = isDone
	state := "offen"
	if t.Done {
		state = "erledigt"
	}
	header := "Thema: " + t.Title + "\nSchlüssel: " + t.Key + "\nStand (" + today() + "): " + t.Status +
		"\nStichwörter: " + t.Keywords + "\nStatus: " + state + "\nAngelegt: " + t.Created + "\n"
	entry := "--- " + at
	if heading != "" {
		entry += " (" + heading + ")"
	}
	entry += " ---\n"
	if notes != "" {
		entry += notes + "\n"
	} else {
		entry += t.Status + "\n"
	}
	m.queueNote(note{Kind: "topic", Key: t.Key, Subject: "Thema [" + t.Key + "]: " + t.Title, Text: header, Entry: entry})
}

func nonEmpty(words []string) []string {
	var out []string
	for _, w := range words {
		if w = strings.TrimSpace(w); w != "" {
			out = append(out, w)
		}
	}
	return out
}

// capTopicsLocked: topics beyond MEMORY_TOPICS: the oldest leave the device, their dossiers stay
// in the archive.
func (m *Memory) capTopicsLocked() {
	for len(m.core.Topics) > prompts.Int("MEMORY_TOPICS", 40) {
		oldest := 0
		for i, t := range m.core.Topics {
			if t.T < m.core.Topics[oldest].T {
				oldest = i
			}
		}
		log.Printf("[Memory] topic only in the archive now: %s", m.core.Topics[oldest].Title)
		m.core.Topics = append(m.core.Topics[:oldest], m.core.Topics[oldest+1:]...)
	}
}

func (m *Memory) apply(u update, c conversation) {
	// Who the assistant talked with: known for sure (phone, Telegram, mail), else what the model found.
	with := ""
	if !outsider(c.channel) {
		with = m.speaker(c.person)
		if with == "" && m.People.Name(u.Speaker) != "" {
			with = u.Speaker
		}
	}
	household := len(m.People.All()) > 1
	at := time.Unix(c.started, 0).Format("2006-01-02 15:04")
	title, summary := u.Title, u.Summary
	// Every conversation leaves a trace, also short commands: The assistant must know when it last
	// talked with the user and about what.
	if summary == "" {
		first := ""
		if i := strings.Index(c.transcript, "Du"); i >= 0 {
			rest := c.transcript[i:]
			if j := strings.IndexByte(rest, ':'); j >= 0 {
				first, _, _ = strings.Cut(rest[j+1:], "\n")
			}
		}
		summary = "Kurzes Gespräch: „" + textutil.Prefix(strings.TrimSpace(first), 160) + "“"
	}
	if title == "" {
		title = "Kurzes Gespräch"
	}
	keywords := strings.Join(nonEmpty(u.Keywords), ", ")
	m.mu.Lock()
	date := today()
	for _, p := range u.Profile {
		person := p.Person
		if m.People.Name(person) == "" {
			person = with
		}
		old := m.core.Profiles[person][p.Field]
		if m.setProfileLocked(person, p.Field, p.Value) && strings.TrimSpace(p.Value) != "" && strings.TrimSpace(p.Value) != old {
			m.noteLearnedLocked(person, "profile", 0, p.Field, strings.TrimSpace(p.Value)) // learnreport.go
		}
	}
	if u.AskedProfile && with != "" {
		m.core.ProfileAsked[with] = date
	}
	for _, id := range u.Remove {
		m.removeFactLocked(id)
	}
	for _, ch := range u.Update {
		if f := m.findFactLocked(ch.ID); f != nil && ch.Text != "" {
			f.Text, f.Category, f.Date = ch.Text, category(ch.Category), date
		}
	}
	for _, f := range u.Add {
		if f.Text != "" {
			m.noteLearnedLocked(with, "fact", m.addFactLocked(f.Category, f.Text, date), "", f.Text)
		}
	}
	m.capFactsLocked()
	// Corrections of the assistant's behaviour it did not save itself. Only from the household's own
	// words: never in calls with outsiders; for everyone or someone else only from the admin (or
	// nobody named at the speaker).
	ruleChanges := 0
	for _, r := range u.Rules {
		if outsider(c.channel) || r.Text == "" || len(r.Text) > ruleChars() {
			continue
		}
		all := with == m.People.Admin()
		if with == "" {
			all = c.channel == Speaker
		}
		owner := r.Person
		if owner != with && !all {
			owner = with
		}
		if owner == "" && !all {
			continue
		}
		replaces := 0
		if r.Replaces != 0 {
			if old := m.findRuleLocked(r.Replaces); old != nil {
				if old.Person != owner && !all {
					continue
				}
				replaces = r.Replaces
			}
		}
		if m.setRuleLocked(owner, r.Text, replaces) != 0 {
			ruleChanges++
			log.Printf("[Rules] from the conversation (%s): %s", map[bool]string{true: owner, false: "household"}[owner != ""], r.Text)
		}
	}
	var topicTitles []string
	for _, t := range u.Topics {
		m.applyTopicLocked(t.Title, t.Status, t.Keywords, t.Notes, t.Done, at, title, true)
		topicTitles = append(topicTitles, t.Title)
	}
	m.capTopicsLocked()
	e := Recent{At: at, T: c.started, Title: title, Summary: summary, Channel: c.channel, Minor: u.Minor}
	if household && with != "" {
		e.With = m.People.Name(with)
	}
	m.core.Recent = append(m.core.Recent, e)
	m.pruneRecentLocked()
	topicsText := strings.Join(topicTitles, ", ")
	label := channelLabel(c.channel)
	text := "Gespräch vom " + at + ": " + title
	if with != "" {
		text += "\nMit: " + m.People.Name(with)
	}
	if label != "" {
		text += "\nKanal: " + label
	}
	text += "\nKurzfassung: " + summary + "\nStichwörter: " + keywords + "\nThemen: " + topicsText +
		"\n\nTranskript:\n" + c.transcript
	m.queueNote(note{Kind: "conversation", Key: at, Subject: "Gespräch " + at + " – " + title, Text: text})
	m.saveLocked()
	m.mu.Unlock()
	m.addToIndex(indexEntry{At: at, T: c.started, Title: title, Summary: summary, Keywords: keywords,
		Topics: topicsText, Person: with, Channel: c.channel})
	minor := ""
	if u.Minor {
		minor = " (minor)"
	}
	log.Printf("[Memory] with %s, profile %d fields, +%d ~%d -%d facts, %d rules, topics: %s, %d affairs, conversation: %s: %s%s",
		with, len(u.Profile), len(u.Add), len(u.Update), len(u.Remove), ruleChanges, topicsText, len(u.Affairs), title, summary, minor)
	// Affairs only from the household's own conversations; whose: the one named, else the partner,
	// else the admin (at the speaker nobody named is the admin's, as the agenda).
	if m.ApplyAffairs != nil && len(u.Affairs) > 0 && !outsider(c.channel) {
		owner := with
		if owner == "" {
			owner = m.People.Admin()
		}
		m.ApplyAffairs(owner, u.Affairs, time.Unix(c.started, 0))
	}
}
