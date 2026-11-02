// Package prefer: the household's preferences that were fixed numbers in code (#82, step 7: the
// hour of the mail digest, of the morning briefing, from when tomorrow is mentioned, what counts
// as urgent). The number stays the start value; the person's behaviour rules decide the value for
// today, read by one Decisions question ("am Wochenende kein Briefing vor 9" makes it 9 on
// Saturday). The answer is kept per person, preference, day and rules, so there is at most one
// question a day per preference, and another only when the rules change.
//
// Hot paths (a conversation's instructions) never wait for the network: Hour and Minutes give the
// kept answer, and when there is none they start the question in the background and give the start
// value meanwhile.
package prefer

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"log"
	"strconv"
	"sync"
	"time"

	"echo-assistant/internal/openai"
	"echo-assistant/internal/prompts"
)

// What a preference is about, for the question.
var about = map[string]string{
	"mail_digest": "at which hour the daily message with the less important mails (the mail digest) should come",
	"briefing":    "from which hour on the first conversation of the day starts with a short morning briefing",
	"evening":     "from which hour on the assistant also mentions tomorrow's appointments before a conversation ends",
	"urgent":      "how many minutes before an appointment a reminder counts as urgent (told even when hints are paused)",
	"learn_report": "at which hour on its day the weekly message comes with what the assistant learned about the person on " +
		"its own",
}

type answer struct {
	value int
	until time.Time // an error: asked again from then on
}

// Prefs answers the preferences. Rules gives a person's behaviour rules as text.
type Prefs struct {
	Rules func(person string) string
	Now   func() time.Time // time.Now; tests set it
	// Ask: the question (Decide); tests replace it.
	Ask func(input string, choices []string, unit string) (string, error)

	mu      sync.Mutex
	answers map[string]answer
	asking  map[string]bool
}

func New(rules func(person string) string) *Prefs {
	return &Prefs{Rules: rules, Now: time.Now, Ask: Decide, answers: map[string]answer{}, asking: map[string]bool{}}
}

// Hour: the hour (0-23) for `what` today; def as long as no rule says otherwise or there is no
// answer yet.
func (p *Prefs) Hour(person, what string, def int) int {
	choices := make([]string, 24)
	for h := range choices {
		choices[h] = strconv.Itoa(h)
	}
	return p.value(person, what, def, choices, "o'clock")
}

// Minutes: one of options for `what`; def without a rule or an answer.
func (p *Prefs) Minutes(person, what string, options []int, def int) int {
	choices := make([]string, len(options))
	for i, m := range options {
		choices[i] = strconv.Itoa(m)
	}
	return p.value(person, what, def, choices, "minutes")
}

func (p *Prefs) value(person, what string, def int, choices []string, unit string) int {
	if p == nil || p.Rules == nil {
		return def
	}
	rules := p.Rules(person)
	if rules == "" {
		return def // no rules: nothing to ask
	}
	now := p.Now()
	sum := sha256.Sum256([]byte(rules))
	key := fmt.Sprintf("%s|%s|%s|%x", person, what, now.Format("2006-01-02"), sum[:6])
	p.mu.Lock()
	a, known := p.answers[key]
	if known && (a.until.IsZero() || now.Before(a.until)) {
		p.mu.Unlock()
		if a.value < 0 {
			return def
		}
		return a.value
	}
	if p.asking[key] {
		p.mu.Unlock()
		return def
	}
	p.asking[key] = true
	p.mu.Unlock()
	input := Input(what, def, unit, rules, now)
	go func() {
		choice, err := p.Ask(input, append([]string{"default"}, choices...), unit)
		a := answer{value: -1}
		switch {
		case err != nil:
			a.until = p.Now().Add(10 * time.Minute)
			log.Printf("[Prefer] %s for %q: %v", what, person, err)
		case choice != "default":
			if v, err := strconv.Atoi(choice); err == nil {
				a.value = v
			}
		}
		if a.value >= 0 && a.value != def {
			log.Printf("[Prefer] %s for %q today: %d %s (start value %d)", what, person, a.value, unit, def)
		}
		p.mu.Lock()
		p.answers[key] = a
		delete(p.asking, key)
		p.mu.Unlock()
	}()
	return def
}

// Input: what the question reads: the rules first, then the question with today's weekday.
func Input(what string, def int, unit, rules string, now time.Time) string {
	kind := "a working day (Monday to Friday)"
	if wd := now.Weekday(); wd == time.Saturday || wd == time.Sunday {
		kind = "the weekend"
	}
	return fmt.Sprintf("%s\n\n# Question\nToday is %s, %s. Preference: %s. Does one of the rules above name a value for "+
		"exactly this preference today (a rule for the weekend holds on Saturday and Sunday only, one for working days "+
		"on Monday to Friday only)? Then answer that value; when several do, the one that fits today most exactly. "+
		"A rule about something else (e.g. a wake-up call) does not count. Otherwise answer default (the start value "+
		"%d %s).", rules, now.Format("Monday, 2006-01-02"), kind, about[what], def, unit)
}

// Decide: the question to the backend model with little reasoning and a fixed answer (an enum).
// The Decisions API picked 7 for "am Wochenende kein Briefing vor 9" on a Sunday, twice
// (25.10.2026): reading a number out of a rule for a weekday is not a classification. At most one
// question a day per preference, so the cost does not matter.
func Decide(input string, choices []string, unit string) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	schema := map[string]any{"type": "object", "additionalProperties": false, "required": []string{"value"},
		"properties": map[string]any{"value": map[string]any{"type": "string", "enum": choices,
			"description": "the value in " + unit + ", or default"}}}
	resp, err := openai.Responses(ctx, map[string]any{
		"model":        prompts.Text("DELEGATION_MODEL"),
		"instructions": "You read the behaviour rules the household gave its voice assistant the assistant and answer one question about a preference.",
		"input":        input,
		"store":        false,
		"reasoning":    map[string]any{"effort": "low"},
		"text": map[string]any{"format": map[string]any{"type": "json_schema", "name": "preference", "strict": true,
			"schema": schema}},
	})
	if err != nil {
		return "", err
	}
	var out struct {
		Value string `json:"value"`
	}
	if err := json.Unmarshal([]byte(resp.Text()), &out); err != nil || out.Value == "" {
		return "", fmt.Errorf("answer not readable")
	}
	return out.Value, nil
}
