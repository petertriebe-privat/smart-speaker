// Package initiative: The assistant helps unasked (#82). A pass between conversations collects
// candidates from the sources (affairs waiting for a reply, appointments with a place coming up),
// keeps each matter once in initiative.json with what became of it, and tallies the user's
// reactions per kind, so the weighing learns how often the user wants to hear from him.
//
// Step 1: candidates are recorded (state "dry") and logged. Step 2 (weigh.go): each is weighed
// and becomes dropped or pending. Nothing is told or done yet. The pass runs on the primary Echo
// only.
package initiative

import (
	"fmt"
	"log"
	"strconv"
	"strings"
	"sync"
	"time"

	"echo-assistant/internal/prompts"
	"echo-assistant/internal/store"
)

const file = "initiative.json"

// States of a suggestion. dry: recorded only (step 1). The others come with the weighing and the
// delivery: dropped (not worth it), pending (to be told), told, and the user's reaction:
// accepted, declined, ignored.
var States = []string{"dry", "later", "dropped", "pending", "told", "accepted", "declined", "ignored"}

// Reactions: the states that count in the tally.
var Reactions = []string{"accepted", "declined", "ignored"}

// Candidate: something the sources found that might help.
type Candidate struct {
	Key    string    // the same matter once: "follow_up:a12", "leave_time:<event id>"
	Kind   string    // follow_up, leave_time
	Person string    // whom it is for
	Text   string    // English, for the weighing and the log
	Until  time.Time // when it stops mattering (zero: no end)
}

// Suggestion: a candidate as kept in the journal.
type Suggestion struct {
	ID      string `json:"id"`
	Key     string `json:"key"`
	Kind    string `json:"kind"`
	Person  string `json:"person"`
	Text    string `json:"text"`
	State   string `json:"state"`
	When    string `json:"when,omitempty"`  // the weighing's Whens
	Level   string `json:"level,omitempty"` // the weighing's Levels
	Reason  string `json:"reason,omitempty"`
	Created int64  `json:"created"`
	Updated int64  `json:"updated"`
	Until   int64  `json:"until,omitempty"`
	At      int64  `json:"at,omitempty"`     // pending: delivered by a background run from then on; 0 = in a conversation
	Handed  int    `json:"handed,omitempty"` // conversations it was in without an answer
}

type saved struct {
	Next        int           `json:"next"`
	Suggestions []*Suggestion `json:"suggestions"`
}

// Journal holds the suggestions.
type Journal struct {
	Now func() time.Time // time.Now; tests set it
	// BriefingHour: the person's briefing hour today from their rules (package prefer), given the
	// start value; nil = the start value.
	BriefingHour func(person string, start int) int

	mu    sync.Mutex
	st    saved
	saves bool // false in tests: nothing written
}

// Load reads initiative.json.
func Load() *Journal {
	j := &Journal{Now: time.Now, saves: true}
	if !store.LoadJSON(file, &j.st) {
		j.st = saved{}
	}
	return j
}

func (j *Journal) save() {
	if !j.saves {
		return
	}
	if err := store.SaveJSON(file, &j.st); err != nil {
		log.Printf("[Initiative] saving failed: %v", err)
	}
}

func (j *Journal) byKey(key string) *Suggestion {
	for _, s := range j.st.Suggestions {
		if s.Key == key {
			return s
		}
	}
	return nil
}

// Add records a candidate once per key; the suggestion and whether it is new.
func (j *Journal) Add(c Candidate, state string) (Suggestion, bool) {
	j.mu.Lock()
	defer j.mu.Unlock()
	if s := j.byKey(c.Key); s != nil {
		if s.State == "dry" || s.State == "later" {
			s.Text = c.Text // still to be weighed: the latest facts ("waiting for 6 days")
		}
		return *s, false
	}
	j.st.Next++
	now := j.Now().Unix()
	s := &Suggestion{ID: "s" + strconv.Itoa(j.st.Next), Key: c.Key, Kind: c.Kind, Person: c.Person, Text: c.Text,
		State: state, Created: now, Updated: now}
	if !c.Until.IsZero() {
		s.Until = c.Until.Unix()
	}
	j.st.Suggestions = append(j.st.Suggestions, s)
	j.prune()
	j.save()
	return *s, true
}

// Set changes a suggestion's state (a reaction of the user, or what the weighing made of it).
func (j *Journal) Set(id, state, reason string) error {
	valid := false
	for _, s := range States {
		valid = valid || s == state
	}
	if !valid {
		return fmt.Errorf("unknown state %q", state)
	}
	j.mu.Lock()
	defer j.mu.Unlock()
	for _, s := range j.st.Suggestions {
		if s.ID == id {
			s.State, s.Reason, s.Updated = state, reason, j.Now().Unix()
			j.save()
			return nil
		}
	}
	return fmt.Errorf("no suggestion %s", id)
}

// Verdict: the weighing's result for a suggestion; at: when a background run delivers it (0 =
// in a conversation).
func (j *Journal) Verdict(id, state, when, level, reason string, at int64) {
	j.mu.Lock()
	defer j.mu.Unlock()
	for _, s := range j.st.Suggestions {
		if s.ID == id {
			s.State, s.When, s.Level, s.Reason, s.At, s.Updated = state, when, level, reason, at, j.Now().Unix()
			j.save()
			return
		}
	}
}

// ForConversation: the person's pending suggestions for a conversation, oldest first: those
// for the next conversation, and briefing ones from the morning after they were found on.
func (j *Journal) ForConversation(person string) []Suggestion {
	now := j.Now()
	today := now.Format("2006-01-02")
	j.mu.Lock()
	defer j.mu.Unlock()
	var out []Suggestion
	for _, s := range j.st.Suggestions {
		if s.Person != person || s.State != "pending" || s.At != 0 || (s.Until != 0 && s.Until < now.Unix()) {
			continue
		}
		briefing := prompts.Int("AGENDA_BRIEFING_FROM_HOUR", 4)
		if j.BriefingHour != nil {
			briefing = j.BriefingHour(person, briefing)
		}
		if s.When == "briefing" && (time.Unix(s.Created, 0).Format("2006-01-02") == today || now.Hour() < briefing) {
			continue
		}
		out = append(out, *s)
	}
	return out
}

// Due: pending suggestions a background run delivers now.
func (j *Journal) Due() []Suggestion {
	now := j.Now().Unix()
	j.mu.Lock()
	defer j.mu.Unlock()
	var out []Suggestion
	for _, s := range j.st.Suggestions {
		if s.State == "pending" && s.At != 0 && s.At <= now {
			out = append(out, *s)
		}
	}
	return out
}

// Handed: the suggestions were in a conversation that ended without an answer to them; after
// INITIATIVE_HAND_MAX such conversations one counts as ignored.
func (j *Journal) Handed(ids []string) {
	limit := prompts.Int("INITIATIVE_HAND_MAX", 3)
	j.mu.Lock()
	defer j.mu.Unlock()
	changed := false
	for _, s := range j.st.Suggestions {
		for _, id := range ids {
			if s.ID == id && s.State == "pending" {
				s.Handed++
				if s.Handed >= limit {
					s.State, s.Reason, s.Updated = "ignored", fmt.Sprintf("no answer in %d conversations", s.Handed), j.Now().Unix()
				}
				changed = true
			}
		}
	}
	if changed {
		j.save()
	}
}

// Reconsider: suggestions weighed "later" at least `after` ago are weighed again (dry).
func (j *Journal) Reconsider(after time.Duration) {
	limit := j.Now().Add(-after).Unix()
	j.mu.Lock()
	defer j.mu.Unlock()
	for _, s := range j.st.Suggestions {
		if s.State == "later" && s.Updated <= limit {
			s.State, s.Updated = "dry", j.Now().Unix()
		}
	}
}

// InState: copies of the suggestions in a state, oldest first.
func (j *Journal) InState(state string) []Suggestion {
	j.mu.Lock()
	defer j.mu.Unlock()
	var out []Suggestion
	for _, s := range j.st.Suggestions {
		if s.State == state {
			out = append(out, *s)
		}
	}
	return out
}

// Tally: the person's reactions to suggestions of a kind (accepted, declined, ignored).
func (j *Journal) Tally(person, kind string) map[string]int {
	j.mu.Lock()
	defer j.mu.Unlock()
	out := map[string]int{}
	for _, s := range j.st.Suggestions {
		if s.Person != person || s.Kind != kind {
			continue
		}
		for _, r := range Reactions {
			if s.State == r {
				out[r]++
			}
		}
	}
	return out
}

// Reactions: the household's answers to the assistant's suggestions, for the dream to learn rules from
// (#82, step 6): the tally per person and kind and each answered or ignored suggestion with its
// reason; "" without any. name gives a person's name.
func (j *Journal) Reactions(name func(string) string) string {
	j.mu.Lock()
	defer j.mu.Unlock()
	type key struct{ person, kind string }
	tally := map[key]map[string]int{}
	var order []key
	var lines strings.Builder
	for _, s := range j.st.Suggestions {
		reacted := false
		for _, r := range Reactions {
			reacted = reacted || s.State == r
		}
		if !reacted {
			continue
		}
		k := key{s.Person, s.Kind}
		if tally[k] == nil {
			tally[k] = map[string]int{}
			order = append(order, k)
		}
		tally[k][s.State]++
		fmt.Fprintf(&lines, "- %s %s, %s: %s", time.Unix(s.Updated, 0).Format("2006-01-02"), name(s.Person), s.State, s.Text)
		if s.Reason != "" {
			lines.WriteString(" (" + s.Reason + ")")
		}
		lines.WriteString("\n")
	}
	if len(order) == 0 {
		return ""
	}
	var b strings.Builder
	for _, k := range order {
		t := tally[k]
		fmt.Fprintf(&b, "%s, %s: accepted %d, declined %d, ignored %d\n", name(k.person), k.kind, t["accepted"], t["declined"], t["ignored"])
	}
	return b.String() + lines.String()
}

// prune: suggestions unchanged for INITIATIVE_KEEP_DAYS go (caller holds the lock). A key that
// is gone may be found again.
func (j *Journal) prune() {
	limit := j.Now().AddDate(0, 0, -prompts.Int("INITIATIVE_KEEP_DAYS", 30)).Unix()
	kept := j.st.Suggestions[:0]
	for _, s := range j.st.Suggestions {
		if s.Updated >= limit {
			kept = append(kept, s)
		}
	}
	j.st.Suggestions = kept
}

// Source: where candidates of one kind come from. Find returns all that hold now: a dry or
// pending suggestion of the kind that is no longer among them is dropped (the appointment moved,
// the reply came).
type Source struct {
	Kind string
	Find func() []Candidate
}

// Gone: dry and pending suggestions of kind whose key is not in seen are dropped.
func (j *Journal) Gone(kind string, seen map[string]bool) {
	j.mu.Lock()
	defer j.mu.Unlock()
	changed := false
	for _, s := range j.st.Suggestions {
		if s.Kind == kind && (s.State == "dry" || s.State == "later" || s.State == "pending") && !seen[s.Key] {
			s.State, s.Reason, s.Updated = "dropped", "no longer found (changed or settled)", j.Now().Unix()
			changed = true
		}
	}
	if changed {
		j.save()
	}
}

// Pass: the initiative pass between conversations.
type Pass struct {
	Journal *Journal
	Sources []Source
	Idle    func() bool // no conversation runs anywhere
	Now     func() time.Time
	// Weigh: the verdict on a candidate (Decide); nil = recorded dry only.
	Weigh Weigher
	// Situation: the person's rules and the situation now, as text for the weighing.
	Situation func(person string) string
	// Deliver: a background run that tells a person the suggestions due together (deliver.go);
	// its answer.
	Deliver func(batch []Suggestion) string
	// Quiet: the person paused unasked hints ("Heute nicht", pause_hints): only urgent ones go.
	Quiet func(person string) bool

	mu     sync.Mutex
	handed map[string][]string // conversation label -> the suggestions in its instructions
	kick   chan struct{}
}

// Kick: a source changed (the agenda found a moved appointment): a pass at the next chance.
func (p *Pass) Kick() {
	p.mu.Lock()
	if p.kick == nil {
		p.kick = make(chan struct{}, 1)
	}
	k := p.kick
	p.mu.Unlock()
	select {
	case k <- struct{}{}:
	default:
	}
}

// Run: a pass every INITIATIVE_EVERY_MS and the due deliveries every minute, while nobody talks
// with the assistant. Start as a goroutine.
func (p *Pass) Run() {
	every := time.Duration(prompts.Int("INITIATIVE_EVERY_MS", 900000)) * time.Millisecond
	p.Kick() // creates the channel; the first pass at once after the start
	p.mu.Lock()
	kick := p.kick
	p.mu.Unlock()
	var last time.Time
	kicked := false
	tick := time.NewTicker(time.Minute)
	for {
		select {
		case <-tick.C:
		case <-kick:
			kicked = true
		}
		if p.Idle != nil && !p.Idle() {
			continue // a kick waits for the next idle minute
		}
		if kicked || time.Since(last) >= every {
			kicked = false
			last = time.Now()
			p.Once()
		}
		p.deliverDue()
	}
}

// Once: one pass; the suggestions recorded new.
func (p *Pass) Once() []Suggestion {
	now := time.Now()
	if p.Now != nil {
		now = p.Now()
	}
	var made []Suggestion
	for _, source := range p.Sources {
		seen := map[string]bool{}
		found := source.Find()
		for _, c := range found {
			seen[c.Key] = true
		}
		p.Journal.Gone(source.Kind, seen)
		for _, c := range found {
			if c.Key == "" || (!c.Until.IsZero() && c.Until.Before(now)) {
				continue
			}
			if s, isNew := p.Journal.Add(c, "dry"); isNew {
				log.Printf("[Initiative] dry %s %s for %q: %s", s.ID, s.Kind, s.Person, s.Text)
				made = append(made, s)
			}
		}
	}
	p.weigh()
	return made
}
