package google

import (
	"strings"
	"testing"
	"time"

	"echo-assistant/internal/backup"
	"echo-assistant/internal/memory"
)

// The Drive types satisfy the interfaces of the packages that use them.
var (
	_ backup.Target  = (*BackupTarget)(nil)
	_ memory.Archive = (*Archive)(nil)
)

func TestParseLocal(t *testing.T) {
	good := []struct{ date, clock string }{{"2026-09-28", ""}, {"2026-09-28", "09:30"}, {"2024-02-29", "23:59"}}
	for _, c := range good {
		if _, ok := parseLocal(c.date, c.clock); !ok {
			t.Errorf("parseLocal(%q, %q) refused", c.date, c.clock)
		}
	}
	bad := []struct{ date, clock string }{{"2026-02-30", ""}, {"2038-01-01", ""}, {"abc", ""}, {"2026-09-28", "24:00"},
		{"2026-09-28", "9"}, {"1969-12-31", ""}, {"", ""}}
	for _, c := range bad {
		if _, ok := parseLocal(c.date, c.clock); ok {
			t.Errorf("parseLocal(%q, %q) accepted", c.date, c.clock)
		}
	}
}

func TestDayName(t *testing.T) {
	if got := dayName("2026-09-28"); got != "Mo 28.09.2026" {
		t.Errorf("dayName = %q", got)
	}
	if got := dayName("garbage"); got != "garbage" {
		t.Errorf("dayName(garbage) = %q", got)
	}
}

func TestDescribeLine(t *testing.T) {
	timed := map[string]any{
		"summary":     "Dentist",
		"location":    "Main Street",
		"description": "bring card\nand x-rays",
		"start":       map[string]any{"dateTime": "2026-09-28T09:00:00+02:00"},
		"end":         map[string]any{"dateTime": "2026-09-28T10:30:00+02:00"},
	}
	want := "- Mo 28.09.2026 09:00–10:30: Dentist | place: Main Street | note: bring card and x-rays"
	if got := describeLine(timed); got != want {
		t.Errorf("timed:\n got %q\nwant %q", got, want)
	}
	overnight := map[string]any{
		"start":            map[string]any{"dateTime": "2026-09-28T22:00:00+02:00"},
		"end":              map[string]any{"dateTime": "2026-09-29T01:00:00+02:00"},
		"recurringEventId": "x",
	}
	want = "- Mo 28.09.2026 22:00–Di 29.09.2026 01:00: (no title) | recurring"
	if got := describeLine(overnight); got != want {
		t.Errorf("overnight:\n got %q\nwant %q", got, want)
	}
	allDay := map[string]any{
		"summary": "Holiday",
		"start":   map[string]any{"date": "2026-10-03"},
		"end":     map[string]any{"date": "2026-10-06"},
	}
	want = "- Sa 03.10.2026 to Mo 05.10.2026 all day: Holiday"
	if got := describeLine(allDay); got != want {
		t.Errorf("all day:\n got %q\nwant %q", got, want)
	}
	oneDay := map[string]any{"summary": "X", "start": map[string]any{"date": "2026-10-03"}, "end": map[string]any{"date": "2026-10-04"}}
	if got := describeLine(oneDay); got != "- Sa 03.10.2026 all day: X" {
		t.Errorf("one day: %q", got)
	}
}

func TestDescribeFlags(t *testing.T) {
	event := map[string]any{
		"id": "e1", "summary": "Fair", "transparency": "transparent",
		"start": map[string]any{"date": "2026-10-03"}, "end": map[string]any{"date": "2026-10-04"},
	}
	main := mainCalendar()
	if got := describe(event, main); !strings.HasSuffix(got, "Fair | info [id e1]\n") {
		t.Errorf("main: %q", got)
	}
	other := calendar{ID: "abc@group", Label: "Family", Own: true}
	if got := describe(event, other); !strings.HasSuffix(got, "Fair | calendar: Family | info [id abc@group/e1]\n") {
		t.Errorf("other: %q", got)
	}
	readOnly := calendar{ID: "holidays", Label: "Holidays"}
	if got := describe(event, readOnly); strings.Contains(got, "[id") {
		t.Errorf("read-only calendar has an id: %q", got)
	}
	event["attendees"] = []any{map[string]any{"self": true, "responseStatus": "declined"}}
	delete(event, "transparency")
	if got := describe(event, main); !strings.Contains(got, " | info | declined [id e1]") {
		t.Errorf("declined: %q", got)
	}
}

func TestSetTimes(t *testing.T) {
	event := map[string]any{"start": 1, "end": 2}
	if msg := setTimes(event, "2026-10-03", "", 60, "2026-10-05"); msg != "" {
		t.Fatal(msg)
	}
	if digS(event, "start", "date") != "2026-10-03" || digS(event, "end", "date") != "2026-10-06" {
		t.Errorf("all-day range: %v", event)
	}
	if msg := setTimes(event, "2026-10-03", "14:00", 90, ""); msg != "" {
		t.Fatal(msg)
	}
	if digS(event, "start", "dateTime") != "2026-10-03T14:00:00" || digS(event, "end", "dateTime") != "2026-10-03T15:30:00" {
		t.Errorf("timed: %v", event)
	}
	if setTimes(event, "2026-10-03", "", 60, "2026-10-01") != badDate || setTimes(event, "nope", "", 60, "") != badDate {
		t.Error("bad dates accepted")
	}
	if !strings.HasPrefix(setTimes(event, "2026-10-03", "14:00", 0, ""), "Error: duration_minutes") {
		t.Error("zero duration accepted")
	}
}

func TestSplitEventID(t *testing.T) {
	cal, ev := splitEventID("abc@group.calendar.google.com/ev1")
	if cal != "abc@group.calendar.google.com" || ev != "ev1" {
		t.Errorf("%q %q", cal, ev)
	}
	if cal, ev = splitEventID("ev2"); cal != "primary" || ev != "ev2" {
		t.Errorf("%q %q", cal, ev)
	}
}

func TestFreeSlots(t *testing.T) {
	day := func(clock string) time.Time {
		v, _ := parseLocal("2030-03-04", clock)
		return v
	}
	busy := []interval{{day("09:00"), day("10:00")}, {day("12:00"), day("13:00")}, {day("19:30"), day("21:00")}}
	text, slots := freeSlots(busy, day(""), day(""), "08:00", "20:00", 60*time.Minute, time.Date(2030, 1, 1, 0, 0, 0, 0, time.Local))
	want := "- Mo 04.03.2030: 08:00–09:00, 10:00–12:00, 13:00–19:30\n"
	if text != want || slots != 3 {
		t.Errorf("got %q (%d), want %q", text, slots, want)
	}
	text, _ = freeSlots(busy, day(""), day(""), "08:00", "20:00", 150*time.Minute, time.Date(2030, 1, 1, 0, 0, 0, 0, time.Local))
	if text != "- Mo 04.03.2030: 13:00–19:30\n" {
		t.Errorf("long: %q", text)
	}
}

func TestDueValueAndTasks(t *testing.T) {
	if got := dueValue("2026-09-05"); got != "2026-09-05T00:00:00.000Z" {
		t.Errorf("dueValue = %q", got)
	}
	if dueValue("2026-13-05") != "" || dueValue("x") != "" {
		t.Error("bad due accepted")
	}
	task := map[string]any{"id": "t1", "title": "Buy nails", "due": "2026-09-05T00:00:00.000Z", "notes": "a\nb", "status": "completed"}
	want := "- Buy nails | due 05.09.2026 | done | note: a b [id L/t1]\n"
	if got := describeTask(task, "L"); got != want {
		t.Errorf("describeTask = %q", got)
	}
	if _, _, ok := splitTaskID("nolist"); ok {
		t.Error("id without list accepted")
	}
	if l, k, ok := splitTaskID("L/t1"); !ok || l != "L" || k != "t1" {
		t.Error("splitTaskID")
	}
}

func TestNormalizedNumber(t *testing.T) {
	for _, in := range []string{"0171 1234567", "+49 171 1234567", "0049171-1234567"} {
		if got := normalizedNumber(in); got != "01711234567" {
			t.Errorf("normalizedNumber(%q) = %q", in, got)
		}
	}
}

func TestBirthday(t *testing.T) {
	if got := birthday(map[string]any{"day": 5.0, "month": 9.0, "year": 1980.0}); got != "05.09.1980" {
		t.Error(got)
	}
	if got := birthday(map[string]any{"day": 5.0, "month": 9.0}); got != "05.09." {
		t.Error(got)
	}
	if birthday(nil) != "" {
		t.Error("empty date")
	}
}

func TestQueries(t *testing.T) {
	if got := Quoted(`it's a \ test`); got != `'it\'s a \\ test'` {
		t.Errorf("Quoted = %q", got)
	}
	if got := propertyQuery("topic", "k-1"); got != "appProperties has { key='topic' and value='k-1' }" {
		t.Errorf("propertyQuery = %q", got)
	}
	if got := enc("a b/c&ü"); got != "a%20b%2Fc%26%C3%BC" {
		t.Errorf("enc = %q", got)
	}
}

func TestUpdatedRow(t *testing.T) {
	if updatedRow("Tabellenblatt1!A5:C5") != 5 || updatedRow("Sheet1!A12") != 12 || updatedRow("") != 0 {
		t.Error("updatedRow")
	}
}

func TestRowText(t *testing.T) {
	if got := rowText([]any{"a", 2.5, 3.0, nil}); got != "a | 2.5 | 3 | " {
		t.Errorf("rowText = %q", got)
	}
}

func TestCodeChallenge(t *testing.T) {
	// RFC 7636 appendix B.
	if got := codeChallenge("dBjftJeZ4CVP-mB92K27uhbUJU1p1r_wW1gFWFOEjXk"); got != "E9Melhoa2OwvFrEMTJguCHaoeK1t8URWbuGJSstw-cM" {
		t.Errorf("codeChallenge = %q", got)
	}
}

func TestMatchesWords(t *testing.T) {
	event := map[string]any{"summary": "1. Elternbeiratssitzung", "location": "Realschule"}
	if !matchesWords(event, "Elternbeirat") || !matchesWords(event, "elternbeirat realschule") {
		t.Error("part of a compound word not found")
	}
	if matchesWords(event, "Elternabend") || matchesWords(event, " ") {
		t.Error("matched without the word")
	}
}
