package live

import (
	"context"
	"log"
	"strings"
	"sync"
	"time"

	"echo-assistant/internal/openai"
	"echo-assistant/internal/prompts"
	"echo-assistant/internal/transcript"
)

// endCheck asks the backend model whether the conversation is over, as
// devices/atoms3r/src/voice/conversation_end.cpp: after the assistant spoke and it stays quiet.
type endCheck struct {
	mu       sync.Mutex
	busy     bool
	answered time.Time // lastActivity stamp of the answer
	over_    bool
}

func (e *endCheck) reset() {
	e.mu.Lock()
	e.answered, e.over_ = time.Time{}, false
	e.mu.Unlock()
}

func (e *endCheck) over(stamp time.Time) bool {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.over_ && e.answered.Equal(stamp)
}

func (e *endCheck) check(full string, stamp time.Time) {
	e.mu.Lock()
	if e.busy {
		e.mu.Unlock()
		return
	}
	e.busy = true
	e.mu.Unlock()
	go e.judge(transcript.Tail(full, 800), stamp)
}

func (e *endCheck) judge(text string, stamp time.Time) {
	over := false
	text = strings.TrimSpace(text)
	started := time.Now()
	// Nothing transcribed yet: the API rejects an empty input, and nothing said is no goodbye.
	if text == "" {
		log.Print("[End?] no (nothing said yet)")
	} else if decided, ok := endDecision(text); ok {
		over = decided
		log.Printf("[End?] %s (decisions, %d ms)", map[bool]string{true: "yes", false: "no"}[over], time.Since(started).Milliseconds())
	} else {
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		resp, err := openai.Responses(ctx, map[string]any{
			"model":        prompts.Text("DELEGATION_MODEL"),
			"instructions": prompts.Text("END_CHECK_INSTRUCTIONS"),
			"input":        text,
			"store":        false,
			// "none" answered at random on a real goodbye; "low" was right 8 of 8 on the M5.
			"max_output_tokens": 200,
			"reasoning":         map[string]any{"effort": "low"},
		})
		cancel()
		if err != nil {
			log.Printf("[End?] error (%v, %d ms)", err, time.Since(started).Milliseconds())
		} else {
			over = strings.Contains(strings.ToUpper(resp.Text()), "YES")
			answer := "no"
			if over {
				answer = "yes"
			}
			log.Printf("[End?] %s (%d ms)", answer, time.Since(started).Milliseconds())
		}
	}
	e.mu.Lock()
	e.answered, e.over_, e.busy = stamp, over, false
	e.mu.Unlock()
}

// endDecision asks the Decisions API (as the quick commands' intent): 0.2-0.9 s and 10 of 10
// right on test endings, where Responses with reasoning took 3.5-4.6 s on the Echo and the
// hang-up waited for it (19.10.2026). ok false: no answer, Responses decides.
func endDecision(text string) (over, ok bool) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	var resp struct {
		Answers []struct {
			Type   string `json:"type"`
			Choice string `json:"choice"`
		} `json:"answers"`
	}
	err := openai.Post(ctx, "/v1/decisions", map[string]any{
		"model": prompts.TextOr("QUICK_INTENT_MODEL", "gpt-6-luna"),
		"input": text,
		"questions": []map[string]any{{"type": "choice", "name": "over",
			"instructions": prompts.Text("END_CHECK_INSTRUCTIONS"),
			// "the conversation is over" alone hung up after a task was done or a calendar was shown,
			// when the user wanted to ask about it (30.10.2026).
			"choices": []map[string]string{{"value": "yes", "description": "the person said goodbye, closed with thanks or asked to stop"},
				{"value": "no", "description": "anything else, also right after a finished task, an answer or something shown"}}}},
	}, &resp)
	if err != nil || len(resp.Answers) == 0 || resp.Answers[0].Type != "choice" {
		log.Printf("[End?] decisions: no answer (%v)", err)
		return false, false
	}
	return resp.Answers[0].Choice == "yes", true
}
