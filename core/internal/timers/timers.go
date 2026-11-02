// Package timers: timers and alarms that ring on the Echo itself, without GPT Live or the
// scheduler's agent (#25). Each Echo keeps its own (timers.json in its state dir): a timer rings
// in the room where it was set. "Timer zehn Minuten" is a quick command (Parse, run by the device
// like "Pause"); in a conversation the tools set_timer, list_timers and cancel_timer do the same.
// A due timer calls Ring; the device plays the alarm until Stop (the action button, the wake word,
// "Timer aus") or TIMER_RING_MAX_S.
package timers

import (
	"fmt"
	"log"
	"sort"
	"strings"
	"sync"
	"time"

	"echo-assistant/internal/prompts"
	"echo-assistant/internal/store"
	"echo-assistant/internal/tools"
)

const file = "timers.json"

// Timer is one timer or alarm.
type Timer struct {
	ID    int       `json:"id"`
	Kind  string    `json:"kind"` // "timer" or "alarm"
	Label string    `json:"label,omitempty"`
	At    time.Time `json:"at"`
	Set   time.Time `json:"set"`
}

// Service holds the timers of this device.
type Service struct {
	// Ring is called when a timer is due (from the service's goroutine); the device plays the
	// alarm until Stop. Nil = only logged.
	Ring func(t Timer)
	// Now is the clock (tests); nil = time.Now.
	Now func() time.Time

	mu      sync.Mutex
	list    []Timer
	next    int
	ringing []Timer // due and not stopped yet
	wake    chan struct{}
}

// New loads the timers kept in the state dir.
func New() *Service {
	s := &Service{wake: make(chan struct{}, 1)}
	store.LoadJSON(file, &s.list)
	for _, t := range s.list {
		s.next = max(s.next, t.ID)
	}
	return s
}

func (s *Service) now() time.Time {
	if s.Now != nil {
		return s.Now()
	}
	return time.Now()
}

func (s *Service) saveLocked() {
	list := s.list
	if list == nil {
		list = []Timer{}
	}
	if err := store.SaveJSON(file, list); err != nil {
		log.Printf("[Timer] not saved: %v", err)
	}
}

func (s *Service) kick() {
	select {
	case s.wake <- struct{}{}:
	default:
	}
}

// Add sets a timer (after > 0) or an alarm at hour:minute (the next such time). Returns it.
func (s *Service) Add(kind string, after time.Duration, hour, minute int, label string) Timer {
	now := s.now()
	at := now.Add(after)
	if kind == "alarm" {
		at = time.Date(now.Year(), now.Month(), now.Day(), hour, minute, 0, 0, now.Location())
		if !at.After(now) {
			at = at.AddDate(0, 0, 1)
		}
	}
	s.mu.Lock()
	s.next++
	t := Timer{ID: s.next, Kind: kind, Label: label, At: at, Set: now}
	s.list = append(s.list, t)
	sort.Slice(s.list, func(i, j int) bool { return s.list[i].At.Before(s.list[j].At) })
	s.saveLocked()
	s.mu.Unlock()
	s.kick()
	log.Printf("[Timer] #%d %s at %s %s", t.ID, kind, t.At.Format("15:04:05"), label)
	return t
}

// Cancel removes one timer (id > 0) or all (id 0); the count removed.
func (s *Service) Cancel(id int) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	kept := s.list[:0]
	n := 0
	for _, t := range s.list {
		if id == 0 || t.ID == id {
			n++
			continue
		}
		kept = append(kept, t)
	}
	s.list = kept
	s.saveLocked()
	return n
}

// List: the timers still to come, soonest first.
func (s *Service) List() []Timer {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]Timer(nil), s.list...)
}

// Ringing: a timer rings now.
func (s *Service) Ringing() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.ringing) > 0
}

// Stop ends the ringing; the timers that rang, nil if none did.
func (s *Service) Stop() []Timer {
	s.mu.Lock()
	defer s.mu.Unlock()
	rang := s.ringing
	s.ringing = nil
	return rang
}

// Run fires due timers until the process ends. A timer due while the device was off rings once
// on start if it is at most TIMER_LATE_MAX_S late, else it is dropped (logged).
func (s *Service) Run() {
	for {
		s.mu.Lock()
		now := s.now()
		var due []Timer
		kept := s.list[:0]
		for _, t := range s.list {
			if t.At.After(now) {
				kept = append(kept, t)
				continue
			}
			if late := now.Sub(t.At); late > time.Duration(prompts.Int("TIMER_LATE_MAX_S", 600))*time.Second {
				log.Printf("[Timer] #%d dropped: due %s ago", t.ID, late.Round(time.Second))
				continue
			}
			due = append(due, t)
		}
		s.list = kept
		if len(due) > 0 {
			s.ringing = append(s.ringing, due...)
			s.saveLocked()
		}
		wait := time.Minute
		if len(s.list) > 0 {
			wait = min(wait, max(50*time.Millisecond, s.list[0].At.Sub(now)))
		}
		s.mu.Unlock()
		for _, t := range due {
			log.Printf("[Timer] #%d rings (%s)", t.ID, t.Label)
			if s.Ring != nil {
				s.Ring(t)
			}
		}
		select {
		case <-s.wake:
		case <-time.After(wait):
		}
	}
}

// Spoken: German words for a duration ("10 Minuten", "1 Stunde und 5 Minuten", "45 Sekunden").
func Spoken(d time.Duration) string {
	d = d.Round(time.Second)
	h, m, sec := int(d.Hours()), int(d.Minutes())%60, int(d.Seconds())%60
	var parts []string
	unit := func(n int, one, many string) {
		if n == 1 {
			parts = append(parts, "1 "+one)
		} else if n > 1 {
			parts = append(parts, fmt.Sprintf("%d %s", n, many))
		}
	}
	unit(h, "Stunde", "Stunden")
	unit(m, "Minute", "Minuten")
	if h == 0 && m < 5 {
		unit(sec, "Sekunde", "Sekunden")
	}
	if len(parts) == 0 {
		return "wenigen Sekunden"
	}
	if len(parts) == 1 {
		return parts[0]
	}
	return strings.Join(parts[:len(parts)-1], ", ") + " und " + parts[len(parts)-1]
}

// Confirmation: what the device says after setting t (German, spoken).
func Confirmation(t Timer, now time.Time) string {
	what := "Timer"
	if t.Label != "" {
		what = "Timer für " + t.Label
	}
	if t.Kind == "alarm" {
		day := ""
		if t.At.YearDay() != now.YearDay() {
			day = " morgen"
		}
		return "Wecker für" + day + " " + t.At.Format("15:04") + " Uhr gestellt."
	}
	return what + ", " + Spoken(t.At.Sub(t.Set)) + ", ab jetzt."
}

// describe: one line per timer for the model.
func describe(list []Timer, now time.Time) string {
	if len(list) == 0 {
		return "No timers or alarms on this speaker."
	}
	var b strings.Builder
	for _, t := range list {
		label := ""
		if t.Label != "" {
			label = " (" + t.Label + ")"
		}
		fmt.Fprintf(&b, "#%d %s%s at %s, in %s\n", t.ID, t.Kind, label, t.At.Format("02.01. 15:04:05"), Spoken(t.At.Sub(now)))
	}
	return strings.TrimSuffix(b.String(), "\n")
}

// AddTools registers set_timer, list_timers and cancel_timer: for conversations at this speaker.
func (s *Service) AddTools(r *tools.Registry) {
	r.Add(tools.Tool{
		Name: "set_timer",
		Description: "Sets a timer or an alarm that rings on this speaker (in this room), e.g. 'Timer zehn Minuten', " +
			"'weck mich um halb sieben'. Not for reminders with a message (create_task) and not for recurring alarms. " +
			"Tell the user briefly what you set.",
		Parameters: `{"type": "object", "additionalProperties": false,
            "properties": {"kind": {"type": "string", "enum": ["timer", "alarm"]},
                           "minutes": {"type": "number", "description": "timer: duration in minutes (0.5 = 30 seconds); alarm: 0"},
                           "time": {"type": "string", "description": "alarm: HH:MM (the next such time); timer: ''"},
                           "label": {"type": "string", "description": "what it is for in German ('Nudeln'), '' for none"}},
            "required": ["kind", "minutes", "time", "label"]}`,
		Channels: tools.Everywhere,
		Run: func(args map[string]any, _ tools.Context) string {
			label := tools.String(args, "label")
			if tools.String(args, "kind") == "alarm" {
				var h, m int
				if _, err := fmt.Sscanf(tools.String(args, "time"), "%d:%d", &h, &m); err != nil || h > 23 || m > 59 {
					return "Error: time must be HH:MM"
				}
				t := s.Add("alarm", 0, h, m, label)
				return Confirmation(t, s.now())
			}
			minutes, _ := args["minutes"].(float64)
			if minutes <= 0 || minutes > 24*60 {
				return "Error: minutes must be between 0 and 1440"
			}
			t := s.Add("timer", time.Duration(minutes*float64(time.Minute)), 0, 0, label)
			return Confirmation(t, s.now())
		},
	})
	r.Add(tools.Tool{
		Name:        "list_timers",
		Description: "The timers and alarms on this speaker with the time left ('wie lange noch?').",
		Parameters:  `{"type": "object", "additionalProperties": false, "properties": {}, "required": []}`,
		Channels:    tools.Everywhere,
		Run:         func(map[string]any, tools.Context) string { return describe(s.List(), s.now()) },
	})
	r.Add(tools.Tool{
		Name:        "cancel_timer",
		Description: "Cancels a timer or alarm on this speaker by its id from list_timers, or all with id 0; also stops one that rings.",
		Parameters: `{"type": "object", "additionalProperties": false,
            "properties": {"id": {"type": "integer", "description": "0 = all"}}, "required": ["id"]}`,
		Channels: tools.Everywhere,
		Run: func(args map[string]any, _ tools.Context) string {
			id, _ := args["id"].(float64)
			stopped := len(s.Stop())
			n := s.Cancel(int(id))
			return fmt.Sprintf("Cancelled %d, stopped %d ringing.", n, stopped)
		},
	})
}
