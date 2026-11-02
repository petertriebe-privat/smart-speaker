package memory

// The weekly learning report (26.10.2026): The assistant learns on its own after every conversation and
// at night, and nobody saw what. Everything he learned by himself (facts and profile fields from
// the consolidation, facts and habits from the dream, rules he derived) goes into a learned log
// (memory/learned.json); once a week (LEARN_REPORT_WEEKDAY, at the person's hour from their rules,
// LEARN_REPORT_HOUR to start with) each person gets their part as a numbered message on their
// chat (people.Message). The report stays in their prompts for LEARN_REPORT_DAYS, so an answer
// like "3 stimmt nicht" can be acted on: forget the fact, delete the rule, fix the profile field.

import (
	"encoding/json"
	"fmt"
	"log"
	"strings"
	"time"

	"echo-assistant/internal/prompts"
	"echo-assistant/internal/store"
)

const learnedFile = "memory/learned.json"

// learned is one thing the assistant learned on its own.
type learned struct {
	T      int64  `json:"t"`
	Person string `json:"person,omitempty"` // whom it is about, "" = the household
	Kind   string `json:"kind"`             // fact, profile, rule
	ID     int    `json:"id,omitempty"`     // fact or rule id
	Field  string `json:"field,omitempty"`  // profile field
	Text   string `json:"text"`
}

// reportItem: one numbered line of a sent report.
type reportItem struct {
	N     int    `json:"n"`
	Kind  string `json:"kind"`
	ID    int    `json:"id,omitempty"`
	Field string `json:"field,omitempty"`
	Text  string `json:"text"`
}

type learnReport struct {
	Date  string                  `json:"date"` // the day of the last report
	Items map[string][]reportItem `json:"items"`
}

// noteLearnedLocked adds to the learned log (caller holds m.mu).
func (m *Memory) noteLearnedLocked(person, kind string, id int, field, text string) {
	if strings.TrimSpace(text) == "" {
		return
	}
	var list []learned
	store.LoadJSON(learnedFile, &list)
	now := time.Now().Unix()
	keep := now - 60*86400
	first := 0
	for first < len(list) && list[first].T < keep {
		first++
	}
	list = append(list[first:], learned{T: now, Person: person, Kind: kind, ID: id, Field: field, Text: text})
	if err := store.SaveJSON(learnedFile, list); err != nil {
		log.Printf("[Memory] learned log not saved: %v", err)
	}
}

func (m *Memory) lastReportLocked() learnReport {
	var r learnReport
	json.Unmarshal(m.core.Other["learn_report"], &r)
	return r
}

// learnReportDue: the report's weekday and hour (the admin's, from his rules) and none sent today.
func (m *Memory) learnReportDue() bool {
	day := prompts.Int("LEARN_REPORT_WEEKDAY", 0) // Sunday
	now := time.Now()
	if day < 0 || now.Year() < 2024 || int(now.Weekday()) != day {
		return false
	}
	hour := prompts.Int("LEARN_REPORT_HOUR", 18)
	if m.ReportHour != nil {
		hour = m.ReportHour(m.People.Admin(), hour)
	}
	if now.Hour() < hour {
		return false
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.lastReportLocked().Date != today()
}

// itemText: how an item reads in the report.
func (m *Memory) itemText(l learned) string {
	switch l.Kind {
	case "profile":
		label := l.Field
		for _, f := range profileFields {
			if f.key == l.Field {
				label = f.label
			}
		}
		return label + ": " + l.Text
	case "rule":
		return "Regel (noch vorläufig): " + l.Text
	}
	return l.Text
}

// sendLearnReport sends each person what the assistant learned about them since the last report (the
// admin also what concerns the household). Facts and rules gone since are left out.
func (m *Memory) sendLearnReport() {
	m.mu.Lock()
	last := m.lastReportLocked()
	since := time.Now().Add(-7 * 24 * time.Hour).Unix()
	if t, err := time.ParseInLocation("2006-01-02", last.Date, time.Local); err == nil && t.Unix() > since {
		since = t.Unix()
	}
	var list []learned
	store.LoadJSON(learnedFile, &list)
	admin := m.People.Admin()
	byPerson := map[string][]learned{}
	seen := map[string]bool{}
	for _, l := range list {
		if l.T < since {
			continue
		}
		if l.Kind == "fact" && m.findFactLocked(l.ID) == nil || l.Kind == "rule" && m.findRuleLocked(l.ID) == nil {
			continue // forgotten, merged or removed again
		}
		key := fmt.Sprintf("%s|%s|%d|%s", l.Kind, l.Field, l.ID, l.Person)
		if seen[key] && l.Kind != "profile" {
			continue
		}
		seen[key] = true
		to := l.Person
		if to == "" || m.People.Name(to) == "" {
			to = admin
		}
		byPerson[to] = append(byPerson[to], l)
	}
	report := learnReport{Date: today(), Items: map[string][]reportItem{}}
	if raw, err := json.Marshal(report); err == nil {
		m.core.Other["learn_report"] = raw // sent or not: once today
	}
	m.saveLocked()
	m.mu.Unlock()

	limit := prompts.Int("LEARN_REPORT_MAX", 15)
	for person, items := range byPerson {
		if len(items) > limit {
			items = items[len(items)-limit:] // the newest
		}
		var b strings.Builder
		b.WriteString("Das habe ich in den letzten Tagen über dich dazugelernt:\n")
		var numbered []reportItem
		for i, l := range items {
			fmt.Fprintf(&b, "%d. %s\n", i+1, m.itemText(l))
			numbered = append(numbered, reportItem{N: i + 1, Kind: l.Kind, ID: l.ID, Field: l.Field, Text: l.Text})
		}
		b.WriteString("\nStimmt etwas nicht? Antworte einfach, z. B. „2 stimmt nicht“ oder „3: lieber Klassik“.")
		via := m.People.Message(person, b.String())
		log.Printf("[Memory] learning report to %s (%d items): %s", person, len(items), via)
		if strings.HasPrefix(via, "Error") {
			continue
		}
		report.Items[person] = numbered
	}
	m.mu.Lock()
	if raw, err := json.Marshal(report); err == nil {
		m.core.Other["learn_report"] = raw
	}
	m.saveLocked()
	m.mu.Unlock()
}

// learnReportContextLocked: the person's last report for LEARN_REPORT_DAYS, so they can correct it.
func (m *Memory) learnReportContextLocked(person string) string {
	with := m.speaker(person)
	if with == "" {
		return ""
	}
	r := m.lastReportLocked()
	items := r.Items[with]
	sent, err := time.ParseInLocation("2006-01-02", r.Date, time.Local)
	if len(items) == 0 || err != nil || time.Since(sent) > time.Duration(prompts.Int("LEARN_REPORT_DAYS", 4))*24*time.Hour {
		return ""
	}
	var b strings.Builder
	b.WriteString("Your weekly report of what you learned on your own, sent to them on " + r.Date + " as a numbered " +
		"message. When they correct an item (\"2 stimmt nicht\", \"3: lieber Klassik\"): a fact: forget it by its id " +
		"(and remember the right one); a rule: delete_rule (or save_rule with replaces); a profile field: " +
		"update_profile. Thank them briefly.\n")
	for _, it := range items {
		ref := ""
		switch it.Kind {
		case "fact":
			ref = fmt.Sprintf("fact %d", it.ID)
		case "rule":
			ref = fmt.Sprintf("rule %d", it.ID)
		default:
			ref = "profile " + it.Field
		}
		fmt.Fprintf(&b, "%d. [%s] %s\n", it.N, ref, it.Text)
	}
	return b.String()
}
