package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"echo-assistant/internal/agent"
	"echo-assistant/internal/live"
	"echo-assistant/internal/openai"
	"echo-assistant/internal/prompts"
	"echo-assistant/internal/textutil"
	"echo-assistant/internal/tools"
)

// Background tasks (#82, step 8): from a conversation the assistant hands a task that takes a while
// (research, a comparison, a plan) to a run of its own and the conversation goes on or ends
// (think_harder makes it wait 30-90 s). Several run at once, at most BACKGROUND_MAX (a run is
// HTTP calls to the backend: little RAM and CPU on the Echo). The result goes into the conversation
// it came from as a notice while that is still live, otherwise to the person as a message
// (Telegram, or WhatsApp for whom has no Telegram).

type bgTask struct {
	id           int
	task, person string
	label        string // the conversation it came from: "speaker" or "phone"
	started      time.Time
	thorough     bool
	dropped      bool // the user does not want the result any more
}

type background struct {
	mu      sync.Mutex
	next    int
	running map[int]*bgTask
}

func (d *device) addBackgroundTools() {
	bg := &background{running: map[int]*bgTask{}}
	d.registry.Add(tools.Tool{
		Name: "start_background_task",
		Description: "Hands a task that takes a while to a run of its own in the background, with all tools, so the " +
			"conversation goes on or can end: research across several sources, comparisons and recommendations, " +
			"plans, longer texts, anything the user does not need to wait for ('schau mal nach …, sag mir dann " +
			"Bescheid'). Returns at once; tell the user you will come back to it. The result always goes to the user " +
			"as a message (to keep), and into this conversation as a note while it lasts. thorough: careful thought " +
			"(slower). Not for what you can answer or do yourself in a moment. Work that takes longer than a few " +
			"minutes, or needs a computer, a browser or code, goes to the server agent instead (its " +
			"delegate_task: it runs on the owner's subscription without API costs, and its result comes back by " +
			"itself, into a running conversation too). Give the complete task in German with everything the user " +
			"said that matters: the run does not know this conversation.",
		Parameters: `{"type": "object", "additionalProperties": false,
            "properties": {"task": {"type": "string", "description": "the complete task in German"},
                           "thorough": {"type": "boolean", "description": "true: careful thought for comparisons, plans, recommendations"}},
            "required": ["task", "thorough"]}`,
		Channels: tools.Voice | tools.Console,
		Run: func(args map[string]any, ctx tools.Context) string {
			return d.startBackground(bg, args, ctx)
		},
	})
	d.registry.Add(tools.Tool{
		Name: "list_background_tasks",
		Description: "Lists the tasks running in the background (start_background_task) with id and how long they run. " +
			"With drop (an id) the user does not want that result any more: it is not told.",
		Parameters: `{"type": "object", "additionalProperties": false,
            "properties": {"drop": {"type": "integer", "description": "id of a task whose result is not wanted, 0 = none"}},
            "required": ["drop"]}`,
		Channels: tools.Voice | tools.Console,
		Run: func(args map[string]any, _ tools.Context) string {
			drop, _ := tools.Int(args, "drop")
			return bg.list(drop)
		},
	})
}

// addFanOutTool: research_parallel for agent runs (a background task, think_harder): several
// independent questions at once in quick runs of their own, the answers together; faster and
// cheaper than one long run going through them one by one.
func (d *device) addFanOutTool() {
	d.registry.Add(tools.Tool{
		Name: "research_parallel",
		Description: "Asks several independent questions at once, each in a quick run of its own with all tools " +
			"(e.g. the offers of three providers, the opening hours of four shops), and returns the answers " +
			"together. Use it when a task splits into parts that do not depend on each other; then compare or " +
			"combine the answers yourself.",
		Parameters: `{"type": "object", "additionalProperties": false,
            "properties": {"questions": {"type": "array", "items": {"type": "string"},
                                         "description": "2 to 6 complete German questions, each understandable on its own"}},
            "required": ["questions"]}`,
		Channels: tools.Agent,
		Run: func(args map[string]any, ctx tools.Context) string {
			var questions []string
			if items, ok := args["questions"].([]any); ok {
				for _, q := range items {
					if s, ok := q.(string); ok && strings.TrimSpace(s) != "" {
						questions = append(questions, strings.TrimSpace(s))
					}
				}
			}
			if n := prompts.Int("FANOUT_MAX", 6); len(questions) > n {
				questions = questions[:n]
			}
			if len(questions) == 0 {
				return "Error: no questions"
			}
			answers := make([]string, len(questions))
			var wg sync.WaitGroup
			for i, q := range questions {
				wg.Add(1)
				go func(i int, q string) {
					defer wg.Done()
					answers[i] = d.svc.agent.Run(q, agent.Options{Instructions: thinkInstructions(), Person: ctx.Person,
						Origin: ctx.Origin, Effort: "low",
						Exclude: []string{"think_harder", "start_background_task", "list_background_tasks", "research_parallel"}})
				}(i, q)
			}
			wg.Wait()
			var b strings.Builder
			for i, q := range questions {
				fmt.Fprintf(&b, "## %s\n%s\n\n", q, strings.TrimSpace(answers[i]))
			}
			log.Printf("[Background] research_parallel: %d questions", len(questions))
			return b.String()
		},
	})
}

func (bg *background) list(drop int) string {
	bg.mu.Lock()
	defer bg.mu.Unlock()
	if t := bg.running[drop]; t != nil {
		t.dropped = true
		return fmt.Sprintf("Task %d: its result will not be told.", drop)
	}
	if len(bg.running) == 0 {
		return "No task runs in the background."
	}
	var ids []int
	for id := range bg.running {
		ids = append(ids, id)
	}
	sort.Ints(ids)
	var b strings.Builder
	for _, id := range ids {
		t := bg.running[id]
		fmt.Fprintf(&b, "[%d] %s (for %d s)\n", id, textutil.Prefix(t.task, 120), int(time.Since(t.started).Seconds()))
	}
	return b.String()
}

func (d *device) startBackground(bg *background, args map[string]any, ctx tools.Context) string {
	task := strings.TrimSpace(tools.String(args, "task"))
	if task == "" {
		return "Error: the task is missing"
	}
	thorough, _ := args["thorough"].(bool)
	label := "speaker"
	if ctx.Conversation != nil {
		label = ctx.Conversation.Label()
	}
	person := ctx.Person
	if person == "" && label == "speaker" {
		person = d.svc.people.Admin() // nobody named at home: the admin, as for the agenda
	}
	bg.mu.Lock()
	if n := prompts.Int("BACKGROUND_MAX", 3); len(bg.running) >= n {
		bg.mu.Unlock()
		return fmt.Sprintf("Error: already %d tasks run in the background; wait for one or drop one (list_background_tasks).", n)
	}
	bg.next++
	t := &bgTask{id: bg.next, task: task, person: person, label: label, started: time.Now(), thorough: thorough}
	bg.running[t.id] = t
	bg.mu.Unlock()
	log.Printf("[Background] %d started (%s): %.100s", t.id, label, task)
	go d.runBackground(bg, t, ctx.Origin)
	return fmt.Sprintf("Started in the background as task %d. Tell the user you will come back to it; the result "+
		"comes by itself.", t.id)
}

func (d *device) runBackground(bg *background, t *bgTask, origin string) {
	effort := "low"
	if t.thorough {
		effort = "high"
	}
	opts := agent.Options{Instructions: thinkInstructions(), Person: t.person, Origin: origin, Effort: effort,
		Exclude: []string{"think_harder", "start_background_task", "list_background_tasks"}}
	out := d.svc.agent.Run(t.task, opts)
	// A careful task is checked before it is told: does the result answer it? If not, one more run
	// with what is missing (a cheap Decisions question).
	if t.thorough && strings.TrimSpace(out) != "" {
		if missing := checkResult(t.task, out); missing != "" {
			log.Printf("[Background] %d incomplete (%s): once more", t.id, missing)
			out = d.svc.agent.Run(t.task+"\n\nA first answer was incomplete ("+missing+"). It was:\n"+out+
				"\n\nGive the complete answer.", opts)
		}
	}
	bg.mu.Lock()
	dropped := t.dropped
	delete(bg.running, t.id)
	bg.mu.Unlock()
	log.Printf("[Background] %d done after %d s%s: %.200s", t.id, int(time.Since(t.started).Seconds()),
		map[bool]string{true: " (dropped)"}[dropped], out)
	if dropped || strings.TrimSpace(out) == "" {
		return
	}
	d.deliverBackground(t, out)
}

// checkResult: "" when the result answers the task, else what is missing in short. The backend
// model with little reasoning goes through each part the task asks for; the Decisions API let
// "no reliable forecast for the rest of the day" for one of three cities pass (25.10.2026). No
// answer: "" (told as it is).
func checkResult(task, result string) string {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	schema := map[string]any{"type": "object", "additionalProperties": false, "required": []string{"verdict", "missing"},
		"properties": map[string]any{
			"verdict": map[string]any{"type": "string", "enum": []string{"complete", "partial", "failed"}},
			"missing": map[string]any{"type": "string", "description": "what is missing, in short; '' when complete"}}}
	resp, err := openai.Responses(ctx, map[string]any{
		"model": prompts.Text("DELEGATION_MODEL"),
		"instructions": "You check a research result against its task. Go through every part the task asks for (each " +
			"place, item, period, question). partial: a part is missing, only partly answered, or the result says it " +
			"has no data for it; failed: nothing usable. complete only when every part is answered.",
		"input":     "# Task\n" + task + "\n\n# Result\n" + textutil.Prefix(result, 8000),
		"store":     false,
		"reasoning": map[string]any{"effort": "low"},
		"text": map[string]any{"format": map[string]any{"type": "json_schema", "name": "check", "strict": true,
			"schema": schema}},
	})
	if err != nil {
		log.Printf("[Background] check: %v", err)
		return ""
	}
	var out struct {
		Verdict, Missing string
	}
	if json.Unmarshal([]byte(resp.Text()), &out) != nil || out.Verdict == "complete" || out.Verdict == "" {
		return ""
	}
	if out.Missing == "" {
		return out.Verdict
	}
	return out.Verdict + ": " + out.Missing
}

var resumedAt atomic.Int64 // unix seconds of the last resume after the content filter

// resumeAfterFilter: GPT Live's moderation stopped an answer and closed the session mid-sentence
// (25.10.2026, on harmless shopping talk): a new session at once, in which the assistant says briefly
// that he was cut off and goes on. At most once in two minutes, so a real refusal does not loop.
func (d *device) resumeAfterFilter(e live.Ended) {
	now := time.Now().Unix()
	if last := resumedAt.Load(); now-last < 120 {
		log.Print("[Live] closed by the content filter again: not resumed")
		return
	}
	resumedAt.Store(now)
	log.Print("[Live] closed by the content filter: a new conversation")
	time.Sleep(time.Second)
	d.session.Start(live.Request{Reason: prompts.Text("LIVE_RESUME_AFTER_FILTER"), Person: e.Request.Person})
}

// deliverBackground: into the conversation it came from while that is live, else as a message.
func (d *device) deliverBackground(t *bgTask, out string) {
	ses := d.session
	if t.label == "phone" {
		ses = d.phoneSession
	}
	// Every result also goes to the person as a message (the owner's wish, 25.10.2026: research
	// results to keep); in a live conversation the assistant tells the gist. A conversation takes a note
	// of about 500 tokens only, so it gets the beginning (it failed and was lost once).
	sent := ""
	if t.person != "" {
		sent = d.svc.people.Message(t.person, "Ergebnis zu: "+textutil.Prefix(t.task, 200)+"\n\n"+out)
		log.Printf("[Background] %d sent: %s", t.id, sent)
	} else {
		log.Printf("[Background] %d: nobody to send the result to", t.id)
	}
	note := "The background task you started is done (task " + fmt.Sprint(t.id) + ": " + textutil.Prefix(t.task, 160) +
		"). Tell the user the gist now, briefly, at the next pause"
	if sent != "" && !strings.HasPrefix(sent, "Error") {
		note += ", and that the full result is in their messages (" + sent + ")"
	}
	note += ". Result:\n" + textutil.Prefix(out, 1000)
	if ses != nil && ses.State() == live.Live && ses.Notice(note, false) {
		log.Printf("[Background] %d told in the conversation", t.id)
	}
}
