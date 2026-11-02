package mcpservers

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"echo-assistant/internal/store"
	"echo-assistant/internal/tools"
)

func TestPrefixAndNames(t *testing.T) {
	cases := map[string]string{"Notion": "notion", "My  Cool-Server!": "my_cool_server", "!!!": "mcp", "": "mcp"}
	for in, want := range cases {
		if got := prefix(in); got != want {
			t.Errorf("prefix(%q) = %q, want %q", in, got, want)
		}
	}
	if got := offeredName("Notion", "search pages.v2"); got != "notion_search_pages_v2" {
		t.Errorf("offeredName = %q", got)
	}
	if got := offeredName("a", strings.Repeat("x", 100)); len(got) != 64 {
		t.Errorf("offeredName length %d", len(got))
	}
}

func TestTruncateAndHost(t *testing.T) {
	if got := truncate("aä", 2); got != "a" {
		t.Errorf("truncate split a rune: %q", got)
	}
	if got := hostOf("https://example.com:8443/mcp?key=secret"); got != "example.com:8443" {
		t.Errorf("hostOf = %q", got)
	}
}

func TestParseConfig(t *testing.T) {
	data := `{"servers":[{"name":"A","url":"https://x/mcp","token":"t","private":false,"only":["q"]},
		{"name":"","url":"https://y"},{"name":"B","url":"http://z"}]}`
	list := parseConfig([]byte(data))
	if len(list) != 2 {
		t.Fatalf("got %d entries", len(list))
	}
	if newServer(list[0]).private || !newServer(list[1]).private {
		t.Error("private flag: explicit false must stay false, missing must be true")
	}
	if parseConfig([]byte("nonsense")) != nil {
		t.Error("broken file must give no servers")
	}
}

func TestRenderResult(t *testing.T) {
	got := renderResult(json.RawMessage(`{"content":[{"type":"text","text":"a"},{"type":"image"},
		{"type":"resource_link","name":"n","uri":"u"}]}`))
	if got != "a\n[image]\nn u" {
		t.Errorf("render = %q", got)
	}
	if got := renderResult(json.RawMessage(`{"content":[]}`)); got != "Done." {
		t.Errorf("empty = %q", got)
	}
	if got := renderResult(json.RawMessage(`{"content":[{"type":"text","text":"bad"}],"isError":true}`)); got != "Error: bad" {
		t.Errorf("error = %q", got)
	}
	long := `{"content":[{"type":"text","text":"` + strings.Repeat("x", 13000) + `"}]}`
	if got := renderResult(json.RawMessage(long)); !strings.HasSuffix(got, "\n[cut off]") || len(got) > 12020 {
		t.Errorf("not cut: %d", len(got))
	}
}

// fake is a minimal MCP server over loopback.
func fake(t *testing.T) *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var req struct {
			ID     json.RawMessage `json:"id"`
			Method string          `json:"method"`
		}
		json.Unmarshal(body, &req)
		if r.Header.Get("X-Key") != "k" {
			w.WriteHeader(401)
			return
		}
		var result string
		switch req.Method {
		case "initialize":
			result = `{"protocolVersion":"2025-06-18","serverInfo":{"name":"fake"}}`
		case "tools/list":
			result = `{"tools":[{"name":"say hi","description":"Greets","inputSchema":{"properties":{"who":{"type":"string"}}}},` +
				`{"name":"decide","description":"[device] A button tap"}]}`
		case "tools/call":
			result = `{"content":[{"type":"text","text":"hello"}]}`
		default:
			w.WriteHeader(202)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"jsonrpc":"2.0","id":`+string(req.ID)+`,"result":`+result+`}`)
	}))
}

func TestConnectCallDisconnect(t *testing.T) {
	store.Dir = t.TempDir()
	srv := fake(t)
	defer srv.Close()
	s := New()
	r := &tools.Registry{}
	s.AddTools(r)

	out := r.Call("connect_mcp_server", `{"name":"Fake","url":"`+srv.URL+`","token":"k","header":"X-Key","description":"test"}`, tools.Context{})
	if !strings.HasPrefix(out, "Connected Fake with 1 tool, only for the admin:\n- fake_say_hi: [Fake]") && !strings.HasPrefix(out, "Connected Fake with 1 tool, only for the admin:\n- fake_say_hi: Greets") {
		t.Fatalf("connect: %s", out)
	}
	if got := r.Call("fake_say_hi", `{"who":"x"}`, tools.Context{}); got != "hello" {
		t.Errorf("call = %q", got)
	}
	// A device-only tool is never offered, but the device can call it (#57).
	if got := r.Call("fake_decide", `{"data":"n:y"}`, tools.Context{}); !strings.HasPrefix(got, "Error") {
		t.Errorf("device tool offered to the model: %q", got)
	}
	if got, ok := s.CallDevice("decide", "n:y"); !ok || got != "hello" {
		t.Errorf("CallDevice = %q, %v", got, ok)
	}
	if _, ok := s.CallDevice("say hi", ""); ok {
		t.Errorf("CallDevice reached a tool for the model")
	}
	if !strings.Contains(s.Summary(), "- Fake: test") {
		t.Errorf("summary = %q", s.Summary())
	}
	if list := r.Call("list_mcp_servers", `{}`, tools.Context{}); !strings.Contains(list, `"status":"connected"`) || strings.Contains(list, "secret") {
		t.Errorf("list = %s", list)
	}
	if got := New().servers; len(got) != 1 || got[0].token != "k" || got[0].header != "X-Key" {
		t.Errorf("not persisted: %+v", got)
	}
	if out := r.Call("connect_mcp_server", `{"name":"fake!","url":"`+srv.URL+`","token":"k","header":"X-Key"}`, tools.Context{}); !strings.Contains(out, "too close") {
		t.Errorf("duplicate prefix: %s", out)
	}
	if out := r.Call("connect_mcp_server", `{"name":"Bad","url":"`+srv.URL+`"}`, tools.Context{}); !strings.Contains(out, "Nothing saved") {
		t.Errorf("auth failure: %s", out)
	}
	if out := r.Call("disconnect_mcp_server", `{"name":"FAKE"}`, tools.Context{}); !strings.HasPrefix(out, "Disconnected Fake") {
		t.Errorf("disconnect: %s", out)
	}
	if got := r.Call("list_mcp_servers", `{}`, tools.Context{}); got != "No MCP servers connected." {
		t.Errorf("list after = %q", got)
	}
}
