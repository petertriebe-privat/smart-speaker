package main

import (
	"crypto/sha256"
	"fmt"
	"log"
	"strings"
	"sync"
	"time"

	"echo-assistant/internal/agenda"
	"echo-assistant/internal/agent"
	"echo-assistant/internal/google"
	"echo-assistant/internal/initiative"
	"echo-assistant/internal/prompts"
)

// The initiative (#82, internal/initiative): The assistant helps unasked. The pass collects the
// candidates from the affairs and the agenda, records them and weighs them with the Decisions
// API (step 2); nothing is told or done yet, so the verdicts can be checked in real use first.

// roomSeen: the room of a person's last conversation at an Echo, and when it ended.
type roomSeen struct {
	room string
	at   time.Time
}

// whereabouts: where the user probably is, for the initiative's delivery: at home or not (Home
// Assistant's person, the phone app), the room of their last conversation at an Echo when it is
// recent, and the speakers whose presence sensor sees someone. The Echos have no presence sensor
// of their own.
func (d *device) whereabouts(person string) string {
	text := "# Where the user is\n"
	where := ""
	if d.svc.presence != nil {
		where = d.svc.presence.Of(person)
	}
	switch where {
	case "home":
		text += "At home (Home Assistant).\n"
	case "away":
		text += "Not at home (Home Assistant); locate_person gives the phone's address.\n"
	default:
		text += "Unknown whether at home.\n"
	}
	d.lastRoomMu.Lock()
	seen := d.lastRoom[person]
	d.lastRoomMu.Unlock()
	if seen.room != "" && time.Since(seen.at) < time.Duration(prompts.Int("INITIATIVE_ROOM_FRESH_MIN", 30))*time.Minute {
		text += fmt.Sprintf("Last talked with you at the speaker in the room %q %d min ago.\n", seen.room,
			int(time.Since(seen.at).Minutes()))
	} else {
		text += "No recent conversation at a speaker: the room is unknown.\n"
	}
	// a speaker with a presence sensor (the M5) says whether someone is in its room now: the only
	// room the assistant may speak in unasked (27.10.2026)
	if speakers := d.speakersText(); strings.Contains(speakers, "someone in the room now") {
		text += "assistant speakers: " + speakers + ".\n"
	} else {
		text += "No speaker sees anyone in its room now.\n"
	}
	return text
}

// houseCandidates: what in the house may need a word, as facts for the weighing: windows or doors
// open while the weather is wet or the house is empty, lights on while nobody is home. The key
// holds the facts, so a change is a new matter and the old one is dropped. Nothing is named in
// code: device classes and Home Assistant's own names.
func (d *device) houseCandidates() []initiative.Candidate {
	house, err := d.svc.ha.HouseState()
	if err != nil {
		log.Printf("[Initiative] the house: %v", err)
		return nil
	}
	home, known := 0, 0
	if d.svc.presence != nil {
		home, known = d.svc.presence.Home()
	}
	empty := known > 0 && home == 0
	wet := map[string]bool{"rainy": true, "pouring": true, "lightning-rainy": true, "snowy": true, "snowy-rainy": true,
		"hail": true}[house.Weather]
	admin := d.svc.people.Admin()
	var out []initiative.Candidate
	if len(house.Open) > 0 && (wet || empty) {
		why := "the weather is " + house.Weather
		if empty {
			why = "nobody of the household is at home"
			if wet {
				why += " and the weather is " + house.Weather
			}
		}
		out = append(out, initiative.Candidate{Kind: "home", Person: admin,
			Key:  "home:open:" + strings.Join(house.Open, ",") + "|" + house.Weather + fmt.Sprintf("|empty %v", empty),
			Text: fmt.Sprintf("open at home: %s, while %s", strings.Join(house.Open, ", "), why)})
	}
	if empty && len(house.Lights) > 0 {
		out = append(out, initiative.Candidate{Kind: "home", Person: admin,
			Key:  "home:lights:" + strings.Join(house.Lights, ","),
			Text: "nobody of the household is at home, but these lights are on: " + strings.Join(house.Lights, ", ")})
	}
	return out
}

// describeAppointment: `"<title>" <day> <start>-<end> in <place>` for the initiative's candidates.
func describeAppointment(e agenda.Appointment) string {
	text := fmt.Sprintf("%q %s %s-%s", e.Title, e.Start.Format("Mon 2006-01-02"), e.Start.Format("15:04"), e.End.Format("15:04"))
	if e.Place != "" {
		text += " in " + e.Place
	}
	return text
}

// situation: the person's rules, the time and whether hints are paused, for the weighing.
func (d *device) situation(person string) string {
	s := &d.svc
	text := "# Situation\nnow: " + time.Now().Format("Monday 2006-01-02 15:04") + "\n"
	if until := s.affairs.Quiet(person); until != "" {
		text += "The user paused unasked hints until the end of " + until + ".\n"
	}
	return text + "\n" + s.memory.Rules(person, false)
}

// initiativeCommand: ctl initiative (a pass now) or ctl initiative weigh <kind>: <text> (a
// verdict on a made-up candidate, nothing recorded).
func (d *device) initiativeCommand(arg string) string {
	if d.initiative == nil {
		return "error: no initiative on this device"
	}
	if arg == "house" && d.svc.ha != nil { // what the house source sees, as counts
		house, err := d.svc.ha.HouseState()
		if err != nil {
			return "error: " + err.Error()
		}
		home, known := 0, 0
		if d.svc.presence != nil {
			home, known = d.svc.presence.Home()
		}
		return fmt.Sprintf("open %d, lights on %d, weather %q, at home %d of %d; candidates %d", len(house.Open),
			len(house.Lights), house.Weather, home, known, len(d.houseCandidates()))
	}
	if rest, ok := strings.CutPrefix(arg, "weigh "); ok {
		kind, text, _ := strings.Cut(rest, ":")
		admin := d.svc.people.Admin()
		s := initiative.Suggestion{Kind: strings.TrimSpace(kind), Text: strings.TrimSpace(text), Person: admin}
		v, err := initiative.Decide(initiative.Input(s, d.situation(admin), d.initiative.Journal.Tally(admin, s.Kind)))
		if err != nil {
			return "error: " + err.Error()
		}
		return fmt.Sprintf("worth=%v sure=%.2f when=%s level=%s", v.Worth, v.Sure, v.When, v.Level)
	}
	made := d.initiative.Once()
	return fmt.Sprintf("%d new; pending %d, dropped %d, dry %d", len(made), len(d.initiative.Journal.InState("pending")),
		len(d.initiative.Journal.InState("dropped")), len(d.initiative.Journal.InState("dry")))
}

func (d *device) setupInitiative() {
	s := &d.svc
	pass := &initiative.Pass{Journal: s.journal, Idle: func() bool { return !d.anyOpen() },
		Weigh: initiative.Decide, Situation: d.situation,
		Quiet: func(person string) bool { return s.affairs.Quiet(person) != "" }}
	pass.Deliver = func(batch []initiative.Suggestion) string {
		task := prompts.Text("INITIATIVE_DELIVER") + "\n\n" + d.whereabouts(batch[0].Person) + "\nSuggestions due now:\n"
		for _, sg := range batch {
			task += "- [" + sg.ID + "] (" + sg.Kind + "): " + sg.Text + "\n"
		}
		return s.agent.Run(task, agent.Options{Instructions: prompts.Text("TASK_INSTRUCTIONS"), Person: batch[0].Person,
			Origin: "eigene Initiative"})
	}
	d.initiative = pass
	admin := s.people.Admin
	pass.Sources = append(pass.Sources, initiative.Source{Kind: "follow_up", Find: func() []initiative.Candidate {
		var out []initiative.Candidate
		// every waiting matter: whether it is time to follow up is the weighing's, not a number of
		// days here (the owner's wish, 24.10.2026)
		for _, a := range s.affairs.Waiting(admin(), 0) {
			text := fmt.Sprintf("[%s] %s is waiting for a reply (%s), unchanged for %s", a.ID, a.Title, a.From, a.Since)
			if a.Due != "" {
				text += ", due " + a.Due
			}
			if box, ref, ok := strings.Cut(a.Mail, ":"); ok {
				text += fmt.Sprintf("; its mail: read_private_mail mailbox %q, ref %q. ", box, ref) + prompts.Text("INITIATIVE_FOLLOW_UP")
			}
			out = append(out, initiative.Candidate{Key: "follow_up:" + a.ID, Kind: "follow_up", Person: admin(), Text: text})
		}
		return out
	}})
	if s.mailwatch != nil { // parcels (step 9): expected today or tomorrow, late, or just delivered
		pass.Sources = append(pass.Sources, initiative.Source{Kind: "delivery", Find: func() []initiative.Candidate {
			var out []initiative.Candidate
			for _, p := range s.mailwatch.Parcels(admin()) {
				state := "on its way, expected " + p.Expected
				if p.Delivered {
					state = "delivered"
				} else if p.Expected == "" {
					state = "on its way, no date known"
				}
				out = append(out, initiative.Candidate{Key: "delivery:" + p.Key, Kind: "delivery", Person: admin(),
					Text: fmt.Sprintf("parcel %s: %s (today %s)", p.Line, state, time.Now().Format("2006-01-02"))})
			}
			return out
		}})
	}
	if s.google.Configured() { // birthdays of the contacts (step 9): read once a day, they are many
		var mu sync.Mutex
		var day string
		var cached []google.Birthday
		pass.Sources = append(pass.Sources, initiative.Source{Kind: "birthday", Find: func() []initiative.Candidate {
			mu.Lock()
			defer mu.Unlock()
			if today := time.Now().Format("2006-01-02"); day != today {
				list, err := s.google.Birthdays(prompts.Int("INITIATIVE_BIRTHDAY_DAYS", 7))
				if err != nil { // keep the last ones: an error must not drop them all
					log.Printf("[Initiative] birthdays: %v", err)
				} else {
					day, cached = today, list
				}
			}
			var out []initiative.Candidate
			for _, b := range cached {
				text := fmt.Sprintf("birthday of %s on %s (in %d days)", b.Name, b.Next.Format("Monday 2006-01-02"), b.Away)
				if b.Age != 0 {
					text += fmt.Sprintf(", turns %d", b.Age)
				}
				out = append(out, initiative.Candidate{Key: "birthday:" + b.Name + "@" + b.Next.Format("2006-01-02"),
					Kind: "birthday", Person: admin(), Until: b.Next.Add(24 * time.Hour), Text: text})
			}
			return out
		}})
	}
	// the dream's offers from recurring wishes (a task, an automation, a skill) go through the
	// initiative, so they are weighed and the answer counts (step 9)
	s.memory.OnSuggestion = func(person, text string) bool {
		if person == "" {
			person = admin()
		}
		sum := sha256.Sum256([]byte(person + "|" + text))
		_, isNew := s.journal.Add(initiative.Candidate{Key: fmt.Sprintf("routine:%x", sum[:8]), Kind: "routine",
			Person: person, Text: "from recurring wishes, an offer: " + text}, "dry")
		if isNew {
			pass.Kick()
		}
		return true
	}
	if s.ha != nil && s.ha.Configured() { // the house (step 9): open windows and doors, lights, weather
		pass.Sources = append(pass.Sources, initiative.Source{Kind: "home", Find: d.houseCandidates})
	}
	if s.agenda != nil {
		s.agenda.OnChange = pass.Kick
		pass.Sources = append(pass.Sources, initiative.Source{Kind: "leave_time", Find: func() []initiative.Candidate {
			// how far ahead appointments are looked at, so a long way is planned in time; when to
			// remind is the run's decision
			ahead := time.Duration(prompts.Int("INITIATIVE_LEAVE_AHEAD_MIN", 720)) * time.Minute
			var out []initiative.Candidate
			for _, e := range s.agenda.WithPlace(ahead) {
				// start, place and the neighbours in the key: a moved appointment, or one added before
				// or after, makes it a new matter and the old one is dropped
				text := "appointment " + describeAppointment(e) + ": when to leave"
				key := "leave_time:" + e.Key + "@" + e.Start.Format("2006-01-02 15:04") + "|" + e.Place
				if b := e.Before; b != nil {
					text += "; before it the same day: " + describeAppointment(*b)
					if b.End.After(e.Start) {
						text += fmt.Sprintf(" (overlaps by %d min)", int(b.End.Sub(e.Start).Minutes()))
					}
					key += "|after " + b.Key + "@" + b.End.Format("15:04")
				}
				if n := e.After; n != nil {
					text += "; after it the same day: " + describeAppointment(*n)
					key += "|before " + n.Key + "@" + n.Start.Format("15:04")
				}
				out = append(out, initiative.Candidate{Key: key, Kind: "leave_time", Person: admin(), Until: e.Start, Text: text})
			}
			return out
		}})
		pass.Sources = append(pass.Sources, initiative.Source{Kind: "overlap", Find: func() []initiative.Candidate {
			var out []initiative.Candidate
			for _, p := range s.agenda.Overlaps() {
				out = append(out, initiative.Candidate{Kind: "overlap", Person: admin(), Until: p[1].Start,
					Key: "overlap:" + p[0].Key + "@" + p[0].Start.Format("2006-01-02 15:04") + "-" + p[0].End.Format("15:04") +
						"|" + p[1].Key + "@" + p[1].Start.Format("2006-01-02 15:04"),
					Text: fmt.Sprintf("two appointments overlap by %d min: %s and %s",
						int(p[0].End.Sub(p[1].Start).Minutes()), describeAppointment(p[0]), describeAppointment(p[1]))})
			}
			return out
		}})
	}
	go pass.Run()
}
