// Package tools: the functions the backend model may call, as devices/atoms3r/src/assistant/tools.cpp
// registers them on the M5: every tool is one entry with name, description and a JSON schema,
// plus where it may be used (channels). Tools known only at run time (Home Assistant's via MCP)
// come from providers with the same shape.
package tools

import (
	"encoding/json"
	"fmt"
	"strings"
	"sync"
)

// Channel: where a call comes from; a tool is offered and runs only in its channels.
type Channel uint8

const (
	Voice    Channel = 1 << iota // voice conversation (GPT Live's backend)
	Agent                        // agent runs: chat messages, mail commands, scheduled tasks
	Console                      // jarvis ctl tool ... (tests)
	Stranger                     // a phone call to someone else: only what that call needs
)

const Everywhere = Voice | Agent | Console

type Context struct {
	Channel Channel
	Person  string // who asked (people id), "" = unknown (at home, nobody named)
	Origin  string // "Lautsprecher", "Anruf", "Telegram", "E-Mail", ...
	// Room: the speaker's room for a conversation held there ("" = elsewhere): a reminder ordered
	// there to be said aloud is said there (#65).
	Room string
	// Conversation: the voice conversation the call comes from (nil outside one). Several run at
	// once (speaker and phone): tools like end_call act on their own.
	Conversation Conversation
	// Record, if set, takes the call instead of the tool (the evaluation: what the model would
	// call, after the permission checks, without doing it) and returns the tool's stand-in output.
	Record func(name, arguments string) string
}

// Conversation: what tools may do with the conversation they were called from.
type Conversation interface {
	EndSoon()      // hang up once the assistant is quiet (end_call)
	Label() string // "speaker" or "phone"
}

type Tool struct {
	Name        string
	Description string
	Parameters  string // JSON schema of the arguments (an object)
	Hosted      bool   // run by OpenAI (web_search): offered as {"type": name}, never called here
	Private     bool   // the admin's own accounts and the assistant's administration: not for other people
	Channels    Channel
	Run         func(args map[string]any, ctx Context) string
}

// Provider: tools known only at run time (Home Assistant, MCP servers).
type Provider struct {
	Name     string
	Describe func(ctx Context) []any // Responses function definitions
	Has      func(name string) bool
	Call     func(name string, args map[string]any, ctx Context) string
	Channels Channel
}

// Denied, if set, says why ctx.Person may not use a tool ("" = allowed); the people register
// fills it in.
var Denied func(person, tool string, private bool) string

type entry struct {
	tool     *Tool
	provider *Provider
}

type Registry struct {
	// Observe, if set, hears every call that ran here and did not fail (after it returned): for
	// the behaviour log (memory/behaviour.go). Not for calls run on another device or recorded.
	Observe func(name, arguments string, ctx Context, out string)

	mu      sync.Mutex
	entries []entry
	rare    map[string]bool
	remote  map[string]bool // run by remoteRun (SetRemote); true = may run here when that fails
	// remoteRun: another device runs the tool (several Echos: the primary one owns memory, tasks
	// and people); an error when it is not reachable.
	remoteRun func(name, arguments string, ctx Context) (string, error)
}

// SetRemote: these tools run on another device through run. readOnly of them only read: when
// that device is not reachable they run here on the local copy; the others then fail, so no
// second state grows here.
func (r *Registry) SetRemote(run func(name, arguments string, ctx Context) (string, error), names, readOnly []string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.remoteRun = run
	r.remote = map[string]bool{}
	for _, n := range names {
		r.remote[n] = false
	}
	for _, n := range readOnly {
		r.remote[n] = true
	}
}

// SetRare names tools a voice conversation's backend is not offered: administration and other
// things rarely asked for by voice. Every request carries the whole tool list (~22000 tokens for
// 126 tools, ctl eval 18.10.2026); those tools stay reachable there through think_harder, whose
// run has them all.
func (r *Registry) SetRare(names ...string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.rare == nil {
		r.rare = map[string]bool{}
	}
	for _, n := range names {
		r.rare[n] = true
	}
}

func (r *Registry) Add(t Tool) {
	if t.Channels == 0 {
		t.Channels = Everywhere
	}
	if !t.Hosted {
		var schema any
		if err := json.Unmarshal([]byte(t.Parameters), &schema); err != nil {
			panic(fmt.Sprintf("tools: schema of %s: %v", t.Name, err))
		}
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.entries = append(r.entries, entry{tool: &t})
}

func (r *Registry) AddProvider(p Provider) {
	if p.Channels == 0 {
		p.Channels = Everywhere
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.entries = append(r.entries, entry{provider: &p})
}

func denied(ctx Context, name string, private bool) string {
	if Denied == nil || ctx.Person == "" {
		return ""
	}
	return Denied(ctx.Person, name, private)
}

// Describe gives the tool list for the Responses API, without those ctx.Person may not use.
func (r *Registry) Describe(ctx Context) []any {
	if ctx.Channel == 0 {
		ctx.Channel = Voice
	}
	r.mu.Lock()
	entries := append([]entry(nil), r.entries...)
	rare := r.rare
	r.mu.Unlock()
	skip := func(name string) bool { return ctx.Channel == Voice && rare[name] }
	list := make([]any, 0, len(entries))
	for _, e := range entries {
		if p := e.provider; p != nil {
			if p.Channels&ctx.Channel != 0 {
				for _, f := range p.Describe(ctx) {
					if m, ok := f.(map[string]any); !ok || !skip(fmt.Sprint(m["name"])) {
						list = append(list, f)
					}
				}
			}
			continue
		}
		t := e.tool
		if t.Channels&ctx.Channel == 0 || denied(ctx, t.Name, t.Private) != "" || skip(t.Name) {
			continue
		}
		if t.Hosted {
			list = append(list, map[string]any{"type": t.Name})
			continue
		}
		var schema any
		json.Unmarshal([]byte(t.Parameters), &schema)
		list = append(list, map[string]any{"type": "function", "name": t.Name,
			"description": t.Description, "parameters": schema})
	}
	return list
}

// Names of all registered fixed tools (status output).
func (r *Registry) Names() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	var names []string
	for _, e := range r.entries {
		if e.tool != nil {
			names = append(names, e.tool.Name)
		} else {
			names = append(names, e.provider.Name+"/*")
		}
	}
	return names
}

// Call runs a tool with its JSON arguments and returns its output for the model.
func (r *Registry) Call(name, arguments string, ctx Context) string {
	out, ran := r.call(name, arguments, ctx)
	if ran && r.Observe != nil && !strings.HasPrefix(out, "Error") {
		r.Observe(name, arguments, ctx, out)
	}
	return out
}

// call: the output and whether the tool ran here.
func (r *Registry) call(name, arguments string, ctx Context) (string, bool) {
	if ctx.Channel == 0 {
		ctx.Channel = Voice
	}
	args := map[string]any{}
	if strings.TrimSpace(arguments) != "" {
		if err := json.Unmarshal([]byte(arguments), &args); err != nil {
			return "Error: arguments are not valid JSON", false
		}
	}
	r.mu.Lock()
	entries := append([]entry(nil), r.entries...)
	readOnly, remote := r.remote[name]
	remoteRun := r.remoteRun
	r.mu.Unlock()
	for _, e := range entries {
		if t := e.tool; t != nil {
			if t.Name != name {
				continue
			}
			if t.Channels&ctx.Channel == 0 || t.Hosted {
				break
			}
			if refused := denied(ctx, name, t.Private); refused != "" {
				return refused, false
			}
			if ctx.Record != nil {
				return ctx.Record(name, arguments), false
			}
			if remote && remoteRun != nil {
				out, err := remoteRun(name, arguments, ctx)
				if err == nil {
					return out, false // ran there; observed there
				}
				if !readOnly {
					return "Error: the main assistant device, which keeps memory, tasks and people, is not reachable (" +
						err.Error() + "). Nothing was saved. Tell the user and offer to try again later.", false
				}
			}
			return t.Run(args, ctx), true
		}
		p := e.provider
		if !p.Has(name) {
			continue
		}
		if p.Channels&ctx.Channel == 0 {
			break
		}
		if refused := denied(ctx, name, false); refused != "" {
			return refused, false
		}
		if ctx.Record != nil {
			return ctx.Record(name, arguments), false
		}
		return p.Call(name, args, ctx), true
	}
	return "Error: unknown function", false
}

// Int reads an integer argument (JSON numbers are float64).
func Int(args map[string]any, name string) (int, bool) {
	v, ok := args[name].(float64)
	return int(v), ok
}

// String reads a string argument (trimmed).
func String(args map[string]any, name string) string {
	v, _ := args[name].(string)
	return strings.TrimSpace(v)
}

// Bool reads a boolean argument with a default.
func Bool(args map[string]any, name string, def bool) bool {
	if v, ok := args[name].(bool); ok {
		return v
	}
	return def
}

// Function builds a Responses function definition (for providers).
func Function(name, description string, parameters json.RawMessage) map[string]any {
	return map[string]any{"type": "function", "name": name, "description": description,
		"parameters": parameters}
}
