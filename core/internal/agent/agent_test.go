package agent

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"echo-assistant/internal/openai"
	"echo-assistant/internal/prompts"
	"echo-assistant/internal/tools"
)

// A model that calls a tool in every round until tools are switched off: after AGENT_MAX_ROUNDS
// rounds the agent asks once more without tools, returns that answer and keeps the thread.
func TestStepLimitEndsWithAnswer(t *testing.T) {
	var requests int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := atomic.AddInt32(&requests, 1)
		var body map[string]any
		json.NewDecoder(r.Body).Decode(&body)
		out := []any{map[string]any{"type": "function_call", "name": "create_entry", "arguments": "{}",
			"call_id": fmt.Sprint("call", n)}}
		if body["tool_choice"] == "none" {
			out = []any{map[string]any{"type": "message", "content": []any{map[string]any{"text": "20 angelegt, 1 offen"}}}}
		}
		json.NewEncoder(w).Encode(map[string]any{"id": fmt.Sprint("resp", n), "output": out})
	}))
	defer server.Close()
	openai.SetBase(server.URL)
	defer openai.SetBase("https://api.openai.com")

	calls := 0
	registry := &tools.Registry{}
	registry.Add(tools.Tool{Name: "create_entry", Description: "test", Parameters: `{"type":"object","properties":{}}`,
		Run: func(map[string]any, tools.Context) string { calls++; return "Created" }})
	a := &Agent{Registry: registry}
	thread := ""
	answer := a.Run("viele Termine", Options{Thread: &thread})

	limit := prompts.Int("AGENT_MAX_ROUNDS", 20)
	if answer != "20 angelegt, 1 offen" {
		t.Errorf("answer %q", answer)
	}
	if calls != limit || int(requests) != limit+1 {
		t.Errorf("%d tool calls in %d requests, want %d in %d", calls, requests, limit, limit+1)
	}
	if thread != fmt.Sprint("resp", limit+1) {
		t.Errorf("thread %q", thread)
	}
}
