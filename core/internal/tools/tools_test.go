package tools

import (
	"errors"
	"strings"
	"testing"
)

// Several Echos: tools of the shared state run on the primary device; when it is not reachable,
// the ones that change something fail and the ones that only read run here.
func TestRemoteTools(t *testing.T) {
	var r Registry
	local := map[string]int{}
	for _, name := range []string{"remember", "recall", "weather"} {
		r.Add(Tool{Name: name, Parameters: `{"type":"object"}`, Run: func(map[string]any, Context) string {
			local[name]++
			return "local " + name
		}})
	}
	up := true
	var sent []string
	r.SetRemote(func(name, arguments string, ctx Context) (string, error) {
		if !up {
			return "", errors.New("no primary device reachable")
		}
		sent = append(sent, name+" "+arguments+" "+ctx.Person)
		return "remote " + name, nil
	}, []string{"remember"}, []string{"recall"})

	if got := r.Call("remember", `{"text":"x"}`, Context{Person: "p1"}); got != "remote remember" {
		t.Errorf("remember: %q", got)
	}
	if len(sent) != 1 || sent[0] != `remember {"text":"x"} p1` {
		t.Errorf("sent %v", sent)
	}
	if got := r.Call("weather", "", Context{}); got != "local weather" {
		t.Errorf("a tool of no shared state: %q", got)
	}
	up = false
	if got := r.Call("remember", `{}`, Context{}); !strings.Contains(got, "not reachable") || local["remember"] != 0 {
		t.Errorf("remember with the primary away: %q, ran here %d times", got, local["remember"])
	}
	if got := r.Call("recall", `{}`, Context{}); got != "local recall" {
		t.Errorf("recall with the primary away: %q", got)
	}
}

// Observe hears the calls that ran here and worked: not failures, not calls run elsewhere.
func TestObserve(t *testing.T) {
	var r Registry
	var heard []string
	r.Observe = func(name, arguments string, _ Context, out string) { heard = append(heard, name+" "+arguments+" "+out) }
	add := func(name, out string) {
		r.Add(Tool{Name: name, Parameters: `{"type":"object"}`, Channels: Everywhere,
			Run: func(map[string]any, Context) string { return out }})
	}
	add("play", "playing")
	add("broken", "Error: no")
	add("shared", "local")
	r.SetRemote(func(string, string, Context) (string, error) { return "remote", nil }, []string{"shared"}, nil)
	r.Call("play", `{"q":"Jazz"}`, Context{})
	r.Call("broken", `{}`, Context{})
	r.Call("shared", `{}`, Context{})
	r.Call("missing", `{}`, Context{})
	if len(heard) != 1 || heard[0] != `play {"q":"Jazz"} playing` {
		t.Errorf("heard %q", heard)
	}
}
