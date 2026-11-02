package memory

import (
	"strconv"
	"strings"
	"testing"
	"time"

	"echo-assistant/internal/config"
	"echo-assistant/internal/people"
	"echo-assistant/internal/store"
)

func TestBehaviourLog(t *testing.T) {
	store.Dir = t.TempDir()
	reg := people.New(config.Secrets{"OWNER_NAME": "Admin"})
	m := &Memory{People: reg}
	m.LogBehaviour(reg.Admin(), "Befehl", "Pause", "Küche")
	m.LogBehaviour("", "Tool", "set_timer kind=timer minutes=10", "")
	m.LogBehaviour("", "Tool", "  ", "") // nothing to note
	times := map[string]bool{}
	got := m.recentBehaviour(times)
	day := weekdays[time.Now().Weekday()]
	if strings.Count(got, "\n") != 2 || !strings.Contains(got, "("+day+", Küche, Admin) Befehl: Pause") ||
		!strings.Contains(got, "("+day+") Tool: set_timer kind=timer minutes=10") {
		t.Fatalf("recentBehaviour:\n%s", got)
	}
	// The entries' times count as evidence for the dream's new facts.
	at := time.Now().Format("2006-01-02 15:04")
	if !times[at] || evidenceDays([]string{"[" + at + "]"}, times) != 1 {
		t.Errorf("times %v", times)
	}
}

func TestLearnReport(t *testing.T) {
	store.Dir = t.TempDir()
	reg := people.New(config.Secrets{"OWNER_NAME": "Admin", "TELEGRAM_OWNER_ID": float64(42)})
	var sent []string
	reg.Channels.Telegram = func(html, person string) string { sent = append(sent, html); return "sent" }
	m := &Memory{People: reg}
	m.core.normalize()
	m.mu.Lock()
	id := m.addFactLocked("Vorliebe", "Gewohnheit: hört samstags Jazz", today())
	m.noteLearnedLocked("", "fact", id, "", "Gewohnheit: hört samstags Jazz")
	m.noteLearnedLocked(reg.Admin(), "profile", 0, "work", "arbeitet im Homeoffice")
	gone := m.addFactLocked("Sonstiges", "schon wieder vergessen", today())
	m.noteLearnedLocked("", "fact", gone, "", "schon wieder vergessen")
	m.removeFactLocked(gone)
	m.mu.Unlock()

	m.sendLearnReport()
	if len(sent) != 1 || !strings.Contains(sent[0], "1. Gewohnheit: hört samstags Jazz") ||
		!strings.Contains(sent[0], "2. Beruf und Alltag: arbeitet im Homeoffice") || strings.Contains(sent[0], "vergessen") {
		t.Fatalf("report: %q", sent)
	}
	m.mu.Lock()
	ctx := m.learnReportContextLocked(reg.Admin())
	m.mu.Unlock()
	if !strings.Contains(ctx, "1. [fact "+strconv.Itoa(id)+"] Gewohnheit") || !strings.Contains(ctx, "2. [profile work]") {
		t.Errorf("context: %s", ctx)
	}
	if m.learnReportDue() {
		t.Errorf("due again on the same day")
	}
}
