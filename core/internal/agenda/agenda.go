// Package agenda: the admin's appointments and due to-dos until tomorrow night, as
// devices/atoms3r/src/assistant/agenda.cpp: fetched between conversations, in the instructions of
// every conversation (and agent run), news when an appointment comes close during a
// conversation, and before hanging up the entries that did not come up yet.
//
// On the Echo several conversations run at once (speaker and phone): what was handed to a
// conversation and what came up is kept per conversation (key: its label), not once as on the M5.
package agenda

import (
	"encoding/json"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"echo-assistant/internal/affairs"
	"echo-assistant/internal/google"
	"echo-assistant/internal/netx"
	"echo-assistant/internal/prompts"
	"echo-assistant/internal/store"
	"echo-assistant/internal/textutil"
)

type item struct {
	key, title, line string
	start, end       time.Time // a to-do: midnight of its due day; an affair: zero without a date
	allDay, info     bool
	todo             bool
	list             string
	affair           *affairs.Item
	affairID         string // a calendar event's or to-do's affair (Observe)
}

type conversation struct {
	handed   []string // keys in its agenda
	noticed  []string // events passed on as a notice
	handedAt time.Time
	affairs  []item
	reader   string
	briefing bool // the morning briefing was asked for in it
}

// The affairs' hooks (package affairs, #71); all optional.
type Affairs struct {
	// Relevant: a reader's most relevant open affairs.
	Relevant func(reader string) []affairs.Item
	// Observe links a calendar event or to-do of the admin to an affair; its id.
	Observe func(src affairs.Source, kind, title, due string) string
	// State: an affair's state now ("done", "obsolete", "snoozed" leave its entries out).
	State func(id string) string
	// Quiet: the day until which the reader wants no unasked hints, "" = none.
	Quiet func(reader string) string
}

type Agenda struct {
	Google  *google.Account
	Admin   func() string
	Busy    func() bool   // a conversation is open: fetching waits
	Every   time.Duration // between fetches; 0 = AGENDA_POLL_MS (the phone fetches less, its battery)
	Affairs Affairs
	// OnChange: a fetch found the events or to-dos changed (the initiative looks at once).
	OnChange func()
	// Prefer: the reader's value of a preference today from their rules (package prefer: what is
	// briefing, evening or urgent), given the start value from the config; nil = the start value.
	// Never waits for the network.
	Prefer func(reader, what string, start int) int

	mu         sync.Mutex
	briefed    map[string]string // reader -> the day of their morning briefing
	items      []item
	fetched    bool
	fetchedAt  time.Time
	fetchedDay string
	nextAt     time.Time
	dirty      bool
	mentioned  map[string]string // key -> the day it came up
	offered    map[string]string // key -> the day it was offered before hanging up
	convs      map[string]*conversation
}

func New(g *google.Account, admin func() string) *Agenda {
	a := &Agenda{Google: g, Admin: admin, mentioned: map[string]string{}, offered: map[string]string{},
		convs: map[string]*conversation{}, briefed: map[string]string{}}
	a.loadSaid()
	return a
}

// saidFile keeps what came up or was offered today across restarts: after each restart the same
// "Übrigens …" came again (18.10.2026, a day of deploys).
func saidFile() string { return filepath.Join(store.Dir, "agenda_said.json") }

func (a *Agenda) loadSaid() {
	data, err := os.ReadFile(saidFile())
	if err != nil {
		return
	}
	var said struct{ Mentioned, Offered, Briefed map[string]string }
	if json.Unmarshal(data, &said) != nil {
		return
	}
	for k, d := range said.Briefed {
		if d == today() {
			a.briefed[k] = d
		}
	}
	for k, d := range said.Mentioned {
		if d == today() {
			a.mentioned[k] = d
		}
	}
	for k, d := range said.Offered {
		if d == today() {
			a.offered[k] = d
		}
	}
}

// saveSaid: called with a.mu held.
func (a *Agenda) saveSaid() {
	data, _ := json.Marshal(map[string]map[string]string{"Mentioned": a.mentioned, "Offered": a.offered,
		"Briefed": a.briefed})
	if err := os.WriteFile(saidFile(), data, 0o600); err != nil {
		log.Printf("[Agenda] %v", err)
	}
}

func day(t time.Time, days int) time.Time {
	return time.Date(t.Year(), t.Month(), t.Day()+days, 0, 0, 0, 0, time.Local)
}

func today() string { return time.Now().Format("2006-01-02") }

func midnightOf(d string) time.Time {
	t, _ := time.ParseInLocation("2006-01-02", d, time.Local)
	return t
}

func (a *Agenda) allowed(person string, atHome bool) bool {
	if person != "" {
		return person == a.Admin()
	}
	return atHome
}

// RefreshSoon: a calendar entry or to-do changed; fetch again before the next conversation.
func (a *Agenda) RefreshSoon() {
	a.mu.Lock()
	a.dirty = true
	a.mu.Unlock()
}

// Run fetches between conversations: every AGENDA_REFRESH_MS, on a new day, after changes.
func (a *Agenda) Run() {
	for range time.Tick(time.Second) {
		a.refresh()
	}
}

func (a *Agenda) refresh() {
	if !netx.TimeSynced || (a.Busy != nil && a.Busy()) || !a.Google.Configured() {
		return
	}
	a.mu.Lock()
	newDay := a.fetched && a.fetchedDay != today()
	due := a.dirty || newDay || time.Now().After(a.nextAt)
	a.mu.Unlock()
	if !due {
		return
	}
	now := time.Now()
	started := now
	events, err := a.Google.Events(day(now, 0), day(now, 2))
	var todos []google.Todo
	if err == nil {
		todos, err = a.Google.DueTodos(day(now, 1).Format("2006-01-02"))
	}
	var fresh []item
	if err == nil {
		for _, e := range events {
			it := item{key: e.ID, title: e.Title, line: e.Line, start: e.Start, end: e.End, allDay: e.AllDay, info: e.Info}
			if a.Affairs.Observe != nil && !e.Info { // each event an affair: "das fällt aus" hides it
				due := e.Start.Format("2006-01-02 15:04")
				if e.AllDay {
					due = e.Start.Format("2006-01-02")
				}
				it.affairID = a.Affairs.Observe(affairs.Source{Kind: "calendar", Ref: e.ID}, "appointment", e.Title, due)
			}
			fresh = append(fresh, it)
		}
		for _, t := range todos {
			m := midnightOf(t.Due)
			it := item{key: t.ID, title: t.Title, line: t.Title, todo: true, allDay: true, list: t.List, start: m, end: m}
			if a.Affairs.Observe != nil {
				it.affairID = a.Affairs.Observe(affairs.Source{Kind: "task", Ref: t.ID}, "todo", t.Title, t.Due)
			}
			fresh = append(fresh, it)
		}
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	a.dirty = false
	a.fetchedDay = today()
	if err != nil {
		log.Printf("[Agenda] %v", err)
		a.nextAt = time.Now().Add(time.Duration(prompts.Int("AGENDA_RETRY_MS", 300000)) * time.Millisecond)
		return
	}
	// the first fetch counts as a change: the initiative's first pass ran before it (24.10.2026)
	changed := !a.fetched || signature(a.items) != signature(fresh)
	a.items, a.fetched, a.fetchedAt = fresh, true, now
	// The Echo fetches more often than the M5 (AGENDA_REFRESH_MS, one TLS connection there): an
	// appointment moved on the phone reaches the initiative within minutes (#82).
	every := time.Duration(prompts.Int("AGENDA_POLL_MS", 300000)) * time.Millisecond
	if a.Every > 0 {
		every = a.Every
	}
	a.nextAt = time.Now().Add(every)
	log.Printf("[Agenda] %d events, %d to-dos due (%d ms)", len(events), len(todos), time.Since(started).Milliseconds())
	if changed && a.OnChange != nil {
		go a.OnChange()
	}
}

// signature: what makes two fetches the same (keys, times, lines).
func signature(items []item) string {
	var b strings.Builder
	for _, it := range items {
		b.WriteString(it.key + "|" + it.start.Format(time.RFC3339) + "|" + it.end.Format(time.RFC3339) + "|" + it.line + "\n")
	}
	return b.String()
}

// describeAffair: "[a7] deadline tomorrow at 14:30: <title> (from a mail from <sender>)": it
// says what the entry is, so the assistant does not call a mail's deadline a calendar entry (#71).
// Order: -1 overdue, 0 today, 1 tomorrow, 2 later, 3 no date.
func describeAffair(it item, now time.Time) (string, int) {
	a := it.affair
	today, tomorrow := day(now, 0), day(now, 1)
	when, order := "", 3
	if !it.start.IsZero() {
		switch {
		case it.start.Before(today):
			order, when = -1, "overdue since "+it.start.Format("02.01.")
		case it.start.Before(tomorrow):
			order, when = 0, "today"
		case it.start.Before(day(now, 2)):
			order, when = 1, "tomorrow"
		default:
			order, when = 2, "on "+it.start.Format("Mon 02.01.")
		}
		if !it.allDay {
			when += " at " + it.start.Format("15:04")
		}
	}
	text := "[" + a.ID + "] " + a.Kind
	if when != "" {
		text += " " + when
	}
	text += ": " + a.Title
	if a.State == "waiting" {
		text += " (waiting for someone else)"
	}
	if a.From != "" {
		text += " (from " + a.From + ")"
	}
	return text, order
}

// describe: the item's line for now ("" once it is over); order sorts: overdue, today, tomorrow.
// A calendar event or to-do with an affair starts with its id, so "das fällt aus" can settle it.
func describe(it item, now time.Time) (string, int) {
	if it.affair != nil {
		return describeAffair(it, now)
	}
	text, order := describeEntry(it, now)
	if text != "" && it.affairID != "" {
		text = "[" + it.affairID + "] " + text
	}
	return text, order
}

// hidden: the item's affair is settled, or snoozed until a later day.
func (a *Agenda) hidden(it item) bool {
	if it.affairID == "" || a.Affairs.State == nil {
		return false
	}
	switch a.Affairs.State(it.affairID) {
	case "done", "obsolete", "snoozed":
		return true
	}
	return false
}

func (a *Agenda) quiet(reader string) string {
	if reader == "" || a.Affairs.Quiet == nil {
		return ""
	}
	return a.Affairs.Quiet(reader)
}

func describeEntry(it item, now time.Time) (string, int) {
	today, tomorrow := day(now, 0), day(now, 1)
	if it.todo {
		var when string
		order := 1
		switch {
		case it.start.Before(today):
			order, when = -1, "overdue since "+it.start.Format("02.01.")
		case it.start.Before(tomorrow):
			order, when = 0, "due today"
		default:
			when = "due tomorrow"
		}
		text := "to-do " + when + ": " + it.line
		if it.list != "" {
			text += " (list " + it.list + ")"
		}
		return text, order
	}
	if !it.end.After(now) || (it.allDay && !it.end.After(today)) {
		return "", 0
	}
	order, prefix := 1, "tomorrow, "
	if it.start.Before(tomorrow) {
		order, prefix = 0, "today, "
	}
	text := prefix + it.line
	if !it.allDay && !it.start.After(now) {
		text += " (running now)"
	}
	return text, order
}

var common = map[string]bool{"termin": true, "heute": true, "morgen": true, "noch": true, "beim": true,
	"aufgabe": true, "erinnerung": true, "with": true, "the": true, "und": true, "oder": true, "fuer": true, "für": true}

// comesUp: a distinctive word of title (4 bytes or more, not a common one) occurs in transcript.
func comesUp(title, transcript string) bool {
	lower := strings.ToLower(transcript)
	for _, w := range strings.FieldsFunc(strings.ToLower(title), func(r rune) bool {
		return !(r >= 'a' && r <= 'z' || r >= '0' && r <= '9' || r >= 0x80)
	}) {
		if len(w) >= 4 && !common[w] && strings.Contains(lower, w) {
			return true
		}
	}
	return false
}

// affairItems: the reader's most relevant open affairs as items.
func (a *Agenda) affairItems(reader string) []item {
	if reader == "" || a.Affairs.Relevant == nil || !netx.TimeSynced {
		return nil
	}
	var out []item
	for _, af := range a.Affairs.Relevant(reader) {
		it := item{key: "affair:" + af.ID, title: af.Title, line: af.Title, allDay: true, todo: true}
		copied := af
		it.affair = &copied
		if len(af.Due) == 16 {
			it.start, _ = time.ParseInLocation("2006-01-02 15:04", af.Due, time.Local)
			it.allDay = false
		} else if len(af.Due) == 10 {
			it.start = midnightOf(af.Due)
		}
		it.end = it.start
		out = append(out, it)
	}
	return out
}

func (a *Agenda) find(key string, c *conversation) *item {
	for i := range a.items {
		if a.items[i].key == key {
			return &a.items[i]
		}
	}
	if c != nil {
		for i := range c.affairs {
			if c.affairs[i].key == key {
				return &c.affairs[i]
			}
		}
	}
	return nil
}

// Context: the agenda for instructions. person: for whom ("" = nobody named); atHome: at the
// speaker; conv: the voice conversation's label ("" = an agent run, nothing remembered).
func (a *Agenda) Context(person string, atHome bool, conv string) string {
	now := time.Now()
	reader := person
	if reader == "" && atHome {
		reader = a.Admin()
	}
	own := a.affairItems(reader)
	a.mu.Lock()
	defer a.mu.Unlock()
	voice := conv != ""
	var c *conversation
	if voice { // a new conversation: nothing of an earlier one carries over
		c = &conversation{affairs: own, reader: reader}
		a.convs[conv] = c
	}
	calendar := a.allowed(person, atHome) && a.fetched
	if !calendar && len(own) == 0 {
		return ""
	}
	// An affair whose calendar event or to-do is shown is that line already.
	if calendar {
		shown := map[string]bool{}
		for _, it := range a.items {
			shown[it.affairID] = it.affairID != ""
		}
		kept := own[:0]
		for _, it := range own {
			if !shown[it.affair.ID] {
				kept = append(kept, it)
			}
		}
		own = kept
	}
	type line struct {
		order     int
		todo      bool
		start     time.Time
		text, key string
	}
	var lines []line
	add := func(it item) {
		if a.hidden(it) {
			return
		}
		if text, order := describe(it, now); text != "" {
			lines = append(lines, line{order, it.todo, it.start,
				textutil.Prefix(text, prompts.Int("AGENDA_LINE_CHARS", 160)), it.key})
		}
	}
	if calendar {
		for _, it := range a.items {
			add(it)
		}
	}
	for _, it := range own {
		add(it)
	}
	sort.SliceStable(lines, func(i, j int) bool {
		if lines[i].order != lines[j].order {
			return lines[i].order < lines[j].order
		}
		if lines[i].todo != lines[j].todo {
			return !lines[i].todo // the day's events, then its to-dos
		}
		return lines[i].start.Before(lines[j].start)
	})
	asOf := now
	if calendar {
		asOf = a.fetchedAt
	}
	heading := strings.ReplaceAll(prompts.Text("AGENDA_HEADING"), "{time}", asOf.Format("15:04"))
	if voice {
		c.handedAt = now
	}
	if len(lines) == 0 {
		return heading + "(nothing in the calendar, no to-do due)\n"
	}
	maxLines := prompts.Int("AGENDA_MAX_LINES", 16)
	var fresh, known strings.Builder
	shown := 0
	for _, l := range lines {
		if shown == maxLines {
			break
		}
		shown++
		if voice && a.mentioned[l.key] == today() {
			known.WriteString("- " + l.text + "\n")
		} else {
			fresh.WriteString("- " + l.text + "\n")
		}
		if voice {
			c.handed = append(c.handed, l.key)
		}
	}
	text := heading
	if !voice {
		text += fresh.String()
	} else {
		if fresh.Len() > 0 {
			text += prompts.Text("AGENDA_NEW") + fresh.String()
		}
		if known.Len() > 0 {
			text += prompts.Text("AGENDA_MENTIONED") + known.String()
		}
	}
	if len(lines) > shown {
		text += fmt.Sprintf("(and %d more: list_events / list_todos / list_affairs)\n", len(lines)-shown)
	}
	if !voice || a.Affairs.Relevant == nil { // the hints' rules: where the assistant keeps affairs only
		return text
	}
	text += prompts.Text("AGENDA_HINTS")
	if q := a.quiet(reader); q != "" {
		text += strings.ReplaceAll(prompts.Text("AGENDA_QUIET"), "{until}", q)
	} else if reader != "" && a.briefed[reader] != today() && now.Hour() >= a.prefer(reader, "briefing", "AGENDA_BRIEFING_FROM_HOUR", 4) &&
		fresh.Len() > 0 {
		text += prompts.Text("AGENDA_BRIEFING")
		c.briefing = true
	}
	return text
}

// prefer: the preference `what` for reader, the config's `name` (def) as the start value (#82).
func (a *Agenda) prefer(reader, what, name string, def int) int {
	start := prompts.Int(name, def)
	if a.Prefer == nil || reader == "" {
		return start
	}
	return a.Prefer(reader, what, start)
}

// Appointment: a timed calendar event of the admin, for the initiative (#82): leaving times and
// overlaps. Before and After: the neighbouring timed events of the same day, so a way can start
// at the place before and the plan of the day is seen as a whole.
type Appointment struct {
	Key, Title, Place string
	Start, End        time.Time
	Before, After     *Appointment
}

func placeOf(line string) string {
	_, place, ok := strings.Cut(line, " | place: ")
	if !ok {
		return ""
	}
	place, _, _ = strings.Cut(place, " | ")
	return strings.TrimSpace(place)
}

// timed: the admin's timed events of the last fetch, by start; all-day, info, to-dos and settled
// ones left out. Called with a.mu held.
func (a *Agenda) timed() []Appointment {
	var out []Appointment
	for _, it := range a.items {
		if it.todo || it.allDay || it.info || a.hidden(it) {
			continue
		}
		out = append(out, Appointment{Key: it.key, Title: it.title, Place: placeOf(it.line), Start: it.start, End: it.end})
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].Start.Before(out[j].Start) })
	return out
}

// WithPlace: the events with a place that start within `within`, with their neighbours.
func (a *Agenda) WithPlace(within time.Duration) []Appointment {
	now := time.Now()
	a.mu.Lock()
	all := a.timed()
	a.mu.Unlock()
	var out []Appointment
	for i, e := range all {
		if e.Place == "" || !e.Start.After(now) || e.Start.Sub(now) > within {
			continue
		}
		if i > 0 && sameDay(all[i-1].Start, e.Start) {
			before := all[i-1]
			e.Before = &before
		}
		if i+1 < len(all) && sameDay(all[i+1].Start, e.Start) {
			after := all[i+1]
			e.After = &after
		}
		out = append(out, e)
	}
	return out
}

// Overlaps: pairs of timed events that overlap and end after now, the earlier one first.
func (a *Agenda) Overlaps() [][2]Appointment {
	now := time.Now()
	a.mu.Lock()
	all := a.timed()
	a.mu.Unlock()
	var out [][2]Appointment
	for i := range all {
		for j := i + 1; j < len(all) && all[j].Start.Before(all[i].End); j++ {
			if all[j].End.After(now) {
				out = append(out, [2]Appointment{all[i], all[j]})
			}
		}
	}
	return out
}

func sameDay(a, b time.Time) bool { return a.Format("2006-01-02") == b.Format("2006-01-02") }

// Notices: appointments coming close during conversation conv (privateForAdmin: the admin's
// calendar may be told there); text and urgency for live.Session.Notice. Call every 20 s.
func (a *Agenda) Notices(conv string, privateForAdmin bool) [][2]any {
	if !privateForAdmin {
		return nil
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	c := a.convs[conv]
	if !a.fetched || c == nil || c.handedAt.IsZero() {
		return nil
	}
	now := time.Now()
	soon := time.Duration(prompts.Int("AGENDA_SOON_MIN", 60)) * time.Minute
	quiet := a.quiet(c.reader) != ""
	urgent := a.prefer(c.reader, "urgent", "AGENDA_URGENT_MIN", 15)
	var out [][2]any
	for _, it := range a.items {
		if it.allDay || it.info || it.todo || !it.start.After(now) || a.hidden(it) {
			continue
		}
		// "Heute nicht": only an appointment in the next minutes still interrupts.
		if quiet && it.start.Sub(now) > time.Duration(urgent)*time.Minute {
			continue
		}
		ahead := it.start.Sub(now)
		// Already this close when the conversation started: it was in its agenda.
		if ahead > soon || it.start.Sub(c.handedAt) <= soon {
			continue
		}
		dup := false
		for _, k := range c.noticed {
			dup = dup || k == it.key
		}
		if dup {
			continue
		}
		c.noticed = append(c.noticed, it.key)
		minutes := int((ahead + time.Minute - 1) / time.Minute)
		text := fmt.Sprintf("Appointment at %s (in %d minutes): %s", it.start.Format("15:04"), minutes,
			textutil.Prefix(it.line, prompts.Int("AGENDA_LINE_CHARS", 160)))
		out = append(out, [2]any{text, minutes <= urgent})
	}
	return out
}

// Closing: before hanging up, entries of conv's agenda that matter now and did not come up yet,
// once a day each (tomorrow's from AGENDA_EVENING_HOUR on). "" = none.
func (a *Agenda) Closing(conv, transcript string) string {
	a.mu.Lock()
	defer a.mu.Unlock()
	c := a.convs[conv]
	if c == nil || c.handedAt.IsZero() || a.quiet(c.reader) != "" {
		return ""
	}
	now := time.Now()
	evening := a.prefer(c.reader, "evening", "AGENDA_EVENING_HOUR", 18)
	var lines []string
	for _, key := range c.handed {
		it := a.find(key, c)
		if it == nil || it.info || a.hidden(*it) {
			continue
		}
		// Settled during this conversation (update_affair): not offered any more.
		if it.affair != nil && a.Affairs.State != nil {
			if st := a.Affairs.State(it.affair.ID); st != "open" && st != "waiting" {
				continue
			}
		}
		text, order := describe(*it, now)
		if text == "" || order > 1 || (order > 0 && now.Hour() < evening) {
			continue
		}
		if a.mentioned[key] == today() || a.offered[key] == today() || comesUp(it.title, transcript) {
			continue
		}
		a.offered[key] = today()
		lines = append(lines, "- "+textutil.Prefix(text, prompts.Int("AGENDA_LINE_CHARS", 160)))
	}
	if len(lines) > 0 {
		a.saveSaid()
	}
	return strings.TrimSpace(strings.Join(lines, "\n"))
}

// ConversationOver: what came up in conv is remembered for today.
func (a *Agenda) ConversationOver(conv, transcript string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	c := a.convs[conv]
	delete(a.convs, conv)
	if c == nil || c.handedAt.IsZero() {
		return
	}
	if c.briefing && strings.TrimSpace(transcript) != "" {
		a.briefed[c.reader] = today() // the first conversation of the day had its briefing
	}
	for _, days := range []map[string]string{a.mentioned, a.offered, a.briefed} { // yesterday's are of no use
		for k, d := range days {
			if d != today() {
				delete(days, k)
			}
		}
	}
	for _, key := range append(append([]string(nil), c.handed...), c.noticed...) {
		if it := a.find(key, c); it != nil && comesUp(it.title, transcript) {
			if _, ok := a.mentioned[key]; !ok {
				log.Printf("[Agenda] came up: %s", it.title)
			}
			a.mentioned[key] = today()
		}
	}
	a.saveSaid()
}
