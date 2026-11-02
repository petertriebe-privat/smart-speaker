package maps

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"echo-assistant/internal/store"
)

func TestDurations(t *testing.T) {
	if seconds("1234s") != 1234 || seconds("") != 0 || seconds("x") != 0 {
		t.Fatal("seconds")
	}
	for in, want := range map[int]string{29: "0 min", 30: "1 min", 3600: "1 h 0 min", 5400: "1 h 30 min"} {
		if got := minutesText(in); got != want {
			t.Errorf("minutesText(%d) = %q, want %q", in, got, want)
		}
	}
}

func TestParseLocal(t *testing.T) {
	today := time.Date(2030, 1, 2, 8, 0, 0, 0, time.Local)
	got, ok := parseLocal("", "09:30", today)
	if !ok || got.Format("2006-01-02 15:04") != "2030-01-02 09:30" {
		t.Fatalf("today: %v %v", got, ok)
	}
	if _, ok := parseLocal("2030-13", "09:30", today); ok {
		t.Error("bad date accepted")
	}
	if _, ok := parseLocal("", "25:00", today); ok {
		t.Error("bad hour accepted")
	}
}

func TestIsHome(t *testing.T) {
	for _, p := range []string{"", "Home", "ZUHAUSE", "daheim"} {
		if !isHome(p) {
			t.Errorf("%q should be home", p)
		}
	}
	if isHome("Hamburg") {
		t.Error("Hamburg is not home")
	}
}

func TestFromUTCAndDayLabel(t *testing.T) {
	if got := fromUTC("2026-09-30T05:19:42.43Z"); got.Unix() != 1790745582 {
		t.Errorf("fromUTC = %v", got.Unix())
	}
	if !fromUTC("garbage").IsZero() {
		t.Error("garbage parsed")
	}
	y, m, d := 2026, 9, 30
	if got := dayLabel(dateDoc{&y, &m, &d}); got != "Mi 30.09." {
		t.Errorf("dayLabel = %q", got)
	}
}

func TestConditions(t *testing.T) {
	var c conditionsDoc
	json.Unmarshal([]byte(`{"weatherCondition":{"description":{"text":"Rain"}},
		"precipitation":{"probability":{"percent":60},"qpf":{"quantity":2.34}},
		"wind":{"speed":{"value":15},"gust":{"value":45}},"thunderstormProbability":25}`), &c)
	want := "Rain, rain 60 % (2.3 mm), wind 15 km/h (gusts 45), thunderstorm 25 %"
	if got := conditions(c); got != want {
		t.Errorf("conditions = %q", got)
	}
	if got := conditions(conditionsDoc{}); got != "?, rain 0 %, wind 0 km/h" {
		t.Errorf("empty conditions = %q", got)
	}
}

func TestConnections(t *testing.T) {
	var steps []step
	json.Unmarshal([]byte(`[
	 {"travelMode":"WALK","staticDuration":"300s"},
	 {"travelMode":"TRANSIT","transitDetails":{"transitLine":{"nameShort":"5","vehicle":{"name":{"text":"Bus"}}},
	  "headsign":"Centre","stopDetails":{"departureStop":{"name":"A"},"arrivalStop":{"name":"B"}},
	  "localizedValues":{"departureTime":{"time":{"text":"08:00"}},"arrivalTime":{"time":{"text":"08:20"}}}}},
	 {"travelMode":"WALK","staticDuration":"120s"}]`), &steps)
	want := "  walk 5 min\n  08:00 Bus 5 towards Centre from A to B, arrives 08:20\n  walk 2 min\n"
	if got := connections(steps); got != want {
		t.Errorf("connections = %q", got)
	}
}

func TestFormatPlaces(t *testing.T) {
	var places []place
	json.Unmarshal([]byte(`[{"displayName":{"text":"Cafe"},"formattedAddress":"Main St 1","rating":4.46,
		"userRatingCount":12,"businessStatus":"CLOSED_TEMPORARILY","currentOpeningHours":{"openNow":false},
		"regularOpeningHours":{"weekdayDescriptions":["Mo 8-9","Di 8-9"]}}]`), &places)
	want := "Places for \"q\":\n- Cafe | Main St 1 | rating 4.5 (12) | CLOSED_TEMPORARILY | closed now | hours: Mo 8-9; Di 8-9\n"
	if got := formatPlaces("q", places); got != want {
		t.Errorf("formatPlaces = %q", got)
	}
	if got := formatPlaces("q", nil); got != "No places found for \"q\"." {
		t.Errorf("empty = %q", got)
	}
}

func TestFormatRoute(t *testing.T) {
	now := time.Date(2030, 1, 2, 8, 0, 0, 0, time.Local)
	r := route{Duration: "1800s", StaticDuration: "1500s", DistanceMeters: 12345, Description: "A1"}
	got := formatRoute(r, "", "Town", "drive", false, now, now)
	want := "By car from home to Town: 30 min with the traffic expected (25 min without), 12.3 km, via A1.\n" +
		"Leaving now, arrival about 08:30.\n"
	if got != want {
		t.Errorf("formatRoute = %q", got)
	}
	late := formatRoute(r, "", "Town", "drive", true, now.Add(10*time.Minute), now)
	if !strings.HasPrefix(late[strings.Index(late, "\n")+1:], "Too late: leaving now, arrival about 08:30 instead of 08:10.") {
		t.Errorf("late = %q", late)
	}
}

func TestQuota(t *testing.T) {
	store.Dir = t.TempDir()
	now := time.Date(2030, 1, 2, 8, 0, 0, 0, time.UTC)
	var warned []string
	q := &Quota{now: func() time.Time { return now }, synced: func() bool { return true },
		Warn: func(text string) { warned = append(warned, text) }}
	for i := 0; i < 10; i++ {
		if err := q.Count("x", 10); err != nil {
			t.Fatalf("request %d: %v", i, err)
		}
	}
	if len(warned) != 1 {
		t.Errorf("warnings: %v", warned)
	}
	err := q.Count("x", 10)
	if err == nil || !strings.HasPrefix(err.Error(), "Error: monthly limit reached (10 x requests") {
		t.Errorf("limit: %v", err)
	}
	now = now.AddDate(0, 1, 0)
	if err := q.Count("x", 10); err != nil {
		t.Errorf("new month: %v", err)
	}
	q.synced = func() bool { return false }
	if q.Count("x", 10) == nil {
		t.Error("unsynced clock accepted")
	}
}
