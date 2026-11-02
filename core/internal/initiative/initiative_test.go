package initiative

import (
	"errors"
	"strings"
	"testing"
	"time"
)

func testJournal(now *time.Time) *Journal {
	return &Journal{Now: func() time.Time { return *now }}
}

// A matter is recorded once per key, however often the sources find it.
func TestAddOncePerKey(t *testing.T) {
	now := time.Date(2026, 10, 24, 10, 0, 0, 0, time.Local)
	j := testJournal(&now)
	c := Candidate{Key: "follow_up:a1", Kind: "follow_up", Person: "admin", Text: "waits"}
	s, isNew := j.Add(c, "dry")
	if !isNew || s.ID != "s1" || s.State != "dry" {
		t.Fatalf("first add: %+v %v", s, isNew)
	}
	if _, isNew := j.Add(c, "dry"); isNew {
		t.Fatal("the same key was added twice")
	}
	if s, _ := j.Add(Candidate{Key: "follow_up:a2", Kind: "follow_up"}, "dry"); s.ID != "s2" {
		t.Fatalf("second key: %s", s.ID)
	}
}

// Only reactions count, per person and kind.
func TestTally(t *testing.T) {
	now := time.Date(2026, 10, 24, 10, 0, 0, 0, time.Local)
	j := testJournal(&now)
	for i, st := range []string{"accepted", "accepted", "declined", "dry"} {
		s, _ := j.Add(Candidate{Key: string(rune('a' + i)), Kind: "follow_up", Person: "admin"}, "dry")
		if err := j.Set(s.ID, st, ""); err != nil {
			t.Fatal(err)
		}
	}
	j.Add(Candidate{Key: "other", Kind: "leave_time", Person: "admin"}, "dry")
	got := j.Tally("admin", "follow_up")
	if got["accepted"] != 2 || got["declined"] != 1 || got["ignored"] != 0 || len(got) != 2 {
		t.Fatalf("tally: %v", got)
	}
	if len(j.Tally("someone", "follow_up")) != 0 {
		t.Fatal("another person's tally is not empty")
	}
	if err := j.Set("s1", "maybe", ""); err == nil {
		t.Fatal("an unknown state was taken")
	}
}

// The reactions for the dream: the tally per person and kind, then each with its reason.
func TestReactions(t *testing.T) {
	now := time.Date(2026, 10, 24, 10, 0, 0, 0, time.Local)
	j := testJournal(&now)
	if j.Reactions(func(string) string { return "" }) != "" {
		t.Fatal("reactions without any")
	}
	a, _ := j.Add(Candidate{Key: "a", Kind: "delivery", Person: "p", Text: "parcel arrives"}, "dry")
	b, _ := j.Add(Candidate{Key: "b", Kind: "delivery", Person: "p", Text: "parcel late"}, "dry")
	j.Add(Candidate{Key: "c", Kind: "delivery", Person: "p", Text: "not answered yet"}, "dry")
	j.Set(a.ID, "declined", "das interessiert mich nicht")
	j.Set(b.ID, "accepted", "")
	got := j.Reactions(func(id string) string { return map[string]string{"p": "Max"}[id] })
	for _, want := range []string{"Max, delivery: accepted 1, declined 1, ignored 0", "declined: parcel arrives (das interessiert mich nicht)",
		"accepted: parcel late"} {
		if !strings.Contains(got, want) {
			t.Errorf("missing %q in:\n%s", want, got)
		}
	}
	if strings.Contains(got, "not answered yet") {
		t.Error("an unanswered suggestion is in the reactions")
	}
}

// Suggestions unchanged for the keep days go, and their key may be found again.
func TestPrune(t *testing.T) {
	now := time.Date(2026, 10, 1, 10, 0, 0, 0, time.Local)
	j := testJournal(&now)
	j.Add(Candidate{Key: "old", Kind: "follow_up"}, "dry")
	now = now.AddDate(0, 0, 31)
	j.Add(Candidate{Key: "new", Kind: "follow_up"}, "dry")
	if j.byKey("old") != nil {
		t.Fatal("the old suggestion was kept")
	}
	if _, isNew := j.Add(Candidate{Key: "old", Kind: "follow_up"}, "dry"); !isNew {
		t.Fatal("a pruned key was not found again")
	}
}

// The verdict becomes the state; an unsure worthwhile one does not interrupt.
func TestApply(t *testing.T) {
	for _, c := range []struct {
		v           Verdict
		state, when string
	}{
		{Verdict{Worth: false, When: "now", Sure: 0.9}, "dropped", "now"},
		{Verdict{Worth: true, When: "never", Sure: 0.9}, "dropped", "never"},
		{Verdict{Worth: true, When: "now", Sure: 0.9}, "pending", "now"},
		{Verdict{Worth: true, When: "message", Sure: 0.4}, "pending", "next_conversation"},
		{Verdict{Worth: true, When: "briefing", Sure: 0.4}, "pending", "briefing"},
		{Verdict{Worth: false, When: "later", Sure: 0.3}, "later", "later"},
	} {
		if state, when := apply(c.v); state != c.state || when != c.when {
			t.Errorf("%+v: %s %s, want %s %s", c.v, state, when, c.state, c.when)
		}
	}
}

// The pass weighs what it recorded; a failed weighing leaves it dry for the next pass.
func TestPassWeighs(t *testing.T) {
	now := time.Date(2026, 10, 24, 10, 0, 0, 0, time.Local)
	p := &Pass{Journal: testJournal(&now), Now: func() time.Time { return now }}
	p.Sources = []Source{{Kind: "follow_up", Find: func() []Candidate {
		return []Candidate{{Key: "follow_up:a1", Kind: "follow_up", Person: "admin", Text: "waits"}}
	}}}
	fail := true
	var seen string
	p.Weigh = func(in string) (Verdict, error) {
		seen = in
		if fail {
			return Verdict{}, errors.New("no answer")
		}
		return Verdict{Worth: true, When: "next_conversation", Level: "suggest", Sure: 0.8}, nil
	}
	p.Situation = func(person string) string { return "# Situation\nrules of " + person }
	p.Once()
	if len(p.Journal.InState("dry")) != 1 {
		t.Fatal("a failed weighing did not leave the suggestion dry")
	}
	fail = false
	p.Once()
	got := p.Journal.InState("pending")
	if len(got) != 1 || got[0].When != "next_conversation" || got[0].Level != "suggest" {
		t.Fatalf("pending: %+v", got)
	}
	if !strings.Contains(seen, "kind: follow_up") || !strings.Contains(seen, "rules of admin") ||
		!strings.Contains(seen, "accepted 0, declined 0, ignored 0") {
		t.Fatalf("input: %s", seen)
	}
}

// Too early: weighed again a day later, with the latest text.
func TestLaterIsReconsidered(t *testing.T) {
	now := time.Date(2026, 10, 24, 10, 0, 0, 0, time.Local)
	p := &Pass{Journal: testJournal(&now), Now: func() time.Time { return now }}
	text := "unchanged for 2 days"
	p.Sources = []Source{{Kind: "follow_up", Find: func() []Candidate {
		return []Candidate{{Key: "follow_up:a1", Kind: "follow_up", Person: "admin", Text: text}}
	}}}
	var seen []string
	p.Weigh = func(in string) (Verdict, error) {
		seen = append(seen, in)
		if len(seen) == 1 {
			return Verdict{When: "later"}, nil
		}
		return Verdict{Worth: true, When: "next_conversation", Level: "suggest", Sure: 0.8}, nil
	}
	p.Once()
	if len(p.Journal.InState("later")) != 1 {
		t.Fatal("not kept for later")
	}
	now = now.Add(2 * time.Hour)
	p.Once()
	if len(seen) != 1 {
		t.Fatal("weighed again too soon")
	}
	now, text = now.Add(23*time.Hour), "unchanged for 3 days"
	p.Once()
	if len(seen) != 2 || !strings.Contains(seen[1], "3 days") || len(p.Journal.InState("pending")) != 1 {
		t.Fatalf("reconsidered: %d weighings, last %q", len(seen), seen[len(seen)-1])
	}
}

// The pass records new candidates only, and none that is already over.
func TestPassOnce(t *testing.T) {
	now := time.Date(2026, 10, 24, 10, 0, 0, 0, time.Local)
	p := &Pass{Journal: testJournal(&now), Now: func() time.Time { return now }}
	start := "@10:00"
	p.Sources = []Source{{Kind: "leave_time", Find: func() []Candidate {
		return []Candidate{
			{Key: "leave_time:e1" + start, Kind: "leave_time", Until: now.Add(time.Hour)},
			{Key: "leave_time:e0", Kind: "leave_time", Until: now.Add(-time.Minute)},
			{Key: "", Kind: "leave_time"},
		}
	}}}
	if made := p.Once(); len(made) != 1 || made[0].Key != "leave_time:e1@10:00" {
		t.Fatalf("first pass: %+v", made)
	}
	if made := p.Once(); len(made) != 0 {
		t.Fatalf("second pass recorded again: %+v", made)
	}
	// The appointment moved: the old suggestion is dropped, the new time is a new one.
	start = "@09:00"
	if made := p.Once(); len(made) != 1 || made[0].Key != "leave_time:e1@09:00" {
		t.Fatalf("moved: %+v", made)
	}
	if got := p.Journal.InState("dropped"); len(got) != 1 || got[0].Key != "leave_time:e1@10:00" {
		t.Fatalf("the old time was not dropped: %+v", got)
	}
}
