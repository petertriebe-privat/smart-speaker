package initiative

import (
	"context"
	"fmt"
	"log"
	"strings"
	"time"

	"echo-assistant/internal/openai"
	"echo-assistant/internal/prompts"
)

// Step 2: each new candidate is weighed with the Decisions API (DECISIONS_MODEL): worth it, when,
// at which level, from the candidate, the user's rules, the tally of their reactions to the kind
// and the situation. Only input is paid and an answer takes ~0.3 s, so no large model looks at
// what is not worth it. No answer: the suggestion stays dry and is weighed again next pass.

// When a suggestion is told; Levels what the assistant may do with it (#82's levels 0-3).
var (
	Whens  = []string{"now", "next_conversation", "briefing", "message", "later", "never"}
	Levels = []string{"know", "suggest", "act", "approve"}
)

// Verdict: what the weighing made of a candidate.
type Verdict struct {
	Worth bool
	When  string
	Level string
	Sure  float64 // the confidence of worth, 0..1
}

// Weigher asks for a verdict on input (the candidate with its context).
type Weigher func(input string) (Verdict, error)

// worthSure: below this confidence a worthwhile suggestion does not interrupt (now, message):
// it waits for the next conversation.
const worthSure = 0.6

// Input: what the weighing reads.
func Input(s Suggestion, situation string, tally map[string]int) string {
	var b strings.Builder
	fmt.Fprintf(&b, "# Candidate\nkind: %s\n%s\n", s.Kind, s.Text)
	fmt.Fprintf(&b, "\n# The user's reactions to earlier suggestions of this kind\naccepted %d, declined %d, ignored %d\n",
		tally["accepted"], tally["declined"], tally["ignored"])
	if situation = strings.TrimSpace(situation); situation != "" {
		b.WriteString("\n" + situation + "\n")
	}
	return b.String()
}

// apply: the verdict as the suggestion's state, with the confidence rule.
func apply(v Verdict) (state, when string) {
	if v.When == "later" { // too early (a reply may still come): weighed again (dry) tomorrow
		return "later", v.When
	}
	if !v.Worth || v.When == "never" {
		return "dropped", v.When
	}
	when = v.When
	if v.Sure < worthSure && (when == "now" || when == "message") {
		when = "next_conversation"
	}
	return "pending", when
}

func choices(values []string, descriptions map[string]string) []map[string]string {
	out := make([]map[string]string, len(values))
	for i, v := range values {
		out[i] = map[string]string{"value": v, "description": descriptions[v]}
	}
	return out
}

// Decide: the Weigher over the Decisions API.
func Decide(text string) (Verdict, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()
	var resp struct {
		Answers []struct {
			Type       string  `json:"type"`
			Name       string  `json:"name"`
			Choice     string  `json:"choice"`
			Confidence float64 `json:"confidence"`
		} `json:"answers"`
	}
	started := time.Now()
	err := openai.Post(ctx, "/v1/decisions", map[string]any{
		"model": prompts.TextOr("DECISIONS_MODEL", "gpt-6-luna"),
		"input": text,
		"questions": []map[string]any{
			{"type": "choice", "name": "worth", "instructions": prompts.Text("INITIATIVE_WORTH"),
				"choices": choices([]string{"yes", "no"}, map[string]string{"yes": "it helps the user", "no": "not worth it"})},
			{"type": "choice", "name": "when", "instructions": prompts.Text("INITIATIVE_WHEN"),
				"choices": choices(Whens, map[string]string{
					"now":               "urgent: say it at once at home",
					"next_conversation": "in the next conversation with the user",
					"briefing":          "in the next morning briefing",
					"message":           "as a message to the user's phone (they are out, or it cannot wait)",
					"later":             "not yet: look at it again tomorrow",
					"never":             "not at all"})},
			{"type": "choice", "name": "level", "instructions": prompts.Text("INITIATIVE_LEVEL"),
				"choices": choices(Levels, map[string]string{
					"know":    "only mention it",
					"suggest": "offer to do something and wait for a yes",
					"act":     "do something reversible and say so",
					"approve": "something irreversible: show it and wait for approval"})},
		},
	}, &resp)
	if err != nil {
		return Verdict{}, err
	}
	v := Verdict{Level: "know"}
	got := 0
	for _, a := range resp.Answers {
		if a.Type != "choice" {
			continue
		}
		switch a.Name {
		case "worth":
			v.Worth, v.Sure = a.Choice == "yes", a.Confidence
			got++
		case "when":
			v.When = a.Choice
			got++
		case "level":
			v.Level = a.Choice
		}
	}
	if got < 2 {
		return Verdict{}, fmt.Errorf("incomplete answer (%d answers)", len(resp.Answers))
	}
	log.Printf("[Initiative] weighed in %d ms", time.Since(started).Milliseconds())
	return v, nil
}

// weigh: the dry suggestions get a verdict (at most INITIATIVE_WEIGH_MAX a pass).
func (p *Pass) weigh() {
	if p.Weigh == nil {
		return
	}
	limit := prompts.Int("INITIATIVE_WEIGH_MAX", 5)
	p.Journal.Reconsider(time.Duration(prompts.Int("INITIATIVE_RECONSIDER_H", 24)) * time.Hour)
	for _, s := range p.Journal.InState("dry") {
		if limit == 0 {
			return
		}
		limit--
		situation := ""
		if p.Situation != nil {
			situation = p.Situation(s.Person)
		}
		v, err := p.Weigh(Input(s, situation, p.Journal.Tally(s.Person, s.Kind)))
		if err != nil {
			log.Printf("[Initiative] weighing %s: %v", s.ID, err)
			return // the API is not there: the next pass
		}
		state, when := apply(v)
		reason := fmt.Sprintf("when %s, level %s, sure %.2f", when, v.Level, v.Sure)
		p.Journal.Verdict(s.ID, state, when, v.Level, reason, deliverAt(s, state, when, p.Journal.Now()))
		log.Printf("[Initiative] %s %s: %s, %s", s.ID, s.Kind, state, reason)
	}
}
