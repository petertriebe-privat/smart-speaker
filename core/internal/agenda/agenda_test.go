package agenda

import (
	"strings"
	"testing"
	"time"

	"echo-assistant/internal/affairs"
	"echo-assistant/internal/netx"
	"echo-assistant/internal/store"
)

// The initiative's view: appointments with a place get their neighbours of the same day, and
// overlapping ones come as pairs (#82).
func TestWithPlaceAndOverlaps(t *testing.T) {
	store.Dir = t.TempDir()
	a := New(nil, func() string { return "p" })
	d := time.Now().AddDate(0, 0, 1)
	at := func(h, m int) time.Time { return time.Date(d.Year(), d.Month(), d.Day(), h, m, 0, 0, time.Local) }
	a.items = []item{
		{key: "shop", title: "Einkaufen", line: "Einkaufen | place: Markt 1", start: at(15, 0), end: at(16, 0)},
		{key: "diy", title: "Baumarkt", line: "Baumarkt | place: Weg 2 | note: Schrauben", start: at(15, 30), end: at(16, 30)},
		{key: "call", title: "Telefonat", line: "Telefonat", start: at(18, 0), end: at(18, 30)},
		{key: "day", title: "Feiertag", line: "Feiertag", start: at(0, 0), end: at(0, 0).AddDate(0, 0, 1), allDay: true},
	}
	got := a.WithPlace(48 * time.Hour)
	if len(got) != 2 || got[1].Key != "diy" || got[1].Place != "Weg 2" {
		t.Fatalf("with place: %+v", got)
	}
	if got[1].Before == nil || got[1].Before.Key != "shop" || got[1].After == nil || got[1].After.Key != "call" {
		t.Fatalf("neighbours: %+v %+v", got[1].Before, got[1].After)
	}
	if got[0].Before != nil {
		t.Fatalf("the first one has no neighbour before: %+v", got[0].Before)
	}
	overlaps := a.Overlaps()
	if len(overlaps) != 1 || overlaps[0][0].Key != "shop" || overlaps[0][1].Key != "diy" {
		t.Fatalf("overlaps: %+v", overlaps)
	}
}

func testAgenda(t *testing.T) (*Agenda, map[string]string, *string) {
	netx.TimeSynced = true
	store.Dir = t.TempDir()
	now := time.Now()
	tomorrow := now.AddDate(0, 0, 1).Format("2006-01-02")
	states := map[string]string{"a1": "open", "a2": "open", "a3": "open"}
	quiet := ""
	a := New(nil, func() string { return "p" })
	a.Affairs = Affairs{
		Relevant: func(reader string) []affairs.Item {
			if reader != "p" {
				return nil
			}
			var out []affairs.Item
			for _, it := range []affairs.Item{
				{ID: "a1", Kind: "deadline", Title: "Mietwagen zurückgeben", Due: tomorrow + " 14:30", State: "open", From: "a mail from Shop"},
				{ID: "a2", Kind: "todo", Title: "Fahrrad reparieren", State: "open", From: "a conversation"},
				{ID: "a3", Kind: "appointment", Title: "Elternabend", Due: now.Format("2006-01-02") + " 23:59", State: "open", From: "the calendar"},
			} {
				if states[it.ID] == "open" {
					out = append(out, it)
				}
			}
			return out
		},
		State: func(id string) string { return states[id] },
		Quiet: func(string) string { return quiet },
	}
	// The calendar event of a3, as a refresh would have linked it.
	ev := now.Add(time.Hour)
	if ev.Day() != now.Day() {
		ev = now
	}
	a.items = []item{{key: "ev1", title: "Elternabend", line: "Elternabend", start: ev, end: ev.Add(time.Hour), affairID: "a3"}}
	a.fetched, a.fetchedAt = true, now
	return a, states, &quiet
}

func TestAffairLines(t *testing.T) {
	a, _, _ := testAgenda(t)
	// The briefing starts at AGENDA_BRIEFING_FROM_HOUR (4): from midnight on the test would fail
	// before that (27.10.2026, 00:06).
	a.Prefer = func(_, what string, start int) int {
		if what == "briefing" {
			return 0
		}
		return start
	}
	now := time.Now()
	items := a.affairItems("p")
	text, order := describe(items[0], now)
	if want := "[a1] deadline tomorrow at 14:30: Mietwagen zurückgeben (from a mail from Shop)"; text != want || order != 1 {
		t.Fatalf("got %q (%d), want %q", text, order, want)
	}
	if text, order := describe(items[1], now); text != "[a2] todo: Fahrrad reparieren (from a conversation)" || order != 3 {
		t.Fatalf("undated: %q (%d)", text, order)
	}
	ctx := a.Context("", true, "speaker")
	if !strings.Contains(ctx, "[a1]") || !strings.Contains(ctx, "[a2]") || strings.Count(ctx, "Elternabend") != 1 ||
		!strings.Contains(ctx, "[a3] today, Elternabend") {
		t.Fatalf("context: %s", ctx)
	}
	if !strings.Contains(ctx, "first conversation with you today") || !strings.Contains(ctx, "Feedback on hints") {
		t.Fatalf("no briefing or hint rules: %s", ctx)
	}
	if ctx := a.Context("q", false, ""); ctx != "" {
		t.Fatalf("someone else got the admin's affairs: %s", ctx)
	}
	// Before hanging up: tomorrow's only in the evening, the undated one never.
	closing := a.Closing("speaker", "")
	if strings.Contains(closing, "[a2]") || (now.Hour() < 18 && strings.Contains(closing, "[a1]")) {
		t.Fatalf("closing: %s", closing)
	}
	// The briefing came: not again today.
	a.ConversationOver("speaker", "Du: Guten Morgen\n")
	if ctx := a.Context("", true, "speaker"); strings.Contains(ctx, "first conversation with you today") {
		t.Fatalf("briefing twice: %s", ctx)
	}
}

func TestSettledAndQuiet(t *testing.T) {
	a, states, quiet := testAgenda(t)
	states["a3"] = "obsolete" // "der Elternabend fällt aus"
	if ctx := a.Context("", true, "speaker"); strings.Contains(ctx, "Elternabend") {
		t.Fatalf("a cancelled event is still in the agenda: %s", ctx)
	}
	*quiet = time.Now().Format("2006-01-02")
	ctx := a.Context("", true, "speaker")
	if !strings.Contains(ctx, "no unasked hints until the end of") || strings.Contains(ctx, "first conversation") {
		t.Fatalf("quiet: %s", ctx)
	}
	if got := a.Closing("speaker", ""); got != "" {
		t.Fatalf("closing while quiet: %s", got)
	}
}
