package memory

// Learning from behaviour (#4 of the 26.10.2026 research, "lernen aus meinem Verhalten"): the
// memory learned only from what people said. The behaviour log keeps what they did: quick
// commands, the tools that ran for them (music, lights, timers; BEHAVIOUR_TOOLS), arrivals and
// departures from Home Assistant's presence. The nightly dream gets the last
// DREAM_BEHAVIOUR_DAYS days of it next to the conversations and may add habits as facts
// ("Gewohnheit: ..."), with the same rule as for any new fact: evidence from DREAM_MIN_DAYS
// different days, checked against the entries it was given. Only the primary keeps the log (a
// secondary sends its entries there, log_behaviour); it is never in a prompt itself.

import (
	"fmt"
	"log"
	"strings"
	"sync"
	"time"

	"echo-assistant/internal/prompts"
	"echo-assistant/internal/store"
	"echo-assistant/internal/tools"
)

const behaviourFile = "memory/behaviour.json"

// behaviourEntry is one thing someone did.
type behaviourEntry struct {
	At     string `json:"at"` // "2006-01-02 15:04", the dream's evidence
	T      int64  `json:"t"`
	Person string `json:"person,omitempty"`
	Kind   string `json:"kind"` // Befehl, Tool, Anwesenheit
	What   string `json:"what"`
	Room   string `json:"room,omitempty"`
}

var behaviourMu sync.Mutex // the log file; never held with m.mu

// LogBehaviour notes what a person did ("" = unknown, at home). kind: "Befehl" (a quick
// command), "Tool" (a tool that ran for them), "Anwesenheit"; room: where ("" = unknown).
func (m *Memory) LogBehaviour(person, kind, what, room string) {
	what = strings.TrimSpace(what)
	if what == "" || !prompts.Bool("BEHAVIOUR_LOG", true) {
		return
	}
	now := time.Now()
	if now.Year() < 2024 { // the clock is not set yet
		return
	}
	behaviourMu.Lock()
	defer behaviourMu.Unlock()
	var list []behaviourEntry
	store.LoadJSON(behaviourFile, &list)
	keep := now.Unix() - int64(prompts.Int("BEHAVIOUR_KEEP_DAYS", 30))*86400
	first := 0
	for first < len(list) && list[first].T < keep {
		first++
	}
	list = append(list[first:], behaviourEntry{At: now.Format("2006-01-02 15:04"), T: now.Unix(), Person: person,
		Kind: kind, What: cut(what, 160), Room: room})
	if limit := prompts.Int("BEHAVIOUR_MAX", 4000); len(list) > limit {
		list = list[len(list)-limit:]
	}
	if err := store.SaveJSON(behaviourFile, list); err != nil {
		log.Printf("[Memory] behaviour not saved: %v", err)
	}
}

func cut(s string, n int) string {
	if r := []rune(s); len(r) > n {
		return string(r[:n]) + " …"
	}
	return s
}

var weekdays = []string{"So", "Mo", "Di", "Mi", "Do", "Fr", "Sa"}

// recentBehaviour: the last DREAM_BEHAVIOUR_DAYS days of the log, oldest first, at most
// DREAM_BEHAVIOUR_CHARS (the newest kept). times gets each entry's "at": valid evidence.
func (m *Memory) recentBehaviour(times map[string]bool) string {
	behaviourMu.Lock()
	var list []behaviourEntry
	store.LoadJSON(behaviourFile, &list)
	behaviourMu.Unlock()
	since := time.Now().Unix() - int64(prompts.Int("DREAM_BEHAVIOUR_DAYS", 14))*86400
	var lines []string
	for _, e := range list {
		if e.T < since {
			continue
		}
		where := weekdays[time.Unix(e.T, 0).Weekday()]
		if e.Room != "" {
			where += ", " + e.Room
		}
		if e.Person != "" {
			where += ", " + m.People.Name(e.Person)
		}
		lines = append(lines, fmt.Sprintf("[%s] (%s) %s: %s\n", e.At, where, e.Kind, e.What))
	}
	limit, used, first := prompts.Int("DREAM_BEHAVIOUR_CHARS", 20000), 0, len(lines)
	for first > 0 && used+len(lines[first-1]) <= limit {
		first--
		used += len(lines[first])
	}
	var b strings.Builder
	for _, line := range lines[first:] {
		b.WriteString(line)
		times[line[1:17]] = true
	}
	return b.String()
}

// AddBehaviourTool registers log_behaviour: for the console only, so a secondary Echo's entries
// reach the primary through the shared tools (never offered to a model).
func (m *Memory) AddBehaviourTool(r *tools.Registry) {
	r.Add(tools.Tool{
		Name:        "log_behaviour",
		Description: "Device only: notes what a person did, for learning habits.",
		Parameters: `{"type": "object", "additionalProperties": false,
            "properties": {"person": {"type": "string"}, "kind": {"type": "string"}, "what": {"type": "string"},
                           "room": {"type": "string"}}, "required": ["person", "kind", "what", "room"]}`,
		Channels: tools.Console,
		Run: func(args map[string]any, _ tools.Context) string {
			m.LogBehaviour(tools.String(args, "person"), tools.String(args, "kind"), tools.String(args, "what"),
				tools.String(args, "room"))
			return "ok"
		},
	})
}
