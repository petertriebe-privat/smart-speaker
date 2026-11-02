package affairs

import (
	"strings"
	"testing"
	"time"
)

func book(now string) *Book {
	t, _ := time.ParseInLocation("2006-01-02 15:04", now, time.Local)
	return &Book{Now: func() time.Time { return t }}
}

func mailSource(ref string) Source { return Source{Kind: "mail", Ref: "Box:" + ref, About: "Shop"} }

func talk(at string) Source { return Source{Kind: "conversation", Ref: at} }

func TestIntakeNewAndSameSource(t *testing.T) {
	b := book("2026-10-19 12:00")
	a := b.Intake("p", Candidate{Kind: "deadline", Title: "Rechnung bezahlen", Due: "2026-10-25", Source: mailSource("1")}, "mail")
	if a == nil || a.ID != "a1" || a.State != "open" || a.Due != "2026-10-25" {
		t.Fatalf("new affair: %+v", a)
	}
	// The same mail again (sorted twice): no second affair.
	if again := b.Intake("p", Candidate{Kind: "deadline", Title: "Rechnung", Due: "2026-10-25", Source: mailSource("1")}, "mail"); again.ID != "a1" {
		t.Fatalf("same source made another affair")
	}
	// A reminder mail with a similar title on the same day: the same affair, two sources.
	if r := b.Intake("p", Candidate{Kind: "deadline", Title: "Erinnerung: Rechnung bezahlen", Due: "2026-10-25", Source: mailSource("2")}, "mail"); r.ID != "a1" || len(r.Sources) != 2 {
		t.Fatalf("reminder mail: %+v", r)
	}
	// Matched by id with a new date.
	b.Intake("p", Candidate{Match: "a1", Kind: "deadline", Title: "Mahnung", Due: "2026-10-30", Source: mailSource("3")}, "mail")
	if a := b.find("a1"); a.Due != "2026-10-30" || len(b.st.Affairs) != 1 {
		t.Fatalf("matched: %+v (%d affairs)", a, len(b.st.Affairs))
	}
	// Someone else's id is not theirs.
	if o := b.Intake("q", Candidate{Match: "a1", Kind: "todo", Title: "Anderes", Source: mailSource("4")}, "mail"); o.ID == "a1" {
		t.Fatalf("matched another person's affair")
	}
}

func TestIntakeSettlesNeverReopens(t *testing.T) {
	b := book("2026-10-19 12:00")
	var settled []string
	b.OnSettled = func(a Affair) { settled = append(settled, a.ID) }
	trip := b.Intake("p", Candidate{Kind: "trip", Title: "Reise", Due: "2026-10-20", Source: talk("x")}, "conversation")
	car := b.Intake("p", Candidate{Kind: "deadline", Title: "Mietwagen zurückgeben", Due: "2026-10-20 14:30", Parent: trip.ID,
		Source: mailSource("8")}, "mail")
	if car.Parent != trip.ID {
		t.Fatalf("parent not kept: %+v", car)
	}
	b.Intake("p", Candidate{Match: trip.ID, State: "obsolete", Reason: "nicht geflogen", Source: talk("y")}, "conversation")
	if b.find(trip.ID).State != "obsolete" || b.find(car.ID).State != "obsolete" {
		t.Fatalf("trip %s, car %s", b.find(trip.ID).State, b.find(car.ID).State)
	}
	if strings.Join(settled, ",") != "a1,a2" {
		t.Fatalf("settled hook: %v", settled)
	}
	// A reminder mail of the car: no new affair, nothing reopened.
	r := b.Intake("p", Candidate{Kind: "deadline", Title: "Mietwagen zurückgeben", Due: "2026-10-20", State: "open", Source: mailSource("9")}, "mail")
	if r.ID != car.ID || b.find(car.ID).State != "obsolete" || len(b.st.Affairs) != 2 {
		t.Fatalf("a mail reopened or doubled a settled affair: %+v", r)
	}
	if b.Intake("p", Candidate{State: "done", Title: "Unbekannt"}, "conversation") != nil {
		t.Fatalf("a done candidate made an affair")
	}
	h := b.find(car.ID).History
	if got := h[len(h)-1]; !strings.Contains(got.What, "with a1") || got.By != "conversation" {
		t.Fatalf("history: %+v", h)
	}
}

func TestUpdateAndPropagation(t *testing.T) {
	b := book("2026-10-19 12:00")
	trip := b.Intake("p", Candidate{Kind: "trip", Title: "Reise nach Süden", Due: "2026-10-18", Source: talk("1")}, "conversation")
	car := b.Intake("p", Candidate{Kind: "deadline", Title: "Mietwagen zurückgeben", Due: "2026-10-20 14:30", Source: mailSource("1")}, "mail")
	hotel := b.Intake("p", Candidate{Kind: "appointment", Title: "Hotel einchecken", Due: "2026-10-18", Source: mailSource("2")}, "mail")
	if _, err := b.Update("q", car.ID, Changes{State: "obsolete"}, "user"); err == nil {
		t.Fatalf("another person changed it")
	}
	if _, err := b.Update("p", trip.ID, Changes{Parent: car.ID}, "user"); err != nil {
		t.Fatal(err)
	}
	if _, err := b.Update("p", car.ID, Changes{Parent: trip.ID}, "user"); err == nil {
		t.Fatalf("a cycle was allowed")
	}
	b.Update("p", trip.ID, Changes{Parent: "none-such"}, "user")
	b.find(trip.ID).Parent = "" // undo the link for the rest
	b.Update("p", car.ID, Changes{Parent: trip.ID}, "user")
	b.Update("p", hotel.ID, Changes{Parent: trip.ID}, "user")
	out, err := b.Update("p", "["+trip.ID+"]", Changes{State: "obsolete", Reason: "Reise fand nicht statt"}, "user")
	if err != nil || !strings.Contains(out, "Also obsolete") || strings.Count(out, "Also") != 2 {
		t.Fatalf("update: %q %v", out, err)
	}
	if len(b.Relevant("p", 5)) != 0 {
		t.Fatalf("obsolete affairs are still relevant: %+v", b.Relevant("p", 5))
	}
	// Undo: open again (the children stay as they are: the user reopens what still matters).
	b.Update("p", trip.ID, Changes{State: "open", Reason: "doch"}, "user")
	if b.find(trip.ID).State != "open" || b.find(car.ID).State != "obsolete" {
		t.Fatalf("undo")
	}
	// A new child of an obsolete parent goes along.
	b.Update("p", trip.ID, Changes{State: "obsolete"}, "user")
	park := b.Intake("p", Candidate{Kind: "todo", Title: "Parkplatz buchen", Source: talk("2")}, "conversation")
	b.Update("p", park.ID, Changes{Parent: trip.ID}, "user")
	if b.find(park.ID).State != "obsolete" {
		t.Fatalf("child of an obsolete parent stayed open")
	}
	if _, err := b.Update("p", car.ID, Changes{Due: "morgen"}, "user"); err == nil {
		t.Fatalf("a bad due date was taken")
	}
}

func TestSnooze(t *testing.T) {
	b := book("2026-10-19 12:00")
	a := b.Intake("p", Candidate{Kind: "deadline", Title: "Formular", Due: "2026-10-20", Source: mailSource("1")}, "mail")
	b.Update("p", a.ID, Changes{State: "snoozed", Remind: "2026-10-22"}, "user")
	if a := b.find(a.ID); a.State != "snoozed" || a.Remind != "2026-10-22" || len(b.Relevant("p", 5)) != 0 {
		t.Fatalf("snoozed: %+v", a)
	}
	b.Now = func() time.Time { return time.Date(2026, 10, 22, 8, 0, 0, 0, time.Local) }
	// Back from its remind day on; a deadline two days over is still shown (AFFAIRS_OVERDUE_DAYS 3).
	if items := b.Relevant("p", 5); len(items) != 1 {
		t.Fatalf("snoozed affair not back: %+v", items)
	}
}

func TestRelevantOrder(t *testing.T) {
	b := book("2026-10-19 12:00")
	b.Intake("p", Candidate{Kind: "todo", Title: "Ohne Datum", Source: talk("1")}, "conversation")
	b.Intake("p", Candidate{Kind: "deadline", Title: "Übermorgen", Due: "2026-10-21", Source: mailSource("1")}, "mail")
	b.Intake("p", Candidate{Kind: "deadline", Title: "Überfällig", Due: "2026-10-18", Source: mailSource("2")}, "mail")
	b.Intake("p", Candidate{Kind: "appointment", Title: "Vorbei", Due: "2026-10-18 10:00", Source: mailSource("3")}, "mail")
	b.Intake("p", Candidate{Kind: "deadline", Title: "Weit weg", Due: "2026-12-01", Source: mailSource("4")}, "mail")
	b.Intake("p", Candidate{Kind: "deadline", Title: "Lange überfällig", Due: "2026-10-01", Source: mailSource("5")}, "mail")
	var titles []string
	for _, it := range b.Relevant("p", 5) {
		titles = append(titles, it.Title)
	}
	if got := strings.Join(titles, "|"); got != "Überfällig|Übermorgen|Ohne Datum" {
		t.Fatalf("relevant: %s", got)
	}
	if items := b.Relevant("p", 1); len(items) != 1 || items[0].From != "a mail from Shop" {
		t.Fatalf("limit or origin: %+v", items)
	}
}

func TestListFilter(t *testing.T) {
	b := book("2026-10-19 12:00")
	a := b.Intake("p", Candidate{Kind: "deadline", Title: "Steuer", Due: "2026-10-25", Source: mailSource("1")}, "mail")
	b.Intake("p", Candidate{Kind: "todo", Title: "Fahrrad", Source: talk("c")}, "conversation")
	b.Update("p", a.ID, Changes{State: "done", Reason: "bezahlt"}, "user")
	if got := b.List("p", Filter{}); strings.Contains(got, "Steuer") || !strings.Contains(got, "Fahrrad") {
		t.Fatalf("open: %s", got)
	}
	if got := b.List("p", Filter{State: "done"}); !strings.Contains(got, "Steuer") || !strings.Contains(got, "bezahlt") {
		t.Fatalf("done: %s", got)
	}
	if got := b.List("p", Filter{State: "all", Days: 10}); !strings.Contains(got, "Steuer") || strings.Contains(got, "Fahrrad") {
		t.Fatalf("days: %s", got)
	}
}

func TestObserveCalendar(t *testing.T) {
	b := book("2026-10-19 12:00")
	var made int
	b.OnNew = func(Affair) { made++ }
	booking := b.Intake("p", Candidate{Kind: "appointment", Title: "Zahnarzt Kontrolle", Due: "2026-10-20 09:00", Source: mailSource("1")}, "mail")
	made = 0
	// The calendar event of the booking: linked, not a second affair.
	id, state := b.Observe("p", Source{Kind: "calendar", Ref: "ev1"}, "appointment", "Kontrolle beim Zahnarzt", "2026-10-20 09:00")
	if id != booking.ID || state != "open" {
		t.Fatalf("not linked: %s %s", id, state)
	}
	// An unrelated event: its own affair, no own to-do for it.
	other, _ := b.Observe("p", Source{Kind: "calendar", Ref: "ev2"}, "appointment", "Elternabend", "2026-10-20 19:00")
	if other == booking.ID || made != 0 {
		t.Fatalf("other event: %s, made %d", other, made)
	}
	if again, _ := b.Observe("p", Source{Kind: "calendar", Ref: "ev2"}, "appointment", "Elternabend", "2026-10-20 19:00"); again != other {
		t.Fatalf("observed twice: %s %s", again, other)
	}
	// Cancelled: the event is reported settled; the trust decides what the answer offers.
	out, _ := b.Update("p", other, Changes{State: "obsolete", Reason: "fällt aus"}, "user")
	if !strings.Contains(out, "calendar event itself is unchanged") {
		t.Fatalf("hide: %s", out)
	}
	if _, st := b.Observe("p", Source{Kind: "calendar", Ref: "ev2"}, "appointment", "Elternabend", "2026-10-20 19:00"); st != "obsolete" {
		t.Fatalf("state: %s", st)
	}
	b.SetTrust("p", "suggest")
	b.Update("p", other, Changes{State: "open"}, "user")
	out, _ = b.Update("p", other, Changes{State: "obsolete"}, "user")
	if !strings.Contains(out, "delete_event, id ev2") {
		t.Fatalf("suggest: %s", out)
	}
	if err := b.SetTrust("p", "everything"); err == nil {
		t.Fatalf("unknown trust level taken")
	}
}

func TestQuiet(t *testing.T) {
	b := book("2026-10-19 12:00")
	if b.Quiet("p") != "" {
		t.Fatal("quiet without asking")
	}
	b.SetQuiet("p", "")
	if b.Quiet("p") != "2026-10-19" {
		t.Fatalf("today: %q", b.Quiet("p"))
	}
	b.Now = func() time.Time { return time.Date(2026, 10, 20, 7, 0, 0, 0, time.Local) }
	if b.Quiet("p") != "" {
		t.Fatal("still quiet the next day")
	}
	b.SetQuiet("p", "2026-10-25")
	b.SetQuiet("p", "off")
	if b.Quiet("p") != "" {
		t.Fatal("off did not lift it")
	}
	if _, err := b.SetQuiet("p", "Montag"); err == nil {
		t.Fatal("a bad day was taken")
	}
}

func TestSimilar(t *testing.T) {
	for _, c := range []struct {
		a, b string
		want bool
	}{
		{"Rechnung bezahlen", "rechnung bezahlen", true},
		{"Zahnarzt", "Termin Zahnarzt", true},
		{"Zahnarzt Kontrolle", "Kontrolle beim Zahnarzt", true},
		{"Elternabend", "Zahnarzt", false},
		{"Termin Bank", "Termin Post", false},
	} {
		if got := similar(c.a, c.b); got != c.want {
			t.Errorf("similar(%q, %q) = %v", c.a, c.b, got)
		}
	}
}

// Waiting: affairs that wait for someone else and have not changed for days, oldest first.
func TestWaiting(t *testing.T) {
	b := book("2026-10-10 12:00")
	b.Intake("p", Candidate{Kind: "waiting", Title: "Antwort vom Vermieter", Source: mailSource("1")}, "mail")
	b.Intake("p", Candidate{Kind: "todo", Title: "Angebot einholen", State: "waiting", Source: mailSource("2")}, "mail")
	b.Intake("p", Candidate{Kind: "todo", Title: "Steuer machen", Source: mailSource("3")}, "mail")
	b.Intake("q", Candidate{Kind: "waiting", Title: "Paket von Anna", Source: mailSource("4")}, "mail")
	if got := b.Waiting("p", 4); len(got) != 0 {
		t.Fatalf("waiting too soon: %+v", got)
	}
	later, _ := time.ParseInLocation("2006-01-02 15:04", "2026-10-15 12:00", time.Local)
	b.Now = func() time.Time { return later }
	got := b.Waiting("p", 4)
	if len(got) != 2 || got[0].ID != "a1" || got[1].ID != "a2" || got[0].Mail != "Box:1" {
		t.Fatalf("waiting: %+v", got)
	}
	b.Update("p", "a1", Changes{State: "done", Reason: "Antwort kam"}, "user")
	if got := b.Waiting("p", 4); len(got) != 1 || got[0].ID != "a2" {
		t.Fatalf("a settled one is still waiting: %+v", got)
	}
}
