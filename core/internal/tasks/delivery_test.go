package tasks

import (
	"testing"

	"echo-assistant/internal/config"
	"echo-assistant/internal/people"
	"echo-assistant/internal/store"
)

// Reminders go to the phone unless the user named a speaker (27.10.2026: results and reminders
// were announced in the living room while the owner was elsewhere).
func TestReminderDelivery(t *testing.T) {
	store.Dir = t.TempDir()
	reg := people.New(config.Secrets{"OWNER_NAME": "Admin", "PHONE_OWNER": "+491701234567"})
	var messages, spoken, calls []string
	wa := "sent"
	reg.Channels.WhatsApp = func(text, _ string) string {
		messages = append(messages, text)
		return wa
	}
	s := New(reg, Runner{
		Announce: func(room, text string) string {
			spoken = append(spoken, room+"|"+text)
			return "ok"
		},
		Call: func(_, task string) string {
			calls = append(calls, task)
			return "Error: no line"
		},
		Open: func() bool { return false },
		Idle: func() bool { return true },
	})

	here := ""
	create := func(args map[string]any) Task {
		args["title"], args["kind"], args["text"], args["schedule"], args["in_minutes"] = "T", "reminder", "Wäsche", "once", float64(5)
		if out := s.create(args, "", here); out[:7] != "Created" {
			t.Fatalf("create: %s", out)
		}
		return s.d.Tasks[len(s.d.Tasks)-1]
	}

	// default: a message, nothing spoken; a room without speak is dropped
	job := create(map[string]any{"room": "Küche"})
	if job.Deliver != "telegram" || job.Room != "" {
		t.Fatalf("default = %q in %q", job.Deliver, job.Room)
	}
	s.runJob(job)
	if len(messages) != 1 || len(spoken) != 0 {
		t.Fatalf("default: messages %v, spoken %v", messages, spoken)
	}

	// speak on the speaker the user named
	job = create(map[string]any{"deliver": "speak", "room": "Küche"})
	s.runJob(job)
	if len(spoken) != 1 || spoken[0] != "Küche|Erinnerung: Wäsche" || len(messages) != 1 {
		t.Fatalf("speak: messages %v, spoken %v", messages, spoken)
	}

	// "sag es mir" at a speaker without a room: said where it was ordered (#65); a message stays
	// a message
	here = "Bad"
	if job = create(map[string]any{"deliver": "speak"}); job.Room != "Bad" {
		t.Fatalf("speak ordered in the bathroom: room %q", job.Room)
	}
	if job = create(map[string]any{}); job.Room != "" {
		t.Fatalf("a message got the room %q", job.Room)
	}
	here = ""

	// call: a failed call becomes a message
	job = create(map[string]any{"deliver": "call"})
	s.runJob(job)
	if len(calls) != 1 || len(messages) != 2 || len(spoken) != 1 {
		t.Fatalf("call: calls %v, messages %v, spoken %v", calls, messages, spoken)
	}

	// a message that cannot be sent is announced here rather than lost
	wa = "Error: down"
	s.runJob(create(map[string]any{}))
	if len(spoken) != 2 || spoken[1] != "|Erinnerung: Wäsche" {
		t.Fatalf("fallback: spoken %v", spoken)
	}
}
