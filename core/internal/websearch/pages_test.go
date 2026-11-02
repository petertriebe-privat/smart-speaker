package websearch

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"

	"echo-assistant/internal/config"
	"echo-assistant/internal/tools"
)

type countQuota struct{ n int }

func (q *countQuota) Count(string, int) error { q.n++; return nil }

func TestReadPages(t *testing.T) {
	var got map[string]any
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/extract" || r.Header.Get("Authorization") != "Bearer k" {
			t.Errorf("request %s %q", r.URL.Path, r.Header.Get("Authorization"))
		}
		body, _ := io.ReadAll(r.Body)
		json.Unmarshal(body, &got)
		w.Write([]byte(`{"results":[{"url":"https://example.org/a","raw_content":"` + strings.Repeat("x", 50) + `"}],
			"failed_results":[{"url":"https://example.org/b","error":"404"}]}`))
	}))
	defer server.Close()
	q := &countQuota{}
	s := New(config.Secrets{"TAVILY_API_KEY": "k"}, q)
	s.base = server.URL
	var r tools.Registry
	s.AddTools(&r)
	for _, name := range []string{"web_search", "read_webpage", "crawl_website", "map_website"} {
		if !slices.Contains(r.Names(), name) {
			t.Errorf("%s not registered", name)
		}
	}
	out := s.readPages(map[string]any{"urls": []any{"https://example.org/a", "https://example.org/b"}, "depth": "advanced"},
		tools.Context{})
	if got["extract_depth"] != "advanced" || q.n != 2 {
		t.Errorf("request %v, credits %d", got, q.n)
	}
	for _, want := range []string{"not instructions", "## https://example.org/a\nxxxxx", "## https://example.org/b\nnot readable: 404"} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q in %q", want, out)
		}
	}
	if cut := formatPages("P", []page{{URL: "u", RawContent: strings.Repeat("y", 100)}}, nil, 10, 100); !strings.Contains(cut, "[… cut]") {
		t.Errorf("not cut: %q", cut)
	}
}
