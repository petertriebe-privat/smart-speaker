package affairs

import (
	"fmt"
	"sort"
	"strings"
	"time"

	"echo-assistant/internal/prompts"
)

// open: still to be brought up today (a snoozed one again from its day on).
func open(a *Affair, today string) bool {
	switch a.State {
	case "open", "waiting":
		return true
	case "snoozed":
		return a.Remind == "" || a.Remind <= today
	}
	return false
}

// Item: an open affair for the agenda.
type Item struct {
	ID, Kind, Title, Due, State string
	From                        string // "a mail from …", "a conversation", "" = unknown
	Mail                        string // Waiting only: the matter's latest mail, "<mailbox>:<ref>"
	Since                       string // Waiting only: how long unchanged, "3 days" / "5 hours"
}

func origin(a *Affair) string {
	for i := len(a.Sources) - 1; i >= 0; i-- {
		switch s := a.Sources[i]; s.Kind {
		case "mail":
			if s.About != "" {
				return "a mail from " + s.About
			}
			return "a mail"
		case "conversation":
			return "a conversation"
		case "calendar":
			return "the calendar"
		case "task":
			if s.About == Own {
				continue // The assistant's own copy says nothing about where it came from
			}
			return "a to-do list"
		}
	}
	return ""
}

// over: an appointment, trip or delivery whose day is past; a deadline, to-do or promise
// AFFAIRS_OVERDUE_DAYS after its day (the nightly review decides on it).
func over(a *Affair, today time.Time) bool {
	d := dueDay(a)
	if d == "" {
		return false
	}
	day, err := time.ParseInLocation("2006-01-02", d, time.Local)
	if err != nil {
		return false
	}
	switch a.Kind {
	case "appointment", "trip", "delivery":
		return day.Before(today)
	}
	return day.AddDate(0, 0, prompts.Int("AFFAIRS_OVERDUE_DAYS", 3)).Before(today)
}

// Relevant: the person's open affairs for the agenda, most relevant first: overdue and due
// soonest, then the undated ones changed last; due beyond AFFAIRS_AHEAD_DAYS left out; at most n.
func (b *Book) Relevant(person string, n int) []Item {
	now := b.Now()
	today := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, time.Local)
	ahead := today.AddDate(0, 0, prompts.Int("AFFAIRS_AHEAD_DAYS", 7)).Format("2006-01-02")
	b.mu.Lock()
	defer b.mu.Unlock()
	var picked []*Affair
	for _, a := range b.st.Affairs {
		if a.Person != person || !open(a, today.Format("2006-01-02")) || over(a, today) || dueDay(a) > ahead {
			continue
		}
		picked = append(picked, a)
	}
	sort.SliceStable(picked, func(i, j int) bool {
		di, dj := picked[i].Due, picked[j].Due
		if (di == "") != (dj == "") {
			return di != ""
		}
		if di != dj {
			return di < dj
		}
		return picked[i].Updated > picked[j].Updated
	})
	if len(picked) > n {
		picked = picked[:n]
	}
	out := make([]Item, len(picked))
	for i, a := range picked {
		out[i] = Item{ID: a.ID, Kind: a.Kind, Title: a.Title, Due: a.Due, State: a.State, From: origin(a)}
	}
	return out
}

// Waiting: the person's affairs that wait for someone else (state waiting, or kind waiting and
// open) and have not changed for days, oldest first: candidates for a follow-up (#82).
func (b *Book) Waiting(person string, days int) []Item {
	limit := b.Now().AddDate(0, 0, -days).Unix()
	b.mu.Lock()
	defer b.mu.Unlock()
	var picked []*Affair
	for _, a := range b.st.Affairs {
		if a.Person == person && a.Updated <= limit && (a.State == "waiting" || a.Kind == "waiting" && a.State == "open") {
			picked = append(picked, a)
		}
	}
	sort.SliceStable(picked, func(i, j int) bool { return picked[i].Updated < picked[j].Updated })
	out := make([]Item, len(picked))
	for i, a := range picked {
		out[i] = Item{ID: a.ID, Kind: a.Kind, Title: a.Title, Due: a.Due, State: a.State, From: origin(a)}
		if age := b.Now().Sub(time.Unix(a.Updated, 0)); age >= 48*time.Hour {
			out[i].Since = fmt.Sprintf("%d days", int(age.Hours()/24))
		} else {
			out[i].Since = fmt.Sprintf("%d hours", int(age.Hours()))
		}
		for _, s := range a.Sources {
			if s.Kind == "mail" {
				out[i].Mail = s.Ref // the latest mail of the matter
			}
		}
	}
	return out
}

// Brief: the open affairs (of one person, "" = everyone's with the person) as data for the
// sorting of mails, the consolidation and the nightly review: id, kind, title, due, the parent,
// where it came from. The newest AFFAIRS_BRIEF_MAX.
func (b *Book) Brief(person string) string {
	b.mu.Lock()
	defer b.mu.Unlock()
	var lines []string
	for _, a := range b.st.Affairs {
		if (person != "" && a.Person != person) || a.Settled() {
			continue
		}
		l := "- " + a.ID + ": " + a.Kind + ", " + a.Title
		if a.Due != "" {
			l += ", due " + a.Due
		}
		if a.State != "open" {
			l += " (" + a.State + ")"
		}
		if a.Parent != "" {
			l += ", part of " + a.Parent
		}
		if o := origin(a); o != "" {
			l += ", from " + o
		}
		if person == "" {
			l += " [" + a.Person + "]"
		}
		lines = append(lines, l)
	}
	if max := prompts.Int("AFFAIRS_BRIEF_MAX", 60); len(lines) > max {
		lines = lines[len(lines)-max:]
	}
	return strings.Join(lines, "\n")
}

func line(a *Affair, now time.Time) string {
	l := "[" + a.ID + "] " + a.Kind + ", " + a.State
	if a.State == "snoozed" && a.Remind != "" {
		l += " until " + a.Remind
	}
	l += ": " + a.Title
	if a.Due != "" {
		l += ", due " + a.Due
	}
	if o := origin(a); o != "" {
		l += " (from " + o + ")"
	}
	if a.Parent != "" {
		l += ", part of " + a.Parent
	}
	return l
}

// Filter for List.
type Filter struct {
	State string // "" or "open" = open, waiting and snoozed; "all"; or one state
	Kind  string // "" = any
	Days  int    // due within that many days from today (overdue included); 0 = any
}

// List: the person's affairs for list_affairs, with their last change.
func (b *Book) List(person string, f Filter) string {
	now := b.Now()
	b.mu.Lock()
	defer b.mu.Unlock()
	until := ""
	if f.Days > 0 {
		until = now.AddDate(0, 0, f.Days).Format("2006-01-02")
	}
	var lines []string
	for _, a := range b.st.Affairs {
		if a.Person != person || (f.Kind != "" && a.Kind != f.Kind) || (until != "" && (a.Due == "" || dueDay(a) > until)) {
			continue
		}
		switch f.State {
		case "", "open":
			if a.Settled() {
				continue
			}
		case "all":
		default:
			if a.State != f.State {
				continue
			}
		}
		l := "- " + line(a, now)
		if n := len(a.History); n > 0 {
			h := a.History[n-1]
			l += "; last change " + time.Unix(h.T, 0).Format("02.01. 15:04") + " by " + h.By + ": " + h.What
			if h.Reason != "" {
				l += " (" + h.Reason + ")"
			}
		}
		lines = append(lines, l)
	}
	if len(lines) == 0 {
		return "No affairs found."
	}
	if q := b.quietLocked(person); q != "" {
		lines = append(lines, "(No unasked hints until the end of "+q+": the user asked for it.)")
	}
	return strings.Join(lines, "\n")
}
