// Package memory: The assistant's long-term memory across conversations, as
// devices/atoms3r/src/assistant/memory*.cpp, tiered by age and latency:
//
//	prompt    the people's profiles, facts by category, the conversations of the last 48 h and
//	          the current topics (Context); older topics by title only
//	device    state_dir/memory: the core (core.json, the M5's format) and an index of all
//	          conversations (sessions.jsonl), searched by recall
//	archive   a dossier per topic and every conversation with its transcript (Archive, Google
//	          Drive), found by full-text search; the core's backup (package backup)
//
// Updates: the tools remember / update_profile / forget, after every conversation the backend
// model's consolidation (Consolidate), behaviour rules and skills.
package memory

import (
	"encoding/json"
	"log"
	"sort"
	"strings"
	"sync"
	"time"

	"echo-assistant/internal/people"
	"echo-assistant/internal/prompts"
	"echo-assistant/internal/store"
)

const (
	coreFile    = "memory/core.json"
	indexFile   = "memory/sessions.jsonl" // one line per conversation
	pendingDir  = "memory/pending"        // conversations not yet consolidated
	outboxDir   = "memory/outbox"         // notes waiting for the archive
	coreBackup  = "core.json"             // in Drive
	coreSubject = "Gedächtnis (Kern)"
	channelHead = "#kanal " // first line of a pending conversation file
)

// Channels of the activity log: how a contact came in.
const (
	Speaker    = "speaker"   // at home, this Echo
	PhoneIn    = "phone_in"  // someone called the assistant
	PhoneOut   = "phone_out" // The assistant called a person of the household
	Telegram   = "telegram"
	WhatsApp   = "whatsapp"
	Email      = "email"
	CallFor    = "call_for"   // The assistant called someone else for the user
	PhoneSelf  = "phone_self" // the user called someone themselves over the speaker (dial_for_me)
	Guest      = "guest"      // a number not released called
	MailNotice = "mail_notice"
)

var categories = []struct{ key, heading string }{
	{"Person", "Personen"},
	{"Vorliebe", "Vorlieben und Gewohnheiten"},
	{"Haushalt", "Wohnung und Geräte"},
	{"Termin", "Termine und Pläne"},
	{"Sonstiges", "Sonstiges"},
}

// profileFields: always in the prompt, never merged away or capped like the facts.
var profileFields = []struct{ key, label string }{
	{"name", "Name"}, {"home", "Wohnort"}, {"birthday", "Geburtstag"},
	{"family", "Familie und Haushalt"}, {"pets", "Haustiere"}, {"work", "Beruf und Alltag"},
	{"interests", "Interessen und Vorlieben"}, {"wishes", "Wünsche an " + prompts.Name()},
}

type Fact struct {
	ID       int    `json:"id"`
	Category string `json:"category"`
	Text     string `json:"text"`
	Date     string `json:"date"`
}

type Topic struct {
	Key      string `json:"key"`
	Title    string `json:"title"`
	Status   string `json:"status"`
	Keywords string `json:"keywords"`
	Created  string `json:"created"`
	Updated  string `json:"updated"`
	T        int64  `json:"t"`
	Done     bool   `json:"done"`
}

type Recent struct {
	At      string `json:"at"`
	T       int64  `json:"t"`
	Title   string `json:"title"`
	Summary string `json:"summary"`
	With    string `json:"with,omitempty"`
	Channel string `json:"channel,omitempty"`
	Minor   bool   `json:"minor,omitempty"`
}

type Skill struct {
	Name         string `json:"name"`
	When         string `json:"when"`
	Instructions string `json:"instructions"`
	Updated      string `json:"updated"`
}

type Rule struct {
	ID     int    `json:"id"`
	Person string `json:"person"` // "" = household
	Text   string `json:"text"`
	Date   string `json:"date"`
	Area   string `json:"area,omitempty"` // RuleAreas (rulebook.go); "" = not sorted yet (the M5 sets none)
	// Learned rules (rulebook.go): Jarvis derived it itself (Origin "learned"; "" = the user said
	// it), with the evidence; State "tentative" until the user confirmed it (confirm_rule).
	Origin   string `json:"origin,omitempty"`
	State    string `json:"state,omitempty"`
	Evidence string `json:"evidence,omitempty"`
}

// Tentative: a learned rule the user has not confirmed yet: not binding.
func (r Rule) Tentative() bool { return r.State == "tentative" }

// Core: the M5's core.json. Keys this version does not know (the dream) are kept as they are.
type Core struct {
	NextID       int                          `json:"next_id"`
	Profiles     map[string]map[string]string `json:"profiles"`
	ProfileAsked map[string]string            `json:"profile_asked"`
	Facts        []Fact                       `json:"facts"`
	Topics       []Topic                      `json:"topics"`
	Recent       []Recent                     `json:"recent"`
	Skills       []Skill                      `json:"skills"`
	Rules        []Rule                       `json:"rules"`
	Other        map[string]json.RawMessage   `json:"-"`
}

var knownKeys = map[string]bool{"next_id": true, "profiles": true, "profile_asked": true, "facts": true,
	"topics": true, "recent": true, "skills": true, "rules": true}

func (c *Core) UnmarshalJSON(data []byte) error {
	type plain Core
	var p plain
	if err := json.Unmarshal(data, &p); err != nil {
		return err
	}
	var all map[string]json.RawMessage
	json.Unmarshal(data, &all)
	p.Other = map[string]json.RawMessage{}
	for k, v := range all {
		if !knownKeys[k] {
			p.Other[k] = v
		}
	}
	*c = Core(p)
	return nil
}

func (c Core) MarshalJSON() ([]byte, error) {
	type plain Core
	data, err := json.Marshal(plain(c))
	if err != nil || len(c.Other) == 0 {
		return data, err
	}
	var all map[string]json.RawMessage
	json.Unmarshal(data, &all)
	for k, v := range c.Other {
		all[k] = v
	}
	return json.Marshal(all)
}

func (c *Core) normalize() {
	if c.NextID < 1 {
		c.NextID = 1
	}
	if c.Profiles == nil {
		c.Profiles = map[string]map[string]string{}
	}
	if c.ProfileAsked == nil {
		c.ProfileAsked = map[string]string{}
	}
	for _, s := range []any{&c.Facts, &c.Topics, &c.Recent, &c.Skills, &c.Rules} {
		switch v := s.(type) {
		case *[]Fact:
			if *v == nil {
				*v = []Fact{}
			}
		case *[]Topic:
			if *v == nil {
				*v = []Topic{}
			}
		case *[]Recent:
			if *v == nil {
				*v = []Recent{}
			}
		case *[]Skill:
			if *v == nil {
				*v = []Skill{}
			}
		case *[]Rule:
			if *v == nil {
				*v = []Rule{}
			}
		}
	}
	if c.Other == nil {
		c.Other = map[string]json.RawMessage{}
	}
}

func (c *Core) empty() bool {
	return len(c.Profiles) == 0 && len(c.Facts) == 0 && len(c.Topics) == 0 && len(c.Recent) == 0 &&
		len(c.Skills) == 0 && len(c.Rules) == 0
}

// Presence: where Home Assistant sees a person (services/presence on the M5); nil = unknown.
type Presence interface {
	Of(person string) (where string) // "home", "away", "" = unknown
	Text() string                    // for the prompt, "" = nothing
}

type Memory struct {
	People   *people.Register
	Presence Presence
	Archive  Archive // nil = no archive tier (dossiers, transcripts)
	// TimeContext gives the time (and place) for instructions (assistant.Context).
	TimeContext func() string
	// The household's open affairs as data for the consolidation (affairs.Book.Brief("")), and
	// the changes it finds in a conversation (#71). Both optional.
	Affairs      func() string
	ApplyAffairs func(person string, changes []AffairChange, at time.Time)
	// For the nightly review of the affairs: evidence (the sorted mails and parcels of the last
	// days) and applying its changes (returns the report lines). Both optional.
	AffairsEvidence func() string
	// Reactions: the answers to the initiative's suggestions (#82), for the dream's learned rules.
	Reactions func() string
	// OnSuggestion: a dream's offer for a person (a routine as a task, an automation) goes to the
	// initiative (#82); true = taken, so it is not put into the prompts as well. nil = the prompts.
	OnSuggestion func(person, text string) bool
	// RulebookLoad / RulebookStore: the rulebook's Markdown file in Drive (rulebook_md.go); nil =
	// no Drive, no file.
	RulebookLoad  func() ([]byte, bool, error)
	RulebookStore func(data []byte) error
	ReviewAffairs func(changes []AffairReview) string
	// ReportHour: the hour of the weekly learning report for a person from their rules
	// (prefer.Hour "learn_report"); nil = LEARN_REPORT_HOUR.
	ReportHour func(person string, start int) int

	mu             sync.Mutex
	core           Core
	ongoing        map[string]string // person -> chat not consolidated yet
	restorePending bool
	backupPending  bool
	idle           bool
	attempts       int
	retryAt        time.Time
	wake           chan struct{}
	restoreRetry   time.Time
	ruleSortRetry  time.Time // the rulebook's sorting failed: not before then
	rulebookNext   time.Time // the next sync of the rulebook's Markdown file
}

func New(p *people.Register) *Memory {
	m := &Memory{People: p, ongoing: map[string]string{}, wake: make(chan struct{}, 1)}
	m.mu.Lock()
	if !store.LoadJSON(coreFile, &m.core) {
		m.core = Core{}
	}
	m.core.normalize()
	m.restorePending = m.core.empty()
	facts, topics := len(m.core.Facts), len(m.core.Topics)
	m.mu.Unlock()
	log.Printf("[Memory] %d facts, %d topics%s", facts, topics, map[bool]string{true: ", looking for the backup"}[m.restorePending])
	return m
}

func today() string { return time.Now().Format("2006-01-02") }

// saveLocked writes the core and marks the backup due.
func (m *Memory) saveLocked() {
	if err := store.SaveJSON(coreFile, m.core); err != nil {
		log.Printf("[Memory] saving failed: %v", err)
	}
	if !m.restorePending {
		m.backupPending = true
	}
}

func category(value string) string {
	for _, c := range categories {
		if c.key == value {
			return value
		}
	}
	return "Sonstiges"
}

func validField(key string) bool {
	for _, f := range profileFields {
		if f.key == key {
			return true
		}
	}
	return false
}

// setProfileLocked: a value replaces the field of person's profile; "" clears it.
func (m *Memory) setProfileLocked(person, key, value string) bool {
	if !validField(key) || person == "" {
		return false
	}
	value = strings.TrimSpace(value)
	if value != "" {
		if m.core.Profiles[person] == nil {
			m.core.Profiles[person] = map[string]string{}
		}
		m.core.Profiles[person][key] = value
	} else if m.core.Profiles[person] != nil {
		delete(m.core.Profiles[person], key)
	}
	if len(m.core.Profiles[person]) == 0 {
		delete(m.core.Profiles, person)
	}
	return true
}

// speaker: who the assistant talks with: the person if known; with only the admin registered it is it.
func (m *Memory) speaker(person string) string {
	if person != "" {
		return person
	}
	if all := m.People.All(); len(all) == 1 {
		return all[0].ID
	}
	return ""
}

func outsider(channel string) bool { return channel == CallFor || channel == Guest }

func (m *Memory) addFactLocked(cat, text, date string) int {
	id := m.core.NextID
	m.core.NextID++
	m.core.Facts = append(m.core.Facts, Fact{ID: id, Category: category(cat), Text: text, Date: date})
	return id
}

func (m *Memory) findFactLocked(id int) *Fact {
	for i := range m.core.Facts {
		if m.core.Facts[i].ID == id {
			return &m.core.Facts[i]
		}
	}
	return nil
}

func (m *Memory) removeFactLocked(id int) bool {
	for i := range m.core.Facts {
		if m.core.Facts[i].ID == id {
			m.core.Facts = append(m.core.Facts[:i], m.core.Facts[i+1:]...)
			return true
		}
	}
	return false
}

// capFactsLocked: a hard cap against a runaway prompt; consolidation merges facts well before.
func (m *Memory) capFactsLocked() {
	for len(m.core.Facts) > prompts.Int("MEMORY_MAX_FACTS", 60)+20 {
		log.Printf("[Memory] full, dropping: %s", m.core.Facts[0].Text)
		m.core.Facts = m.core.Facts[1:]
	}
}

// findTopicLocked: by title (exact, ignoring case), else the first whose title contains it or
// vice versa.
func (m *Memory) findTopicLocked(title string) *Topic {
	wanted := folded(title)
	var partial *Topic
	for i := range m.core.Topics {
		own := folded(m.core.Topics[i].Title)
		if own == wanted {
			return &m.core.Topics[i]
		}
		if partial == nil && wanted != "" && (strings.Contains(own, wanted) || strings.Contains(wanted, own)) {
			partial = &m.core.Topics[i]
		}
	}
	return partial
}

func (m *Memory) topicsByAge() []Topic {
	topics := append([]Topic(nil), m.core.Topics...)
	sort.SliceStable(topics, func(i, j int) bool { return topics[i].T > topics[j].T })
	return topics
}

// pruneRecentLocked: short-term memory: only the last MEMORY_RECENT_HOURS.
func (m *Memory) pruneRecentLocked() {
	minor := prompts.Int("MEMORY_RECENT_MINOR", 3)
	oldest := time.Now().Unix() - int64(prompts.Int("MEMORY_RECENT_HOURS", 48))*3600
	// Short commands stay only while among the newest few: they must not push out the rest.
	for i := 0; i+minor < len(m.core.Recent); {
		if m.core.Recent[i].Minor {
			m.core.Recent = append(m.core.Recent[:i], m.core.Recent[i+1:]...)
		} else {
			i++
		}
	}
	for len(m.core.Recent) > 0 && (m.core.Recent[0].T < oldest || len(m.core.Recent) > prompts.Int("MEMORY_RECENT", 12)) {
		m.core.Recent = m.core.Recent[1:]
	}
}

// ProfileValue: one field of a person's profile ("" = the admin's), "" if unknown.
func (m *Memory) ProfileValue(field, person string) string {
	if person == "" {
		person = m.People.Admin()
	}
	m.mu.Lock()
	value := m.core.Profiles[person][field]
	m.mu.Unlock()
	if value == "" && field == "name" {
		value = m.People.Name(person)
	}
	return value
}

// SetOngoing: a chat with person still going on in another channel (Telegram) and not
// consolidated yet; part of every Context with that person. "" clears it.
func (m *Memory) SetOngoing(person, text string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if text == "" {
		delete(m.ongoing, person)
	} else {
		m.ongoing[person] = text
	}
}

// Skills as "name: when" lines (Telegram's /skills), "" without skills.
func (m *Memory) Skills() string {
	m.mu.Lock()
	defer m.mu.Unlock()
	var b strings.Builder
	for _, s := range m.core.Skills {
		b.WriteString(s.Name + ": " + s.When + "\n")
	}
	return b.String()
}
