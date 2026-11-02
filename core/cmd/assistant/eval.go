package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"sort"
	"strings"
	"sync"
	"time"

	"echo-assistant/internal/agent"
	"echo-assistant/internal/openai"
	"echo-assistant/internal/prompts"
	"echo-assistant/internal/tools"
)

// The evaluation ("a agent tests the agent", ctl eval): example requests go to the backend model
// with the device's real instructions, tool list and permissions, but no tool runs: every call is
// recorded and answered with a stand-in (the case's stub, else "OK."). Each case runs several
// times, because the model is not deterministic; the report gives a pass rate per case and the
// tokens, so a prompt change can be measured before and after. Checks are exact where they can
// be (which tools, which arguments) and a judge question to the Decisions API where they cannot
// ("asks before it creates the event"). Cases: tests/eval/cases.json; they cost API money.

type evalCase struct {
	ID      string            `json:"id"`
	Input   string            `json:"input"`
	Channel string            `json:"channel"` // voice (default) or agent
	Stubs   map[string]string `json:"stubs"`   // tool -> its stand-in output
	Context string            `json:"context"` // added to the instructions (a made-up agenda, #71)
	Expect  struct {
		Any   []string                  `json:"any"`   // at least one of them called
		All   []string                  `json:"all"`   // all of them called
		None  []string                  `json:"none"`  // none of them called
		First []string                  `json:"first"` // the first call is one of them
		Args  map[string]map[string]any `json:"args"`  // tool -> argument -> expected (strings: contained, any case)
		Calls *int                      `json:"calls"` // exactly this many calls (0: answers or asks back)
		Judge string                    `json:"judge"` // a yes/no question about the run, for the Decisions API
	} `json:"expect"`
}

type evalCall struct{ Name, Arguments string }

type evalRun struct {
	Calls  []evalCall `json:"calls"`
	Answer string     `json:"answer"`
	Failed []string   `json:"failed"`
	Tokens int        `json:"input_tokens"`
	Cached int        `json:"cached_tokens"`
	Ms     int64      `json:"ms"`
}

type evalResult struct {
	ID     string    `json:"id"`
	Passed int       `json:"passed"`
	Runs   []evalRun `json:"runs"`
}

var evalRunning sync.Mutex

// startEval: ctl eval <cases.json> [runs] [id prefix]; the report goes next to the cases file.
func (d *device) startEval(arg string) string {
	fields := strings.Fields(arg)
	if len(fields) == 0 {
		return "error: eval <cases.json> [runs] [id prefix]"
	}
	data, err := os.ReadFile(fields[0])
	if err != nil {
		return "error: " + err.Error()
	}
	var cases []evalCase
	if err := json.Unmarshal(data, &cases); err != nil {
		return "error: cases: " + err.Error()
	}
	runs := 2
	if len(fields) > 1 {
		fmt.Sscan(fields[1], &runs)
	}
	if len(fields) > 2 {
		kept := cases[:0]
		for _, c := range cases {
			if strings.HasPrefix(c.ID, fields[2]) {
				kept = append(kept, c)
			}
		}
		cases = kept
	}
	if !evalRunning.TryLock() {
		return "error: an evaluation is running"
	}
	report := strings.TrimSuffix(fields[0], ".json") + "-report.json"
	go func() {
		defer evalRunning.Unlock()
		d.runEval(cases, runs, report)
	}()
	return fmt.Sprintf("eval started: %d cases x %d runs -> %s", len(cases), runs, report)
}

func (d *device) runEval(cases []evalCase, runs int, report string) {
	started := time.Now()
	var results []evalResult
	passed, total, tokens := 0, 0, 0
	for _, c := range cases {
		r := evalResult{ID: c.ID}
		for i := 0; i < runs; i++ {
			run := d.evalOnce(c)
			if len(run.Failed) == 0 {
				r.Passed++
			}
			tokens += run.Tokens
			r.Runs = append(r.Runs, run)
		}
		passed, total = passed+r.Passed, total+runs
		log.Printf("[Eval] %s: %d/%d %v", c.ID, r.Passed, runs, r.Runs[len(r.Runs)-1].Failed)
		results = append(results, r)
		d.writeReport(report, results, passed, total, tokens, started)
	}
	log.Printf("[Eval] done: %d/%d passed, %d input tokens, %s", passed, total, tokens, time.Since(started).Round(time.Second))
}

func (d *device) writeReport(path string, results []evalResult, passed, total, tokens int, started time.Time) {
	// what every request carries: the tool list and the fixed instructions (~4 bytes a token)
	list, _ := json.Marshal(d.registry.Describe(tools.Context{Channel: tools.Voice}))
	data, _ := json.MarshalIndent(map[string]any{"passed": passed, "total": total, "input_tokens": tokens,
		"tools": len(d.registry.Describe(tools.Context{Channel: tools.Voice})), "tool_list_bytes": len(list),
		"delegation_instructions_bytes": len(prompts.Text("DELEGATION_INSTRUCTIONS")),
		"seconds":                       int(time.Since(started).Seconds()), "model": prompts.Text("DELEGATION_MODEL"), "cases": results}, "", " ")
	os.WriteFile(path, data, 0o600)
}

func (d *device) evalOnce(c evalCase) evalRun {
	var run evalRun
	var mu sync.Mutex
	record := func(name, arguments string) string {
		mu.Lock()
		run.Calls = append(run.Calls, evalCall{name, arguments})
		mu.Unlock()
		if out, ok := c.Stubs[name]; ok {
			return out
		}
		return "OK."
	}
	o := agent.Options{Instructions: prompts.Text("DELEGATION_INSTRUCTIONS"), Origin: "Lautsprecher", Effort: "low",
		Channel: tools.Voice, Record: record, Usage: &agent.Usage{}}
	if c.Channel == "agent" {
		o.Instructions, o.Origin, o.Effort, o.Channel = prompts.Text("AGENT_INSTRUCTIONS"), "Telegram", "", tools.Agent
	}
	if c.Context != "" {
		o.Instructions += "\n" + c.Context
	}
	t := time.Now()
	run.Answer = d.svc.agent.Run(c.Input, o)
	run.Ms = time.Since(t).Milliseconds()
	run.Tokens, run.Cached = o.Usage.Input, o.Usage.Cached
	run.Failed = checkRun(c, run)
	return run
}

// checkRun: what of the case's expectations the run missed (nil = passed).
func checkRun(c evalCase, run evalRun) []string {
	var failed []string
	called := map[string][]string{}
	var order []string
	for _, call := range run.Calls {
		called[call.Name] = append(called[call.Name], call.Arguments)
		order = append(order, call.Name)
	}
	e := c.Expect
	if len(e.Any) > 0 && !anyOf(called, e.Any) {
		failed = append(failed, "none of "+strings.Join(e.Any, "/")+" called")
	}
	for _, name := range e.All {
		if !anyOf(called, []string{name}) {
			failed = append(failed, name+" not called")
		}
	}
	for _, name := range e.None {
		if anyOf(called, []string{name}) {
			failed = append(failed, name+" called")
		}
	}
	if len(e.First) > 0 && (len(order) == 0 || !contains(e.First, order[0])) {
		failed = append(failed, "first call not "+strings.Join(e.First, "/"))
	}
	if e.Calls != nil && len(run.Calls) != *e.Calls {
		failed = append(failed, fmt.Sprintf("%d calls, expected %d", len(run.Calls), *e.Calls))
	}
	names := make([]string, 0, len(e.Args))
	for name := range e.Args {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		if !argsMatch(called[name], e.Args[name]) {
			failed = append(failed, name+" without the expected arguments")
		}
	}
	if e.Judge != "" && !judge(e.Judge, c.Input, run) {
		failed = append(failed, "judge: "+e.Judge)
	}
	return failed
}

// matches: a name, or a prefix ending in * ("Hass*": Home Assistant's tools).
func matches(pattern, name string) bool {
	if prefix, ok := strings.CutSuffix(pattern, "*"); ok {
		return strings.HasPrefix(name, prefix)
	}
	return pattern == name
}

func anyOf(called map[string][]string, patterns []string) bool {
	for name := range called {
		if contains(patterns, name) {
			return true
		}
	}
	return false
}

func contains(patterns []string, name string) bool {
	for _, p := range patterns {
		if matches(p, name) {
			return true
		}
	}
	return false
}

// argsMatch: one of the calls has every expected argument (strings contained, any case).
func argsMatch(calls []string, want map[string]any) bool {
	for _, raw := range calls {
		var got map[string]any
		if json.Unmarshal([]byte(raw), &got) != nil {
			continue
		}
		ok := true
		for k, v := range want {
			g, present := got[k]
			if !present {
				ok = false
				break
			}
			if ws, isString := v.(string); isString {
				ok = ok && strings.Contains(strings.ToLower(fmt.Sprint(g)), strings.ToLower(ws))
			} else {
				ok = ok && fmt.Sprint(g) == fmt.Sprint(v)
			}
		}
		if ok {
			return true
		}
	}
	return false
}

// judge asks the Decisions API a yes/no question about the run.
func judge(question, input string, run evalRun) bool {
	var b strings.Builder
	fmt.Fprintf(&b, "Request to a voice assistant: %s\nTool calls it made:\n", input)
	for _, c := range run.Calls {
		fmt.Fprintf(&b, "- %s %s\n", c.Name, c.Arguments)
	}
	fmt.Fprintf(&b, "Its final answer: %s", run.Answer)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	var resp struct {
		Answers []struct {
			Probability float64 `json:"probability"`
		} `json:"answers"`
	}
	err := openai.Post(ctx, "/v1/decisions", map[string]any{"model": "gpt-6-luna", "input": b.String(),
		"questions": []map[string]any{{"type": "predicate", "name": "ok", "instructions": question}}}, &resp)
	return err == nil && len(resp.Answers) == 1 && resp.Answers[0].Probability >= 0.5
}
