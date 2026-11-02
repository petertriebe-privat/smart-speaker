package initiative

import (
	"strings"
	"testing"
	"time"
)

// A leaving time is planned at once (the run picks the time), urgent ones too, the rest in a
// conversation.
func TestDeliverAt(t *testing.T) {
	now := time.Date(2026, 10, 24, 13, 0, 0, 0, time.Local)
	leave := Suggestion{Kind: "leave_time", Until: now.Add(10 * time.Hour).Unix()}
	if at := deliverAt(leave, "pending", "next_conversation", now); at != now.Unix() {
		t.Fatalf("leaving time at %v", time.Unix(at, 0))
	}
	follow := Suggestion{Kind: "follow_up"}
	if deliverAt(follow, "pending", "message", now) != now.Unix() || deliverAt(follow, "pending", "next_conversation", now) != 0 ||
		deliverAt(leave, "dropped", "never", now) != 0 {
		t.Fatal("follow-up or dropped delivered wrongly")
	}
}

// Briefing suggestions wait for the next morning; three conversations without an answer: ignored.
func TestConversationDelivery(t *testing.T) {
	now := time.Date(2026, 10, 24, 13, 0, 0, 0, time.Local)
	j := testJournal(&now)
	p := &Pass{Journal: j}
	a, _ := j.Add(Candidate{Key: "a", Kind: "follow_up", Person: "admin", Text: "offer waits"}, "dry")
	b, _ := j.Add(Candidate{Key: "b", Kind: "follow_up", Person: "admin", Text: "tomorrow's thing"}, "dry")
	j.Verdict(a.ID, "pending", "next_conversation", "suggest", "", 0)
	j.Verdict(b.ID, "pending", "briefing", "know", "", 0)
	text := p.Context("admin", "speaker")
	if !strings.Contains(text, "[s1] (suggest) offer waits") || strings.Contains(text, "tomorrow's thing") {
		t.Fatalf("context today: %s", text)
	}
	if p.Context("admin", "") != "" || p.Context("someone", "speaker") != "" {
		t.Fatal("suggestions outside a voice conversation or for someone else")
	}
	now = now.AddDate(0, 0, 1).Add(-6 * time.Hour) // 07:00 the next day
	if text := p.Context("admin", "speaker"); !strings.Contains(text, "tomorrow's thing") {
		t.Fatalf("briefing the next morning: %s", text)
	}
	j.Set(b.ID, "accepted", "")
	for i := 0; i < 3; i++ {
		p.Context("admin", "speaker")
		p.ConversationOver("speaker")
	}
	if got := j.InState("ignored"); len(got) != 1 || got[0].ID != a.ID {
		t.Fatalf("ignored: %+v", got)
	}
	if j.Tally("admin", "follow_up")["accepted"] != 1 {
		t.Fatal("the accepted one is not in the tally")
	}
}

// A due suggestion is told once; one whose matter is over is dropped untold.
func TestDeliverDue(t *testing.T) {
	now := time.Date(2026, 10, 24, 13, 0, 0, 0, time.Local)
	j := testJournal(&now)
	var told []string
	runs := 0
	p := &Pass{Journal: j, Deliver: func(batch []Suggestion) string {
		runs++
		for _, s := range batch {
			told = append(told, s.ID)
		}
		return "Fahr um 15 los."
	}}
	a, _ := j.Add(Candidate{Key: "a", Kind: "leave_time", Until: now.Add(time.Hour)}, "dry")
	b, _ := j.Add(Candidate{Key: "b", Kind: "leave_time", Until: now.Add(time.Minute)}, "dry")
	j.Verdict(a.ID, "pending", "next_conversation", "know", "", now.Unix())
	j.Verdict(b.ID, "pending", "next_conversation", "know", "", now.Unix())
	now = now.Add(2 * time.Minute)
	p.deliverDue()
	p.deliverDue()
	if len(told) != 1 || told[0] != a.ID || len(j.InState("told")) != 1 || len(j.InState("dropped")) != 1 {
		t.Fatalf("told %v, states told %d dropped %d", told, len(j.InState("told")), len(j.InState("dropped")))
	}
	// Hints paused: only an urgent one is told.
	p.Quiet = func(string) bool { return true }
	c, _ := j.Add(Candidate{Key: "c", Kind: "follow_up"}, "dry")
	u, _ := j.Add(Candidate{Key: "u", Kind: "follow_up"}, "dry")
	j.Verdict(c.ID, "pending", "message", "know", "", now.Unix())
	j.Verdict(u.ID, "pending", "now", "know", "", now.Unix())
	told = nil
	p.deliverDue()
	if len(told) != 1 || told[0] != u.ID {
		t.Fatalf("paused: told %v", told)
	}
	// Several due together for one person: one run.
	p.Quiet = nil
	x, _ := j.Add(Candidate{Key: "x", Kind: "leave_time", Person: "admin"}, "dry")
	y, _ := j.Add(Candidate{Key: "y", Kind: "overlap", Person: "admin"}, "dry")
	j.Verdict(x.ID, "pending", "now", "know", "", now.Unix())
	j.Verdict(y.ID, "pending", "now", "know", "", now.Unix())
	told, runs = nil, 0
	p.deliverDue()
	if runs != 1 || len(told) != 2 {
		t.Fatalf("batch: %d runs, told %v", runs, told)
	}
}
