package main

import (
	"bufio"
	"encoding/json"
	"net"
	"strings"
	"testing"

	"echo-assistant/internal/tools"
)

// A tool call goes to the app as a line and comes back as its result.
func TestPhoneToolRoundTrip(t *testing.T) {
	if got := phoneToolCall("phone_media", nil); !strings.HasPrefix(got, "Error") {
		t.Fatalf("without the app: %q", got)
	}
	core, app := net.Pipe()
	defer core.Close()
	defer app.Close()
	p := &phoneConn{conn: core, cards: make(chan []byte, 1)}
	phoneLink.Store(p)
	defer phoneLink.Store(nil)
	go func() {
		line, _ := bufio.NewReader(app).ReadString('\n')
		parts := strings.SplitN(strings.TrimSpace(line), " ", 3)
		var call struct {
			Name string         `json:"name"`
			Args map[string]any `json:"args"`
		}
		json.Unmarshal([]byte(parts[2]), &call)
		text, _ := json.Marshal("paused " + call.Name + " " + call.Args["action"].(string) + "\nline two")
		phoneToolResult("result " + parts[1] + " " + string(text))
	}()
	if got := phoneToolCall("phone_media", map[string]any{"action": "pause"}); got != "paused phone_media pause\nline two" {
		t.Fatalf("result %q", got)
	}
}

// The phone's calls: dial_for_me (the user talks, from the phone; the app's phone_call) and
// phone_call (the assistant with a task, over the landline at home: on the primary), the same
// names as at the speakers at home.
func TestPhoneCallTools(t *testing.T) {
	d := &device{registry: &tools.Registry{}}
	d.addPhoneTools()
	names := "," + strings.Join(d.registry.Names(), ",") + ","
	if !strings.Contains(names, ",dial_for_me,") || !strings.Contains(names, ",phone_call,") {
		t.Fatalf("tools %s", names)
	}
	if got := d.registry.Call("phone_call", `{"to":"07161 123456","task":"einen Tisch reservieren","listen":true}`,
		tools.Context{Channel: tools.Voice}); !strings.Contains(got, "not reachable") {
		t.Errorf("without the primary: %q", got)
	}
	core, app := net.Pipe()
	defer core.Close()
	defer app.Close()
	phoneLink.Store(&phoneConn{conn: core, cards: make(chan []byte, 1)})
	defer phoneLink.Store(nil)
	got := make(chan string, 1)
	go func() {
		line, _ := bufio.NewReader(app).ReadString('\n')
		parts := strings.SplitN(strings.TrimSpace(line), " ", 3)
		got <- parts[2]
		text, _ := json.Marshal("Calling Anna")
		phoneToolResult("result " + parts[1] + " " + string(text))
	}()
	if out := d.registry.Call("dial_for_me", `{"to":"Anna"}`, tools.Context{Channel: tools.Voice}); out != "Calling Anna" {
		t.Errorf("dial_for_me: %q", out)
	}
	if call := <-got; call != `{"args":{"who":"Anna"},"name":"phone_call"}` {
		t.Errorf("to the app: %s", call)
	}
}
