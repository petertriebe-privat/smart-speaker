package tasks

import (
	"testing"
	"time"
)

func TestNextRun(t *testing.T) {
	loc, _ := time.LoadLocation("Europe/Berlin")
	time.Local = loc
	// Friday 16.10.2026, 02:00
	now := time.Date(2026, 10, 16, 2, 0, 0, 0, loc)
	cases := []struct {
		sc   Schedule
		want string
	}{
		{Schedule{Type: "once", Time: "06:30"}, "2026-10-16 06:30"},
		{Schedule{Type: "once", Time: "01:00"}, "2026-10-17 01:00"},
		{Schedule{Type: "once", Time: "08:00", Date: "2026-12-24"}, "2026-12-24 08:00"},
		{Schedule{Type: "daily", Time: "01:59"}, "2026-10-17 01:59"},
		{Schedule{Type: "weekly", Time: "07:00", Days: []int{1}}, "2026-10-19 07:00"},
		{Schedule{Type: "weekly", Time: "03:00", Days: []int{5}}, "2026-10-16 03:00"},
		{Schedule{Type: "monthly", Time: "09:00", DayOfMonth: 31}, "2026-10-31 09:00"},
		{Schedule{Type: "monthly", Time: "09:00", DayOfMonth: 15}, "2026-11-15 09:00"},
		{Schedule{Type: "interval", EveryHours: 3}, "2026-10-16 05:00"},
		// DST ends on 25.10.2026: still 07:00 local time.
		{Schedule{Type: "once", Time: "07:00", Date: "2026-10-25"}, "2026-10-25 07:00"},
	}
	for _, c := range cases {
		got, err := NextRun(c.sc, now)
		if err != "" || got.Format("2006-01-02 15:04") != c.want {
			t.Errorf("%+v: got %s (%q), want %s", c.sc, got.Format("2006-01-02 15:04"), err, c.want)
		}
	}
	if got, _ := NextRun(Schedule{Type: "once", Time: "08:00", Date: "2026-01-01"}, now); !got.IsZero() {
		t.Errorf("past date: %v", got)
	}
	if _, err := NextRun(Schedule{Type: "weekly", Time: "08:00"}, now); err == "" {
		t.Error("weekly without days must fail")
	}
}
