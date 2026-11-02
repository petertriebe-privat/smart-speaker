package prefer

import (
	"errors"
	"strings"
	"sync"
	"testing"
	"time"
)

// waitAsked: the background question has stored its answer.
func waitAsked(t *testing.T, p *Prefs) {
	for i := 0; i < 200; i++ {
		p.mu.Lock()
		n := len(p.asking)
		p.mu.Unlock()
		if n == 0 {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("the question did not end")
}

func TestHourFromRules(t *testing.T) {
	now := time.Date(2026, 10, 24, 10, 0, 0, 0, time.Local) // a Saturday
	rules := "- Am Wochenende kein Briefing vor 9."
	var mu sync.Mutex
	asked := 0
	p := New(func(string) string { return rules })
	p.Now = func() time.Time { return now }
	p.Ask = func(input string, choices []string, _ string) (string, error) {
		mu.Lock()
		asked++
		first := asked == 1
		mu.Unlock()
		if first && !strings.Contains(input, "Saturday") || !strings.Contains(input, "the start value 4") ||
			choices[0] != "default" || len(choices) != 25 {
			t.Errorf("input %q, choices %v", input, choices)
		}
		return "9", nil
	}
	if got := p.Hour("p", "briefing", 4); got != 4 {
		t.Fatalf("before the answer: %d, want the start value", got)
	}
	waitAsked(t, p)
	if got := p.Hour("p", "briefing", 4); got != 9 || asked != 1 {
		t.Fatalf("after the answer: %d, asked %d", got, asked)
	}
	// a new day asks again
	now = now.AddDate(0, 0, 2)
	p.Hour("p", "briefing", 4)
	waitAsked(t, p)
	if asked != 2 {
		t.Fatalf("a new day: asked %d", asked)
	}
	// changed rules ask again
	rules = "- Briefing erst ab 7."
	p.Hour("p", "briefing", 4)
	waitAsked(t, p)
	if asked != 3 {
		t.Fatalf("new rules: asked %d", asked)
	}
}

func TestDefaultsWithoutRulesOrAnswer(t *testing.T) {
	p := New(func(string) string { return "" })
	p.Ask = func(string, []string, string) (string, error) { t.Fatal("asked without rules"); return "", nil }
	if p.Hour("p", "mail_digest", 18) != 18 {
		t.Fatal("no rules: not the start value")
	}
	p = New(func(string) string { return "- irgendwas" })
	p.Ask = func(string, []string, string) (string, error) { return "default", nil }
	p.Minutes("p", "urgent", []int{5, 10, 15, 30}, 15)
	waitAsked(t, p)
	if p.Minutes("p", "urgent", []int{5, 10, 15, 30}, 15) != 15 {
		t.Fatal("default answer: not the start value")
	}
	p = New(func(string) string { return "- irgendwas" })
	p.Ask = func(string, []string, string) (string, error) { return "", errors.New("down") }
	p.Hour("p", "evening", 18)
	waitAsked(t, p)
	if p.Hour("p", "evening", 18) != 18 {
		t.Fatal("an error: not the start value")
	}
	var nilPrefs *Prefs
	if nilPrefs.Hour("p", "evening", 18) != 18 {
		t.Fatal("nil prefs")
	}
}
