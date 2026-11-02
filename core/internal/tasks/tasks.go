// Package tasks: scheduled tasks and reminders, as devices/atoms3r/src/assistant/tasks.cpp. A
// reminder is announced at its time; a prompt task is an instruction the agent carries out with
// all tools and delivers by mail, announcement or Telegram. Stored as tasks.json (the M5's
// format) and backed up; restored from the backup only on the primary device (the M5 runs its
// own copy of the same tasks while it is primary).
package tasks

import (
	"fmt"
	"log"
	"strconv"
	"strings"
	"sync"
	"time"

	"echo-assistant/internal/backup"
	"echo-assistant/internal/netx"
	"echo-assistant/internal/people"
	"echo-assistant/internal/prompts"
	"echo-assistant/internal/store"
	"echo-assistant/internal/tools"
)

const (
	filePath   = "tasks.json"
	backupFile = "tasks.json"
	noClock    = "Error: the clock is not set yet"
)

var weekdays = []string{"", "Mo", "Di", "Mi", "Do", "Fr", "Sa", "So"}

type Schedule struct {
	Type       string `json:"type"`
	Time       string `json:"time,omitempty"`
	Date       string `json:"date,omitempty"`
	Days       []int  `json:"days,omitempty"`
	DayOfMonth int    `json:"day_of_month,omitempty"`
	EveryHours int    `json:"every_hours,omitempty"`
}

type Task struct {
	ID       int      `json:"id"`
	Title    string   `json:"title"`
	Kind     string   `json:"kind"` // "reminder" or "prompt"
	Text     string   `json:"text"`
	Deliver  string   `json:"deliver"`        // email, speak, both, telegram, none, call
	Room     string   `json:"room,omitempty"` // speak, both: the assistant speaker to say it on, '' = this one
	Schedule Schedule `json:"schedule"`
	Next     int64    `json:"next"`
	Last     int64    `json:"last,omitempty"`
	Paused   bool     `json:"paused"`
	Created  string   `json:"created"`
	Person   string   `json:"person,omitempty"` // who created it: the run is theirs
}

type doc struct {
	NextID int    `json:"next_id"`
	Tasks  []Task `json:"tasks"`
}

// Runner: what the scheduler needs from the device.
type Runner struct {
	Announce func(room, text string) string                 // agent announcer, on the speaker in room
	RunAgent func(task, instructions, person string) string // agent run with TASK_INSTRUCTIONS
	Call     func(person, task string) string               // phone_call to the person; nil = none
	// Notice: an urgent note into the running conversation; false if none is live.
	Notice    func(text string) bool
	Open      func() bool // a conversation is open
	Idle      func() bool // no conversation: background work may run
	OwnerMail string
}

type Scheduler struct {
	People *people.Register
	Run_   Runner

	mu             sync.Mutex
	d              doc
	backupPending  bool
	restorePending bool
	jobs           chan Task
	retryAt        time.Time // restore or backup failed: not before then
}

func New(p *people.Register, r Runner) *Scheduler {
	s := &Scheduler{People: p, Run_: r, jobs: make(chan Task, 4)}
	loaded := store.LoadJSON(filePath, &s.d)
	s.normalize()
	s.restorePending = !loaded && backup.Primary
	log.Printf("[Tasks] %d scheduled%s", len(s.d.Tasks), map[bool]string{true: ", looking for the backup"}[s.restorePending])
	return s
}

func (s *Scheduler) normalize() {
	if s.d.NextID < 1 {
		s.d.NextID = 1
	}
	if s.d.Tasks == nil {
		s.d.Tasks = []Task{}
	}
}

// --- Schedules -----------------------------------------------------------------------------

func parseTime(text string) (int, int, bool) {
	h, m, ok := strings.Cut(text, ":")
	if !ok {
		return 0, 0, false
	}
	hour, err1 := strconv.Atoi(strings.TrimSpace(h))
	minute, err2 := strconv.Atoi(strings.TrimSpace(m))
	return hour, minute, err1 == nil && err2 == nil && hour >= 0 && hour < 24 && minute >= 0 && minute < 60
}

// at: local time on base's date plus days, at hh:mm (time.Date normalizes overflow and DST).
func at(base time.Time, days, hour, minute int) time.Time {
	return time.Date(base.Year(), base.Month(), base.Day()+days, hour, minute, 0, 0, time.Local)
}

// NextRun: first run of schedule after after; zero if there is none (a once-task in the past) or
// the schedule is incomplete (err says why).
func NextRun(sc Schedule, after time.Time) (time.Time, string) {
	if sc.Type == "interval" {
		if sc.EveryHours < 1 || sc.EveryHours > 24*31 {
			return time.Time{}, "every_hours must be between 1 and 744"
		}
		return after.Add(time.Duration(sc.EveryHours) * time.Hour), ""
	}
	hour, minute, ok := parseTime(sc.Time)
	if !ok {
		return time.Time{}, "time missing (HH:MM)"
	}
	switch sc.Type {
	case "once":
		if sc.Date == "" { // today, or tomorrow if the time has passed
			if t := at(after, 0, hour, minute); t.After(after) {
				return t, ""
			}
			return at(after, 1, hour, minute), ""
		}
		d, err := time.ParseInLocation("2006-01-02", sc.Date, time.Local)
		if err != nil {
			return time.Time{}, "date must be YYYY-MM-DD"
		}
		if t := at(d, 0, hour, minute); t.After(after) {
			return t, ""
		}
		return time.Time{}, ""
	case "daily":
		for d := 0; d <= 1; d++ {
			if t := at(after, d, hour, minute); t.After(after) {
				return t, ""
			}
		}
	case "weekly":
		wanted := map[int]bool{}
		for _, d := range sc.Days {
			if d >= 1 && d <= 7 {
				wanted[d] = true
			}
		}
		for d := 0; d <= 7; d++ {
			t := at(after, d, hour, minute)
			weekday := int(t.Weekday())
			if weekday == 0 {
				weekday = 7 // 1 = Monday
			}
			if t.After(after) && wanted[weekday] {
				return t, ""
			}
		}
		return time.Time{}, "days missing (1 = Monday ... 7 = Sunday)"
	case "monthly":
		if sc.DayOfMonth < 1 || sc.DayOfMonth > 31 {
			return time.Time{}, "day_of_month must be 1 to 31"
		}
		for m := 0; m <= 12; m++ {
			first := time.Date(after.Year(), after.Month()+time.Month(m), 1, hour, minute, 0, 0, time.Local)
			last := time.Date(first.Year(), first.Month()+1, 0, 0, 0, 0, 0, time.Local).Day()
			if t := at(first, min(sc.DayOfMonth, last)-1, hour, minute); t.After(after) {
				return t, ""
			}
		}
	}
	return time.Time{}, "unknown schedule (once, daily, weekly, monthly, interval)"
}

func describeSchedule(sc Schedule) string {
	switch sc.Type {
	case "interval":
		return fmt.Sprintf("alle %d Stunden", sc.EveryHours)
	case "daily":
		return "täglich " + sc.Time
	case "weekly":
		var days []string
		for _, d := range sc.Days {
			if d >= 1 && d <= 7 {
				days = append(days, weekdays[d])
			}
		}
		return strings.Join(days, ", ") + " " + sc.Time
	case "monthly":
		return fmt.Sprintf("monatlich am %d. um %s", sc.DayOfMonth, sc.Time)
	}
	return "einmalig"
}

func (s *Scheduler) describe(t Task) string {
	deliveries := map[string]string{"email": "per Mail", "speak": "als Ansage", "both": "per Mail und Ansage",
		"telegram": "per Telegram", "none": "ohne Meldung"}
	kind := deliveries[t.Deliver]
	if t.Kind == "reminder" {
		kind = map[string]string{"speak": "Erinnerung als Ansage", "both": "Erinnerung als Ansage und Nachricht",
			"call": "Erinnerung per Anruf"}[t.Deliver]
		if kind == "" {
			kind = "Erinnerung per Nachricht"
		}
	}
	if t.Room != "" && (t.Deliver == "speak" || t.Deliver == "both") {
		kind += " in " + t.Room
	}
	text := fmt.Sprintf("#%d %s (%s, %s", t.ID, t.Title, describeSchedule(t.Schedule), kind)
	if name := s.People.Name(t.Person); name != "" {
		text += ", für " + name
	}
	text += ")"
	if t.Paused {
		text += ", pausiert"
	} else if t.Next != 0 {
		next := time.Unix(t.Next, 0)
		weekday := int(next.Weekday())
		if weekday == 0 {
			weekday = 7
		}
		layout := "02.01.2006 15:04"
		if next.Year() == time.Now().Year() {
			layout = "02.01. 15:04"
		}
		text += ", nächster Lauf " + weekdays[weekday] + " " + next.Format(layout)
	}
	return text
}

// --- Storage (caller holds the lock) -------------------------------------------------------

func (s *Scheduler) saveLocked() {
	if err := store.SaveJSON(filePath, s.d); err != nil {
		log.Printf("[Tasks] saving failed: %v", err)
	}
	s.backupPending = backup.Available() && !s.restorePending
}

func (s *Scheduler) findLocked(id int) *Task {
	for i := range s.d.Tasks {
		if s.d.Tasks[i].ID == id {
			return &s.d.Tasks[i]
		}
	}
	return nil
}

// --- Running -------------------------------------------------------------------------------

func (s *Scheduler) runJob(job Task) {
	r := s.Run_
	// The agent next to a conversation: wait until it is over (at most half an hour).
	for waited := 0; job.Kind != "reminder" && !r.Idle() && waited < 1800; waited++ {
		time.Sleep(time.Second)
	}
	started := time.Now()
	if job.Kind == "reminder" {
		// Planned reminders say so (and for whom, when not the admin's); announcements from Home
		// Assistant (id 0) speak for themselves.
		text := job.Text
		if job.ID != 0 {
			if name := s.People.Name(job.Person); job.Person != s.People.Admin() && name != "" {
				text = "Erinnerung für " + name + ": " + text
			} else {
				text = "Erinnerung: " + text
			}
		}
		// call: The assistant phones the person and tells it (very important ones); a failed call becomes
		// a message
		if job.Deliver == "call" && r.Call != nil {
			out := r.Call(job.Person, "Erinnerung, die jetzt fällig ist; sag sie dem Nutzer: "+job.Text)
			log.Printf("[Tasks] #%d reminder: call %.200s", job.ID, out)
			if !strings.HasPrefix(out, "Error") {
				return
			}
		}
		// telegram (the default): a message to the person (WhatsApp for whom has no Telegram).
		// Jarvis cannot know which room they are in: announced only on their own words ("sag es mir
		// in der Küche"), 27.10.2026. A failed message is announced here rather than lost.
		if job.Deliver != "speak" {
			via := s.People.Message(job.Person, text)
			log.Printf("[Tasks] #%d reminder: message %s", job.ID, via)
			if job.Deliver != "both" && !strings.HasPrefix(via, "Error") {
				return
			}
		}
		log.Printf("[Tasks] #%d reminder: %s", job.ID, r.Announce(job.Room, text))
		return
	}
	email := r.OwnerMail
	if owner, ok := s.People.Find(job.Person); ok && owner.Email != "" {
		email = owner.Email
	}
	delivery := ""
	if job.Deliver == "email" || job.Deliver == "both" {
		delivery += "Send the result with send_email to " + email + ", subject: " + job.Title + ". "
	}
	if job.Deliver == "speak" || job.Deliver == "both" {
		room := "''"
		if job.Room != "" {
			room = job.Room
		}
		delivery += "Announce the result with speak, room " + room + " (short, spoken German). "
	}
	if job.Deliver == "telegram" { // the person's chat: WhatsApp for whom has no Telegram (#34)
		if s.People.ChatChannel(job.Person) == "whatsapp" {
			delivery += "Send the result with send_whatsapp to the user (short, *bold* title, bare links). "
		} else {
			delivery += "Send the result with send_telegram to the user's Telegram chat (short, bold title, links as " +
				"<a href>). "
		}
	}
	if job.Deliver == "call" {
		to := "''"
		if job.Person != "" {
			to = s.People.Name(job.Person)
		}
		delivery += "Tell the result by phone: phone_call with to = " + to + " and the result as task. "
	}
	if job.Deliver == "none" {
		delivery = "Report nothing, only carry out the instruction. "
	}
	task := "Scheduled task \"" + job.Title + "\" is due.\nInstruction: " + job.Text + "\nDelivery: " + delivery
	answer := r.RunAgent(task, prompts.Text("TASK_INSTRUCTIONS"), job.Person)
	log.Printf("[Tasks] #%d %s done after %d s: %.200s", job.ID, job.Title, int(time.Since(started).Seconds()), answer)
}

func (s *Scheduler) queue(t Task) bool {
	select {
	case s.jobs <- t:
		return true
	default:
		return false
	}
}

func (s *Scheduler) reminderInConversation(text string) bool {
	return s.Run_.Notice != nil && s.Run_.Notice("A reminder or message that is due now: "+text)
}

// RunNow: an event to run once at once (Home Assistant's announcements: kind reminder, id 0).
func (s *Scheduler) RunNow(kind, title, text, deliver, person string) {
	if text == "" {
		return
	}
	if kind == "reminder" && s.reminderInConversation(text) {
		return
	}
	if deliver == "" {
		deliver = "speak"
	}
	if !s.queue(Task{Kind: kind, Title: title, Text: text, Deliver: deliver, Person: person}) {
		log.Print("[Tasks] queue full, event dropped")
	}
}

// Run: the scheduler (checks once a second) and the worker. Start as a goroutine.
func (s *Scheduler) Run() {
	go func() {
		for job := range s.jobs {
			s.runJob(job)
		}
	}()
	catchup := int64(prompts.Int("TASK_CATCHUP_S", 7200))
	for range time.Tick(time.Second) {
		s.background()
		if !netx.TimeSynced {
			continue
		}
		now := time.Now()
		s.mu.Lock()
		changed := false
		for i := 0; i < len(s.d.Tasks); {
			t := &s.d.Tasks[i]
			if t.Paused || t.Next == 0 || t.Next > now.Unix() {
				i++
				continue
			}
			reminder := t.Kind == "reminder"
			aloud := reminder && (t.Deliver == "speak" || t.Deliver == "both") // a message or call needs no quiet
			open := s.Run_.Open()
			switch {
			case now.Unix()-t.Next > catchup:
				log.Printf("[Tasks] #%d missed (%s), skipped", t.ID, time.Unix(t.Next, 0).Format("02.01. 15:04"))
			case aloud && open && s.reminderInConversation(t.Text):
				// in a conversation the assistant says it itself
			case aloud && open || !reminder && !s.Run_.Idle():
				i++ // after the conversation
				continue
			case !s.queue(*t):
				i++ // queue full: next second
				continue
			}
			t.Last = now.Unix()
			changed = true
			var next time.Time
			if t.Schedule.Type != "once" {
				next, _ = NextRun(t.Schedule, now)
			}
			if !next.IsZero() {
				t.Next = next.Unix()
				i++
			} else {
				s.d.Tasks = append(s.d.Tasks[:i], s.d.Tasks[i+1:]...) // once-tasks are done
			}
		}
		if changed {
			s.saveLocked()
		}
		s.mu.Unlock()
	}
}

// background: restore (primary only) or back up, between conversations.
func (s *Scheduler) background() {
	s.mu.Lock()
	restore, pending := s.restorePending, s.backupPending
	s.mu.Unlock()
	if !s.Run_.Idle() || !(restore || pending) || !backup.Available() || time.Now().Before(s.retryAt) {
		return
	}
	if restore {
		data, err := backup.Load(backupFile, backupSubject())
		if err != nil {
			log.Printf("[Tasks] restore: %v", err)
			s.retryAt = time.Now().Add(time.Duration(prompts.Int("MEMORY_OUTBOX_RETRY_MS", 300000)) * time.Millisecond)
			return
		}
		s.mu.Lock()
		defer s.mu.Unlock()
		s.restorePending = false
		var saved doc
		if data == nil || store.Unmarshal(data, &saved) != nil {
			return
		}
		if len(s.d.Tasks) > 0 { // created meanwhile: those count, the list becomes the backup
			s.backupPending = true
			return
		}
		s.d = saved
		s.normalize()
		now := time.Now()
		for i := range s.d.Tasks { // runs missed meanwhile are skipped
			if s.d.Tasks[i].Next < now.Unix() {
				next, _ := NextRun(s.d.Tasks[i].Schedule, now)
				s.d.Tasks[i].Next = next.Unix()
				if next.IsZero() {
					s.d.Tasks[i].Next = 0
				}
			}
		}
		s.saveLocked()
		s.backupPending = false
		log.Printf("[Tasks] restored from the backup: %d", len(s.d.Tasks))
		return
	}
	s.mu.Lock()
	text := prompts.Name() + "' geplante Aufgaben, Stand " + time.Now().Format("02.01.2006 15:04") +
		". Stellt sie nach einem Gerätewechsel wieder her; bitte nicht bearbeiten.\n\n"
	for _, t := range s.d.Tasks {
		text += "- " + s.describe(t) + ": " + t.Text + "\n"
	}
	data, _ := store.Marshal(s.d)
	s.backupPending = false
	s.mu.Unlock()
	if err := backup.Store(backupFile, backupSubject(), text, data); err != nil {
		log.Printf("[Tasks] backup: %v", err)
		s.mu.Lock()
		s.backupPending = true
		s.retryAt = time.Now().Add(time.Duration(prompts.Int("MEMORY_OUTBOX_RETRY_MS", 300000)) * time.Millisecond)
		s.mu.Unlock()
	}
}

// RetryNow: a restore or backup that failed for want of Google is tried again at once.
func (s *Scheduler) RetryNow() {
	s.mu.Lock()
	s.retryAt = time.Time{}
	s.mu.Unlock()
}

// Context: the scheduled tasks for the prompts, "" without any.
func (s *Scheduler) Context() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.d.Tasks) == 0 {
		return ""
	}
	text := "Scheduled tasks:\n"
	for _, t := range s.d.Tasks {
		text += "- " + s.describe(t) + "\n"
	}
	return text
}

// --- Tools ---------------------------------------------------------------------------------

// create: room is where the conversation that ordered it was held ("" = not at a speaker).
func (s *Scheduler) create(args map[string]any, person, here string) string {
	title, kind, text := tools.String(args, "title"), tools.String(args, "kind"), tools.String(args, "text")
	if title == "" || text == "" {
		return "Error: give title and text"
	}
	if kind != "reminder" && kind != "prompt" {
		return "Error: kind is reminder or prompt"
	}
	// The backend fills unused optional parameters with empty values ("" / 0 / []): those are
	// simply ignored here.
	sc := Schedule{Type: tools.String(args, "schedule"), Time: tools.String(args, "time"), Date: tools.String(args, "date")}
	if items, ok := args["days"].([]any); ok {
		for _, d := range items {
			if v, ok := d.(float64); ok {
				sc.Days = append(sc.Days, int(v))
			}
		}
	}
	sc.DayOfMonth, _ = tools.Int(args, "day_of_month")
	sc.EveryHours, _ = tools.Int(args, "every_hours")
	now := time.Now()
	if in, _ := tools.Int(args, "in_minutes"); in > 0 && sc.Type == "once" { // "in 10 Minuten"
		t := now.Add(time.Duration(in) * time.Minute)
		sc.Date, sc.Time = t.Format("2006-01-02"), t.Format("15:04")
	}
	next, errText := NextRun(sc, now)
	if next.IsZero() {
		if errText == "" {
			errText = "the time is in the past"
		}
		return "Error: " + errText
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if n := prompts.Int("TASK_MAX", 30); len(s.d.Tasks) >= n {
		return fmt.Sprintf("Error: already %d tasks, delete one first", n)
	}
	deliver, room := tools.String(args, "deliver"), strings.TrimSpace(tools.String(args, "room"))
	if kind == "reminder" {
		if deliver != "speak" && deliver != "both" && deliver != "call" {
			deliver = "telegram" // The assistant cannot know the room: the phone, unless the user names a speaker
		}
	} else if deliver == "" {
		deliver = "email"
	}
	if deliver != "speak" && deliver != "both" {
		room = ""
	} else if room == "" {
		room = here // "sag es mir" at a speaker: said where it was ordered (#65)
	}
	t := Task{ID: s.d.NextID, Title: title, Kind: kind, Text: text, Deliver: deliver, Room: room, Schedule: sc,
		Next: next.Unix(), Created: now.Format("2006-01-02"), Person: person}
	s.d.NextID++
	s.d.Tasks = append(s.d.Tasks, t)
	s.saveLocked()
	log.Printf("[Tasks] created: %s", s.describe(t))
	return "Created: " + s.describe(t)
}

// AddTools registers create_task, list_tasks, delete_task, pause_task and run_task. Everything
// needs the clock (schedules, next runs).
func (s *Scheduler) AddTools(r *tools.Registry) {
	clock := func(f func() string) string {
		if !netx.TimeSynced {
			return noClock
		}
		return f()
	}
	r.Add(tools.Tool{
		Name: "create_task",
		Description: "Plans a task for later or on a schedule, after the user confirmed it. kind reminder: text is " +
			"sent to the person's phone as a message at the time (deliver telegram, the default: you do not know " +
			"which room they will be in); announced on a speaker only when they ask for it (deliver speak, room " +
			"the speaker they name, '' = this one: an alarm clock, 'sag es mir hier'); deliver call (a phone call) " +
			"when they ask or it is very important. kind prompt: text is an instruction the assistant carries " +
			"out at the time with all its tools (news, weather, web search, smart home, mail) and delivers as " +
			"deliver says; a condition in the instruction ('only if it rains') lets it stay silent otherwise. " +
			"One-off or delayed device actions ('in 10 minutes switch the light on') are a prompt task with " +
			"deliver none; recurring device control belongs in create_home_automation. The instruction names the " +
			"tool for then: wake-up with music = play_music at low volume; 'ruf mich um 7 an' = phone_call; " +
			"'weck mich hier' = start_conversation (deliver none, until they say they are awake); a sleep timer = " +
			"music_control pause; 'erinnere mich rechtzeitig loszufahren' = an hour before, travel_time, then say " +
			"when to leave. 'In 10 Minuten' is in_minutes. To change a task: delete_task, then create_task.",
		Parameters: `{"type": "object", "additionalProperties": false,
            "properties": {
              "title": {"type": "string", "description": "short German title, e.g. 'Tech-News'"},
              "kind": {"type": "string", "enum": ["reminder", "prompt"]},
              "text": {"type": "string", "description": "reminder: the exact German sentence the assistant will say, addressed to the user (e.g. 'Die Performance Messe ist am Samstag, 4. September, in Metzingen.'); prompt: the complete instruction in German"},
              "deliver": {"type": "string", "enum": ["email", "speak", "both", "telegram", "call", "none"], "description": "prompt tasks: email to the owner, speak (announced on a speaker), both (email and announcement), a message to the person's chat, call (told by phone) or none (only act). reminders: telegram (default, a message to the person's phone), speak (announced on the speaker in room), both (announced and messaged) or call (told by phone)"},
              "room": {"type": "string", "description": "speak and both: the assistant speaker the user named (room or name), '' = the speaker this conversation is held at"},
              "schedule": {"type": "string", "enum": ["once", "daily", "weekly", "monthly", "interval"]},
              "time": {"type": "string", "description": "HH:MM (24 h), all but interval"},
              "date": {"type": "string", "description": "once: YYYY-MM-DD, '' = today or tomorrow"},
              "in_minutes": {"type": "integer", "description": "once: in this many minutes instead of date/time"},
              "days": {"type": "array", "items": {"type": "integer"}, "description": "weekly: 1 = Monday … 7 = Sunday"},
              "day_of_month": {"type": "integer"},
              "every_hours": {"type": "integer", "description": "interval: hours between runs"}},
            "required": ["title", "kind", "text", "schedule"]}`,
		Run: func(args map[string]any, ctx tools.Context) string {
			return clock(func() string { return s.create(args, ctx.Person, ctx.Room) })
		},
	})
	r.Add(tools.Tool{
		Name: "list_tasks", Description: "Lists the planned tasks with id, schedule and next run.",
		Parameters: `{"type": "object", "additionalProperties": false, "properties": {}, "required": []}`,
		Run: func(map[string]any, tools.Context) string {
			return clock(func() string {
				s.mu.Lock()
				defer s.mu.Unlock()
				if len(s.d.Tasks) == 0 {
					return "No scheduled tasks."
				}
				text := ""
				for _, t := range s.d.Tasks {
					text += s.describe(t) + ": " + t.Text + "\n"
				}
				return text
			})
		},
	})
	r.Add(tools.Tool{
		Name: "delete_task", Description: "Deletes a planned task by id.",
		Parameters: `{"type": "object", "additionalProperties": false, "properties": {"id": {"type": "integer"}}, "required": ["id"]}`,
		Run: func(args map[string]any, _ tools.Context) string {
			return clock(func() string {
				id, _ := tools.Int(args, "id")
				s.mu.Lock()
				defer s.mu.Unlock()
				for i, t := range s.d.Tasks {
					if t.ID == id {
						s.d.Tasks = append(s.d.Tasks[:i], s.d.Tasks[i+1:]...)
						s.saveLocked()
						return fmt.Sprintf("Task #%d deleted.", id)
					}
				}
				return fmt.Sprintf("Error: no task #%d", id)
			})
		},
	})
	r.Add(tools.Tool{
		Name: "pause_task", Description: "Pauses a planned task (paused true) or resumes it (false).",
		Parameters: `{"type": "object", "additionalProperties": false,
            "properties": {"id": {"type": "integer"}, "paused": {"type": "boolean"}}, "required": ["id", "paused"]}`,
		Run: func(args map[string]any, _ tools.Context) string {
			return clock(func() string {
				id, _ := tools.Int(args, "id")
				paused := tools.Bool(args, "paused", true)
				s.mu.Lock()
				defer s.mu.Unlock()
				t := s.findLocked(id)
				if t == nil {
					return fmt.Sprintf("Error: no task #%d", id)
				}
				t.Paused = paused
				if !paused { // continue from now on, no catching up
					next, _ := NextRun(t.Schedule, time.Now())
					t.Next = 0
					if !next.IsZero() {
						t.Next = next.Unix()
					}
				}
				s.saveLocked()
				if paused {
					return "Paused: " + s.describe(*t)
				}
				return "Running again: " + s.describe(*t)
			})
		},
	})
	r.Add(tools.Tool{
		Name: "run_task", Description: "Runs a planned task right now once (e.g. to try it out).",
		Parameters: `{"type": "object", "additionalProperties": false, "properties": {"id": {"type": "integer"}}, "required": ["id"]}`,
		Run: func(args map[string]any, _ tools.Context) string {
			return clock(func() string {
				id, _ := tools.Int(args, "id")
				s.mu.Lock()
				t := s.findLocked(id)
				var copyTask Task
				if t != nil {
					copyTask = *t
				}
				s.mu.Unlock()
				if t == nil {
					return fmt.Sprintf("Error: no task #%d", id)
				}
				if !s.queue(copyTask) {
					return "Error: too many tasks at once"
				}
				if s.Run_.Open() && copyTask.Kind != "reminder" {
					return fmt.Sprintf("Task #%d runs now (once the conversation is over).", id)
				}
				return fmt.Sprintf("Task #%d runs now.", id)
			})
		},
	})
}

// backupSubject: the mail with the tasks in the mailbox, named after the assistant.
func backupSubject() string { return prompts.Name() + "-Aufgaben" }
