// Package affairs: the open matters of the household's people (appointments, deadlines, to-dos,
// promises, things someone owes them, deliveries, trips) in one working file, issue #71. Every
// new piece of information is mapped onto them when it is written (a sorted mail, a finished
// conversation, a calendar event or to-do the agenda fetched), not when it is read: the agenda
// shows only open ones, so a matter settled or cancelled once never comes up again. A matter
// that becomes obsolete takes everything that hangs on it along (parent), without a model call.
// Every change keeps its reason in the history; the nightly review (the memory's dream) goes over
// the open ones once a night.
//
// The primary Echo keeps affairs.json in the state directory (mirrored to the others like every
// state file); the tools that change it run there (cmd/assistant/shared.go).
package affairs

import (
	"fmt"
	"log"
	"strconv"
	"strings"
	"sync"
	"time"

	"echo-assistant/internal/prompts"
	"echo-assistant/internal/store"
	"echo-assistant/internal/textutil"
)

const file = "affairs.json"

// Kinds and states.
var (
	Kinds  = []string{"appointment", "deadline", "todo", "promise", "waiting", "delivery", "trip", "other"}
	States = []string{"open", "waiting", "done", "obsolete", "snoozed"}
)

// Source: where an affair was learned or is kept. Kind mail (Ref "<mailbox>:<ref>"), calendar
// (event id), task (Google task "<list>/<task>"; About Own for the assistant's own list),
// conversation (its time).
type Source struct {
	Kind  string `json:"kind"`
	Ref   string `json:"ref"`
	About string `json:"about,omitempty"` // a mail's sender; Own for a task in the assistant's own list
}

// Own: the About of a task in the assistant's own list. Until #29 it was "jarvis" (Load converts).
const Own = "assistant"

// Change: one entry of an affair's history.
type Change struct {
	T      int64  `json:"t"`
	By     string `json:"by"`   // mail, conversation, calendar, task, user, dream, takeover
	What   string `json:"what"` // e.g. "new", "state obsolete", "due 2026-10-20"
	Reason string `json:"reason,omitempty"`
}

type Affair struct {
	ID      string   `json:"id"`
	Title   string   `json:"title"` // German, short
	Kind    string   `json:"kind"`
	State   string   `json:"state"`
	Due     string   `json:"due,omitempty"`    // YYYY-MM-DD or YYYY-MM-DD HH:MM
	Remind  string   `json:"remind,omitempty"` // snoozed until (YYYY-MM-DD)
	Person  string   `json:"person"`
	Sources []Source `json:"sources,omitempty"`
	Parent  string   `json:"parent,omitempty"` // the affair it hangs on (a rental car on its trip)
	Created int64    `json:"created"`
	Updated int64    `json:"updated"`
	History []Change `json:"history,omitempty"`
}

// Settled: done or obsolete, never shown again.
func (a *Affair) Settled() bool { return a.State == "done" || a.State == "obsolete" }

// Source returns the affair's first source of a kind (Ref "" if none).
func (a *Affair) Source(kind string) Source {
	for _, s := range a.Sources {
		if s.Kind == kind {
			return s
		}
	}
	return Source{}
}

type saved struct {
	Next    int               `json:"next"`
	Affairs []*Affair         `json:"affairs"`
	Trust   map[string]string `json:"trust,omitempty"` // person -> hide, suggest, act (settings.go)
	Quiet   map[string]string `json:"quiet,omitempty"` // person -> no unasked hints until that day
}

// Book holds the affairs.
type Book struct {
	Now func() time.Time // time.Now; tests set it
	// OnNew: an affair was made from a mail or a conversation (The assistant's own to-do list);
	// OnSettled: one became done or obsolete. Called without the lock, in the caller's goroutine.
	OnNew     func(a Affair)
	OnSettled func(a Affair)

	mu    sync.Mutex
	st    saved
	saves bool // false in tests: nothing written
}

// Load reads affairs.json.
func Load() *Book {
	b := &Book{Now: time.Now, saves: true}
	if !store.LoadJSON(file, &b.st) {
		b.st = saved{}
	}
	for _, a := range b.st.Affairs {
		for i := range a.Sources {
			if a.Sources[i].About == "jarvis" {
				a.Sources[i].About = Own
			}
		}
	}
	return b
}

// Existed: affairs.json was there (the mail deadlines are taken over once when not).
func (b *Book) Existed() bool { return store.Exists(file) }

// Save writes the file (also an empty one: the takeover is done).
func (b *Book) Save() {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.save()
}

func (b *Book) save() {
	if !b.saves {
		return
	}
	if err := store.SaveJSON(file, &b.st); err != nil {
		log.Printf("[Affairs] saving failed: %v", err)
	}
}

func valid(v string, list []string) bool {
	for _, x := range list {
		if x == v {
			return true
		}
	}
	return false
}

// normDue: "YYYY-MM-DD" or "YYYY-MM-DD HH:MM", else "".
func normDue(s string) string {
	s = strings.TrimSpace(strings.Replace(s, "T", " ", 1))
	if len(s) >= 16 {
		if _, err := time.Parse("2006-01-02 15:04", s[:16]); err == nil {
			return s[:16]
		}
	}
	if len(s) >= 10 {
		if _, err := time.Parse("2006-01-02", s[:10]); err == nil {
			return s[:10]
		}
	}
	return ""
}

// dueDay: the due date's day, "" without one.
func dueDay(a *Affair) string {
	if len(a.Due) >= 10 {
		return a.Due[:10]
	}
	return ""
}

func (b *Book) find(id string) *Affair {
	id = strings.TrimSpace(strings.Trim(strings.TrimSpace(id), "[]"))
	for _, a := range b.st.Affairs {
		if a.ID == id {
			return a
		}
	}
	return nil
}

func (b *Book) bySource(person string, src Source) *Affair {
	if src.Ref == "" || src.Kind == "conversation" {
		return nil // a conversation is the source of many
	}
	for _, a := range b.st.Affairs {
		for _, s := range a.Sources {
			if a.Person == person && s.Kind == src.Kind && s.Ref == src.Ref {
				return a
			}
		}
	}
	return nil
}

// words: the distinctive words of a title (4 letters or more), lower case.
func words(title string) map[string]bool {
	out := map[string]bool{}
	for _, w := range strings.FieldsFunc(strings.ToLower(title), func(r rune) bool {
		return !(r >= 'a' && r <= 'z' || r >= '0' && r <= '9' || r >= 0x80)
	}) {
		if len([]rune(w)) >= 4 {
			out[w] = true
		}
	}
	return out
}

// similar: the same title, one containing the other, or two distinctive words in common.
func similar(a, b string) bool {
	la, lb := strings.ToLower(strings.TrimSpace(a)), strings.ToLower(strings.TrimSpace(b))
	if la == "" || lb == "" {
		return false
	}
	if la == lb || strings.Contains(la, lb) || strings.Contains(lb, la) {
		return true
	}
	wb, common := words(b), 0
	for w := range words(a) {
		if wb[w] {
			common++
		}
	}
	return common >= 2
}

// sameMatter: an affair of the person on the same day with a similar title (a reminder mail of a
// known deadline); also a settled one: a reminder of a cancelled booking must not bring it back.
func (b *Book) sameMatter(person, title, due string) *Affair {
	for _, a := range b.st.Affairs {
		if a.Person == person && dueDay(a) == due[:10] && similar(a.Title, title) {
			return a
		}
	}
	return nil
}

func (b *Book) log(a *Affair, by, what, reason string) {
	now := b.Now().Unix()
	a.History = append(a.History, Change{T: now, By: by, What: what, Reason: reason})
	if n := len(a.History); n > 20 { // the first entry (where it came from) stays
		a.History = append(a.History[:1], a.History[n-19:]...)
	}
	a.Updated = now
}

func (b *Book) newID() string {
	b.st.Next++
	return "a" + strconv.Itoa(b.st.Next)
}

func addSource(a *Affair, src Source) bool {
	if src.Ref == "" {
		return false
	}
	for _, s := range a.Sources {
		if s.Kind == src.Kind && s.Ref == src.Ref {
			return false
		}
	}
	a.Sources = append(a.Sources, src)
	if len(a.Sources) > 10 {
		a.Sources = a.Sources[len(a.Sources)-10:]
	}
	return true
}

// propagate: an obsolete affair takes what hangs on it along (its children, theirs, ...), each
// with a history entry. Caller holds the lock; returns copies of the affairs it settled.
func (b *Book) propagate(a *Affair, by string) []Affair {
	if a.State != "obsolete" {
		return nil
	}
	var out []Affair
	for _, c := range b.st.Affairs {
		if c.Parent != a.ID || c.Settled() || c == a {
			continue
		}
		c.State = "obsolete"
		b.log(c, by, "state obsolete (with "+a.ID+")", "gehört zu „"+a.Title+"“, das hinfällig ist")
		log.Printf("[Affairs] %s obsolete with %s: %s", c.ID, a.ID, c.Title)
		out = append(out, *c)
		out = append(out, b.propagate(c, by)...)
	}
	return out
}

// setParent: the affair hangs on parent from now on (not on itself or one of its own children).
// Caller holds the lock.
func (b *Book) setParent(a *Affair, parent string) error {
	p := b.find(parent)
	if p == nil || p.Person != a.Person {
		return fmt.Errorf("no affair %s of this person to link to", parent)
	}
	for q := p; q != nil; q = b.find(q.Parent) {
		if q == a {
			return fmt.Errorf("%s hangs on %s already", p.ID, a.ID)
		}
		if q.Parent == "" {
			break
		}
	}
	a.Parent = p.ID
	return nil
}

func (b *Book) fire(made []Affair, settled []Affair) {
	if b.OnNew != nil {
		for _, a := range made {
			b.OnNew(a)
		}
	}
	if b.OnSettled != nil {
		for _, a := range settled {
			b.OnSettled(a)
		}
	}
}

// Candidate: a piece of information about a matter (from a mail or a conversation).
type Candidate struct {
	Match  string // the id of the affair it belongs to, "" = none known
	Kind   string
	Title  string
	Due    string
	State  string // open (default), waiting, done, obsolete
	Parent string // the id of the affair it hangs on, "" = none
	Source Source
	Reason string
}

// Intake maps a candidate onto the person's affairs: the affair of its source, the one it was
// matched to, one with a similar title on the same day, or a new one (only an open or waiting
// one). Returns a copy of the affair changed or made, nil if nothing happened.
func (b *Book) Intake(person string, c Candidate, by string) *Affair {
	if person == "" {
		return nil
	}
	c.Title = textutil.Prefix(strings.TrimSpace(c.Title), 100)
	c.Due = normDue(c.Due)
	if !valid(c.State, States) || c.State == "snoozed" {
		c.State = "open"
	}
	if !valid(c.Kind, Kinds) {
		c.Kind = "other"
	}
	var made, settled []Affair
	b.mu.Lock()
	a := b.bySource(person, c.Source)
	if a == nil && c.Match != "" {
		if m := b.find(c.Match); m != nil && m.Person == person {
			a = m
		}
	}
	if a == nil && c.Title != "" && c.Due != "" {
		a = b.sameMatter(person, c.Title, c.Due)
	}
	if a == nil {
		if c.Title == "" || c.State == "done" || c.State == "obsolete" {
			b.mu.Unlock()
			return nil
		}
		now := b.Now().Unix()
		a = &Affair{ID: b.newID(), Title: c.Title, Kind: c.Kind, State: c.State, Due: c.Due, Person: person, Created: now}
		addSource(a, c.Source)
		if c.Parent != "" {
			b.setParent(a, c.Parent) // an unknown parent is left out
		}
		b.st.Affairs = append(b.st.Affairs, a)
		what := "new"
		if c.Due != "" {
			what += ", due " + c.Due
		}
		if a.Parent != "" {
			what += ", part of " + a.Parent
		}
		b.log(a, by, what, c.Reason)
		b.prune()
		b.save()
		log.Printf("[Affairs] %s new (%s, %s): %s %s", a.ID, by, a.Kind, a.Title, a.Due)
		made = append(made, *a)
		out := *a
		b.mu.Unlock()
		b.fire(made, nil)
		return &out
	}
	addSource(a, c.Source)
	var what []string
	if c.Due != "" && c.Due != a.Due && !a.Settled() {
		a.Due = c.Due
		what = append(what, "due "+c.Due)
	}
	if c.Parent != "" && a.Parent == "" && b.setParent(a, c.Parent) == nil {
		what = append(what, "part of "+a.Parent)
	}
	// Settling or waiting, never reopening: a later mail about a matter is no reason to bring
	// back what the user settled.
	settles := (c.State == "done" || c.State == "obsolete") && !a.Settled()
	if settles || c.State == "waiting" && a.State == "open" {
		a.State = c.State
		what = append(what, "state "+c.State)
	}
	if len(what) > 0 {
		b.log(a, by, strings.Join(what, ", "), c.Reason)
		log.Printf("[Affairs] %s (%s): %s, %s", a.ID, by, strings.Join(what, ", "), a.Title)
	}
	if settles {
		settled = append(append(settled, *a), b.propagate(a, by)...)
	}
	b.save()
	out := *a
	b.mu.Unlock()
	b.fire(nil, settled)
	return &out
}

// AddSource links a source (The assistant's own to-do of the affair) to it.
func (b *Book) AddSource(id string, src Source) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if a := b.find(id); a != nil && addSource(a, src) {
		b.save()
	}
}

// prune drops settled affairs after AFFAIRS_KEEP_DAYS and caps the file at AFFAIRS_MAX (the
// oldest settled first, then the oldest). Caller holds the lock.
func (b *Book) prune() {
	now := b.Now().Unix()
	keep := int64(prompts.Int("AFFAIRS_KEEP_DAYS", 90)) * 86400
	kept := b.st.Affairs[:0]
	for _, a := range b.st.Affairs {
		if !(a.Settled() && now-a.Updated > keep) {
			kept = append(kept, a)
		}
	}
	b.st.Affairs = kept
	for len(b.st.Affairs) > prompts.Int("AFFAIRS_MAX", 300) {
		drop := 0
		for i, a := range b.st.Affairs {
			o := b.st.Affairs[drop]
			if a.Settled() != o.Settled() {
				if a.Settled() {
					drop = i
				}
				continue
			}
			if a.Updated < o.Updated {
				drop = i
			}
		}
		b.st.Affairs = append(b.st.Affairs[:drop], b.st.Affairs[drop+1:]...)
	}
}

// Changes for Update; "" = unchanged.
type Changes struct {
	State, Reason, Due, Remind, Title, Parent string
}

// Update changes one affair (the tool update_affair; the nightly review). person "" = anyone's
// (the review), else only the person's own. Due "none" clears it. The answer says what else
// changed (propagation) and, through Settled, what to do with the calendar event or to-do.
func (b *Book) Update(person, id string, ch Changes, by string) (string, error) {
	b.mu.Lock()
	a := b.find(id)
	if a == nil || (person != "" && a.Person != person) {
		b.mu.Unlock()
		return "", fmt.Errorf("no affair %s of this person (list_affairs lists them)", id)
	}
	wasSettled := a.Settled()
	var what []string
	if ch.State != "" && ch.State != a.State {
		if !valid(ch.State, States) {
			b.mu.Unlock()
			return "", fmt.Errorf("unknown state %s", ch.State)
		}
		a.State = ch.State
		what = append(what, "state "+ch.State)
	}
	if ch.State == "snoozed" || ch.Remind != "" {
		r := normDue(ch.Remind)
		if r == "" {
			r = b.Now().AddDate(0, 0, 1).Format("2006-01-02")
		}
		a.Remind = r[:10]
		if a.State == "open" {
			a.State = "snoozed"
		}
		what = append(what, "remind "+a.Remind)
	}
	switch due := strings.TrimSpace(ch.Due); {
	case due == "none":
		a.Due = ""
		what = append(what, "no due date")
	case due != "":
		if d := normDue(due); d == "" {
			b.mu.Unlock()
			return "", fmt.Errorf("due must be YYYY-MM-DD or YYYY-MM-DD HH:MM")
		} else if d != a.Due {
			a.Due = d
			what = append(what, "due "+d)
		}
	}
	if t := strings.TrimSpace(ch.Title); t != "" && t != a.Title {
		a.Title = textutil.Prefix(t, 100)
		what = append(what, "title")
	}
	if p := strings.TrimSpace(strings.Trim(ch.Parent, "[]")); p != "" && p != a.Parent {
		if err := b.setParent(a, p); err != nil {
			b.mu.Unlock()
			return "", err
		}
		what = append(what, "part of "+p)
	}
	now := b.Now()
	if len(what) == 0 {
		out := "Nothing changed: " + line(a, now)
		b.mu.Unlock()
		return out, nil
	}
	b.log(a, by, strings.Join(what, ", "), ch.Reason)
	var settled []Affair
	if a.Settled() && !wasSettled {
		settled = append(append(settled, *a), b.propagate(a, by)...)
	}
	// A child linked to an obsolete parent goes along at once.
	if a.Parent != "" && !a.Settled() {
		if p := b.find(a.Parent); p != nil && p.State == "obsolete" {
			a.State = "obsolete"
			b.log(a, by, "state obsolete (with "+p.ID+")", "gehört zu „"+p.Title+"“, das hinfällig ist")
			settled = append(settled, *a)
		}
	}
	b.prune()
	b.save()
	log.Printf("[Affairs] %s (%s): %s, %s: %s", a.ID, by, strings.Join(what, ", "), a.Title, ch.Reason)
	out := "Changed: " + line(a, now)
	for _, s := range settled[min(1, len(settled)):] {
		if s.ID != a.ID {
			out += "\nAlso " + s.State + " (part of it): " + line(&s, now)
		}
	}
	if len(settled) > 0 {
		out += b.followUpLocked(settled)
	}
	b.mu.Unlock()
	b.fire(nil, settled)
	return out, nil
}
