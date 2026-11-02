package main

import (
	"bufio"
	"encoding/json"
	"net"
	"strings"
	"testing"
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
