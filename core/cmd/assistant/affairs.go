package main

import (
	"log"
	"strings"
	"sync"
	"time"

	"echo-assistant/internal/affairs"
	"echo-assistant/internal/mailwatch"
	"echo-assistant/internal/memory"
	"echo-assistant/internal/prompts"
)

// The affairs (#71, internal/affairs): the mail watch and the consolidation write them, the
// agenda reads them and links the calendar and the to-do lists to them, the dream reviews them
// once a night. What the assistant infers itself (a deadline from a mail, a promise from a
// conversation) also goes into his own Google Tasks list, kept in step both ways.

// ownList: the name of the assistant's own to-do list in Google Tasks.
func ownList() string { return prompts.TextOr("AFFAIRS_TODO_LIST", "Von "+prompts.Name()) }

// ownKinds: the kinds that become a to-do in the assistant's own list.
var ownKinds = map[string]bool{"todo": true, "deadline": true, "promise": true}

func (d *device) setupAffairs(primary bool) {
	s := &d.svc
	book := s.affairs
	s.memory.Affairs = func() string { return book.Brief("") }
	s.memory.ApplyAffairs = d.applyAffairs
	s.memory.AffairsEvidence = func() string { return s.mailwatch.Evidence(prompts.Int("DREAM_DAYS", 7)) }
	s.memory.ReviewAffairs = d.reviewAffairs
	s.mailwatch.Affairs = book.Brief
	s.mailwatch.Intake = func(person string, a mailwatch.MailAffair) string {
		before := book.State(a.Match)
		wasOpen := a.Match != "" && before != "" && before != "done" && before != "obsolete"
		got := book.Intake(person, affairs.Candidate{Match: a.Match, Kind: a.Kind, Title: a.Title, Due: a.Due, State: a.State,
			Parent: a.Parent, Source: affairs.Source{Kind: "mail", Ref: a.Box + ":" + a.Ref, About: a.From},
			Reason: "Mail: " + a.Subject}, "mail")
		if got == nil || !wasOpen || got.ID != a.Match {
			return ""
		}
		return book.SettledNote(*got) // the mail settled it: the person hears so with the mail
	}
	// The calendar ahead and every open to-do as affairs before a mail is sorted: the return's
	// to-do (no date) and its appointment on Monday were no affairs yet, so its "on the way back"
	// mail on Saturday had nothing to settle (31.10.2026). At most every AFFAIRS_SYNC_MS.
	var prepared time.Time
	var prepareMu sync.Mutex
	s.mailwatch.Prepare = func(person string) {
		if person != s.people.Admin() || !s.google.Configured() {
			return
		}
		prepareMu.Lock()
		defer prepareMu.Unlock()
		if time.Since(prepared) < time.Duration(prompts.Int("AFFAIRS_SYNC_MS", 1200000))*time.Millisecond {
			return
		}
		prepared = time.Now()
		now := time.Now()
		start := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, now.Location())
		if events, err := s.google.Events(start, start.AddDate(0, 0, prompts.Int("AFFAIRS_AHEAD_DAYS", 14))); err == nil {
			for _, e := range events {
				if e.Info {
					continue
				}
				due := e.Start.Format("2006-01-02 15:04")
				if e.AllDay {
					due = e.Start.Format("2006-01-02")
				}
				book.Observe(person, affairs.Source{Kind: "calendar", Ref: e.ID}, "appointment", e.Title, due)
			}
		} else {
			log.Printf("[Affairs] calendar ahead: %v", err)
		}
		if todos, err := s.google.AllOpenTodos(ownList()); err == nil {
			for _, t := range todos {
				book.Observe(person, affairs.Source{Kind: "task", Ref: t.ID}, "todo", t.Title, t.Due)
			}
		} else {
			log.Printf("[Affairs] open to-dos: %v", err)
		}
	}
	if primary && !book.Existed() {
		d.takeOverDeadlines()
	}
	if s.agenda != nil {
		admin := s.people.Admin
		s.agenda.Affairs.Relevant = func(reader string) []affairs.Item {
			return book.Relevant(reader, prompts.Int("AFFAIRS_CONTEXT_MAX", 5))
		}
		s.agenda.Affairs.Observe = func(src affairs.Source, kind, title, due string) string {
			id, _ := book.Observe(admin(), src, kind, title, due)
			return id
		}
		s.agenda.Affairs.State = book.State
		s.agenda.Affairs.Quiet = book.Quiet
	}
	if !primary || !s.google.Configured() {
		return
	}
	// The assistant's own to-do list, in its own goroutine: Google takes seconds.
	book.OnNew = func(a affairs.Affair) {
		if a.Person != s.people.Admin() || !ownKinds[a.Kind] {
			return
		}
		switch src := a.Sources; {
		case len(src) == 0, src[0].Kind != "mail" && src[0].Kind != "conversation":
			return // the calendar and the to-do lists have it already
		}
		go func() {
			id, err := s.google.AddOwnTodo(ownList(), a.Title, a.Due, prompts.Name()+" ["+a.ID+"], "+a.History[0].Reason)
			if err != nil {
				log.Printf("[Affairs] own to-do for %s: %v", a.ID, err)
				return
			}
			book.AddSource(a.ID, affairs.Source{Kind: "task", Ref: id, About: affairs.Own})
			log.Printf("[Affairs] %s in \"%s\"", a.ID, ownList())
		}()
	}
	book.OnSettled = func(a affairs.Affair) {
		act := book.Trust(a.Person) == "act"
		for _, src := range a.Sources {
			if src.Kind != "task" || (src.About != affairs.Own && !act) {
				continue
			}
			go func(ref string) {
				if err := s.google.CompleteTodo(ref); err != nil {
					log.Printf("[Affairs] ticking off %s for %s: %v", ref, a.ID, err)
				}
			}(src.Ref)
		}
	}
	go d.syncOwnTodos()
}

// syncOwnTodos: what the user ticked off or deleted in the assistant's own list settles its affair.
// Every AFFAIRS_SYNC_MS between conversations.
func (d *device) syncOwnTodos() {
	s := &d.svc
	every := time.Duration(prompts.Int("AFFAIRS_SYNC_MS", 1200000)) * time.Millisecond
	for range time.Tick(every) {
		own := s.affairs.OwnTasks()
		if len(own) == 0 || d.anyOpen() {
			continue
		}
		open, err := s.google.OpenTodos(ownList())
		if err != nil || open == nil || len(open) >= 100 { // the first page only: not sure what is gone
			if err != nil {
				log.Printf("[Affairs] own to-dos: %v", err)
			}
			continue
		}
		for ref, id := range own {
			if !open[ref] {
				s.affairs.Update("", id, affairs.Changes{State: "done", Reason: "in Google Tasks abgehakt"}, "task")
			}
		}
	}
}

// applyAffairs: the affairs the consolidation found in a conversation. A change names its person
// when it is someone else's of the household, else it is the partner's (owner).
func (d *device) applyAffairs(owner string, changes []memory.AffairChange, at time.Time) {
	src := affairs.Source{Kind: "conversation", Ref: at.Format("2006-01-02 15:04")}
	for _, ch := range changes {
		person := owner
		if ch.Person != "" && d.svc.people.Name(ch.Person) != "" {
			person = ch.Person
		}
		d.svc.affairs.Intake(person, affairs.Candidate{Match: ch.ID, Kind: ch.Kind, Title: ch.Title, Due: ch.Due,
			State: ch.State, Parent: ch.Parent, Source: src, Reason: ch.Reason}, "conversation")
	}
}

// reviewAffairs: the nightly review's changes; the report lines for the dream diary.
func (d *device) reviewAffairs(changes []memory.AffairReview) string {
	var report strings.Builder
	for _, ch := range changes {
		state := ch.State
		if state != "" && state != "done" && state != "obsolete" && state != "snoozed" {
			state = "" // the review never reopens
		}
		out, err := d.svc.affairs.Update("", ch.ID, affairs.Changes{State: state, Due: ch.Due, Remind: ch.RemindAt,
			Parent: ch.Parent, Reason: ch.Reason}, "dream")
		if err != nil {
			report.WriteString("- " + ch.ID + ": nicht geändert (" + err.Error() + ")\n")
			continue
		}
		first, _, _ := strings.Cut(out, "\n")
		report.WriteString("- " + first + " (" + ch.Reason + ")\n")
	}
	return report.String()
}

// takeOverDeadlines: the first start with affairs takes the deadlines still ahead from the
// sorted mails, so the agenda does not lose them.
func (d *device) takeOverDeadlines() {
	s := &d.svc
	from := time.Now().Format("2006-01-02")
	to := time.Now().AddDate(0, 0, 60).Format("2006-01-02")
	n := 0
	for _, p := range s.people.All() {
		for _, dl := range s.mailwatch.Deadlines(p.ID, from, to) {
			box, ref, _ := strings.Cut(strings.TrimPrefix(dl.Key, "mail:"), ":")
			kind := "deadline"
			if strings.HasPrefix(dl.Line, "appointment,") {
				kind = "appointment"
			}
			if s.affairs.Intake(p.ID, affairs.Candidate{Kind: kind, Title: dl.Subject, Due: dl.Day,
				Source: affairs.Source{Kind: "mail", Ref: box + ":" + ref}, Reason: dl.Line}, "takeover") != nil {
				n++
			}
		}
	}
	log.Printf("[Affairs] %d deadlines taken over from the mail watch", n)
	s.affairs.Save()
}
