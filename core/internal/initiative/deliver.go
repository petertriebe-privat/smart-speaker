package initiative

import (
	"fmt"
	"log"
	"strings"
	"time"

	"echo-assistant/internal/prompts"
	"echo-assistant/internal/textutil"
	"echo-assistant/internal/tools"
)

// Step 3: pending suggestions reach the user. Most go into the next conversation's
// instructions (Context), where the assistant brings them up and records the answer
// (answer_suggestion); one handed to INITIATIVE_HAND_MAX conversations without an answer counts
// as ignored. A leaving time goes to a background run at once, which plans when to remind;
// urgent ones (now, message) too.

// deliverAt: when a background run delivers a pending suggestion; 0 = in a conversation.
func deliverAt(s Suggestion, state, when string, now time.Time) int64 {
	if state != "pending" {
		return 0
	}
	// A leaving time is planned at once: the run decides itself when to remind and whether to look
	// at the traffic again (a fixed check 75 min before was too late for a long way).
	if s.Kind == "leave_time" || when == "now" || when == "message" {
		return now.Unix()
	}
	return 0
}

// deliverDue: the due suggestions, one background run per person for all that are due together
// (three hints on one afternoon came as three announcements in 40 s, 24.10.2026). Between
// conversations only.
func (p *Pass) deliverDue() {
	if p.Deliver == nil {
		return
	}
	batches := map[string][]Suggestion{}
	var order []string
	for _, s := range p.Journal.Due() {
		if s.Until != 0 && s.Until < p.Journal.Now().Unix() {
			p.Journal.Set(s.ID, "dropped", "over before it was told")
			continue
		}
		if s.When != "now" && p.Quiet != nil && p.Quiet(s.Person) {
			p.Journal.Set(s.ID, "dropped", "hints paused")
			continue
		}
		if batches[s.Person] == nil {
			order = append(order, s.Person)
		}
		batches[s.Person] = append(batches[s.Person], s)
	}
	for _, person := range order {
		batch := batches[person]
		answer := p.Deliver(batch)
		for _, s := range batch {
			log.Printf("[Initiative] %s told: %.200s", s.ID, answer)
			p.Journal.Set(s.ID, "told", textutil.Prefix(answer, 200))
		}
	}
}

// Context: the person's pending suggestions for the instructions of conversation conv ("" =
// not a voice conversation: none); remembered as handed to it.
func (p *Pass) Context(person, conv string) string {
	if conv == "" || person == "" {
		return ""
	}
	list := p.Journal.ForConversation(person)
	if len(list) == 0 {
		return ""
	}
	if n := prompts.Int("INITIATIVE_CONTEXT_MAX", 3); len(list) > n {
		list = list[:n]
	}
	var b strings.Builder
	b.WriteString("\n" + prompts.Text("INITIATIVE_CONTEXT") + "\n")
	ids := make([]string, len(list))
	for i, s := range list {
		ids[i] = s.ID
		fmt.Fprintf(&b, "- [%s] (%s) %s\n", s.ID, s.Level, s.Text)
	}
	p.mu.Lock()
	if p.handed == nil {
		p.handed = map[string][]string{}
	}
	p.handed[conv] = ids
	p.mu.Unlock()
	return b.String()
}

// ConversationOver: the suggestions handed to conv that got no answer count once more.
func (p *Pass) ConversationOver(conv string) {
	p.mu.Lock()
	ids := p.handed[conv]
	delete(p.handed, conv)
	p.mu.Unlock()
	if len(ids) > 0 {
		p.Journal.Handed(ids)
	}
}

// AddTools registers answer_suggestion (on every Echo; a secondary hands it to the primary).
func (j *Journal) AddTools(r *tools.Registry) {
	r.Add(tools.Tool{
		Name:    "answer_suggestion",
		Private: true, // the suggestions are the admin's
		Description: "Records the user's answer to one of your own suggestions (the ids in 'Your own suggestions' of " +
			"your instructions): accepted when they want it or liked it, declined when they do not. Call it right after " +
			"they answer, also for 'ja, mach das' before you do it. A lasting wish about such suggestions ('sowas " +
			"musst du mir nicht sagen', 'erinnere mich da immer dran') is a rule too: save_rule.",
		Parameters: `{"type": "object", "additionalProperties": false,
            "properties": {"id": {"type": "string", "description": "e.g. s12"},
                           "answer": {"type": "string", "enum": ["accepted", "declined"]},
                           "reason": {"type": "string", "description": "the user's words in short, '' if none"}},
            "required": ["id", "answer", "reason"]}`,
		Run: func(args map[string]any, _ tools.Context) string {
			id := strings.Trim(strings.TrimSpace(tools.String(args, "id")), "[]")
			answer := tools.String(args, "answer")
			if answer != "accepted" && answer != "declined" {
				return "Error: answer is accepted or declined"
			}
			if err := j.Set(id, answer, tools.String(args, "reason")); err != nil {
				return "Error: " + err.Error()
			}
			return "Recorded: " + id + " " + answer + "."
		},
	})
}
