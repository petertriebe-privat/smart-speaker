package vault

import (
	"strings"
	"testing"
)

func TestWeek(t *testing.T) {
	key, monday, sunday, ok := Week("2026-11-14 18:38")
	if !ok || key != "2026-W46" || monday.Format("2006-01-02") != "2026-11-09" || sunday.Format("2006-01-02") != "2026-11-15" {
		t.Errorf("Week: %v %q %v %v", ok, key, monday, sunday)
	}
	if key, _, _, _ := Week("2026-01-01 08:00"); key != "2026-W01" {
		t.Errorf("new year: %q", key)
	}
	if _, _, _, ok := Week("gestern"); ok {
		t.Error("no time")
	}
}

func TestParseConversation(t *testing.T) {
	in := Conversation{At: "2026-11-14 18:38", Title: "Heizung", With: "Anna", Channel: "Telegram",
		Summary: "Die Heizung wird entlüftet.", Keywords: "heizung", Topics: []string{"Heizung: Keller", "Wärme"},
		Transcript: "# keine Überschrift\nAnna: Kalt."}
	c := ParseConversation("2026-11-14 18.38 – Heizung.md", in.Markdown())
	if c.At != in.At || c.Title != "Heizung" || c.With != "Anna" || c.Channel != "Telegram" || c.Summary != in.Summary ||
		strings.Join(c.Topics, "|") != "Heizung: Keller|Wärme" {
		t.Errorf("parsed: %+v", c)
	}
	raw := ParseConversation("2026-11-14 18.38 – nicht zusammengefasst.md", Conversation{At: "2026-11-14 18:38", Transcript: "x"}.Markdown())
	if raw.Title != "Gespräch" || raw.Summary != "" {
		t.Errorf("not summarized: %+v", raw)
	}
}

func TestWeekNote(t *testing.T) {
	_, monday, sunday, _ := Week("2026-11-14 18:38")
	a := WeekLine(Conversation{At: "2026-11-14 18:38", Title: "Heizung", With: "Anna", Summary: "Entlüftet.",
		Topics: []string{"Heizung"}})
	b := WeekLine(Conversation{At: "2026-11-10 07:00", Title: "Gespräch"})
	if a != "- **2026-11-14 18:38** Heizung — Entlüftet. (mit [[Anna]]; Themen: [[Heizung]])" ||
		b != "- **2026-11-10 07:00** Gespräch — nicht zusammengefasst" {
		t.Errorf("lines:\n%s\n%s", a, b)
	}
	first := WeekNote("2026-W46", monday, sunday, "", []string{a})
	note := WeekNote("2026-W46", monday, sunday, first, []string{b, a}) // a again: once
	for _, want := range []string{"typ: \"Woche\"\nwoche: \"2026-W46\"\nvon: 2026-11-09\nbis: 2026-11-15\ngespräche: 2\n",
		"personen:\n  - \"[[Anna]]\"\n", "themen:\n  - \"[[Heizung]]\"\n", "# Woche 2026-W46 (09.11.–15.11.2026)",
		"## Gespräche\n\n" + b + "\n" + a + "\n"} {
		if !strings.Contains(note, want) {
			t.Errorf("missing %q in\n%s", want, note)
		}
	}
	if WeekName("2026-W46") != "Woche 2026-W46" {
		t.Error(WeekName("2026-W46"))
	}
}
