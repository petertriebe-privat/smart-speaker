package timers

import (
	"strings"
	"testing"
	"time"

	"echo-assistant/internal/store"
	"echo-assistant/internal/tools"
)

func TestParse(t *testing.T) {
	for _, c := range []struct {
		in   string
		want Command
	}{
		{"Timer zehn Minuten.", Command{Action: "set", After: 10 * time.Minute}},
		{"Stell einen Timer auf 5 Minuten", Command{Action: "set", After: 5 * time.Minute}},
		{"Timer fünfundzwanzig Minuten für die Nudeln.", Command{Action: "set", After: 25 * time.Minute, Label: "Nudeln"}},
		{"Timer eine halbe Stunde", Command{Action: "set", After: 30 * time.Minute}},
		{"Stell einen Timer für 1 Stunde und 30 Minuten", Command{Action: "set", After: 90 * time.Minute}},
		{"Timer 45 Sekunden", Command{Action: "set", After: 45 * time.Second}},
		{"Eieruhr eine Viertelstunde", Command{Action: "set", After: 15 * time.Minute}},
		{"Weck mich um 6:30", Command{Action: "alarm", Hour: 6, Minute: 30}},
		{"Wecker auf sieben Uhr", Command{Action: "alarm", Hour: 7}},
		{"Weck mich um halb sieben.", Command{Action: "alarm", Hour: 6, Minute: 30}},
		{"Wecker 7 Uhr 15", Command{Action: "alarm", Hour: 7, Minute: 15}},
		{"Weck mich in 20 Minuten", Command{Action: "set", After: 20 * time.Minute}},
		{"Timer aus", Command{Action: "cancel"}},
		{"Lösch den Wecker.", Command{Action: "cancel"}},
		// stays with GPT Live
		{"Wie lange läuft der Timer noch?", Command{}},
		{"Stell einen Timer", Command{}},
		{"Mach das Licht aus", Command{}},
		{"Erinnere mich in zehn Minuten an die Wäsche", Command{}},
	} {
		if got := Parse(c.in); got != c.want {
			t.Errorf("Parse(%q) = %+v, want %+v", c.in, got, c.want)
		}
	}
}

func TestServiceRings(t *testing.T) {
	store.Dir = t.TempDir()
	now := time.Date(2026, 10, 27, 22, 0, 0, 0, time.Local)
	s := New()
	s.Now = func() time.Time { return now }
	rang := make(chan Timer, 4)
	s.Ring = func(tm Timer) { rang <- tm }

	a := s.Add("alarm", 0, 6, 30, "")
	if a.At.Day() != 28 || a.At.Hour() != 6 {
		t.Errorf("alarm at %v, want tomorrow 06:30", a.At)
	}
	if got := Confirmation(a, now); got != "Wecker für morgen 06:30 Uhr gestellt." {
		t.Errorf("confirmation %q", got)
	}
	tm := s.Add("timer", 10*time.Minute, 0, 0, "Tee")
	if got := Confirmation(tm, now); got != "Timer für Tee, 10 Minuten, ab jetzt." {
		t.Errorf("confirmation %q", got)
	}
	if len(New().List()) != 2 {
		t.Errorf("not kept in the state dir")
	}
	now = now.Add(11 * time.Minute) // before Run: its goroutine reads the clock
	go s.Run()
	s.kick()
	select {
	case got := <-rang:
		if got.ID != tm.ID {
			t.Errorf("rang %+v", got)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("the timer did not ring")
	}
	if !s.Ringing() || len(s.Stop()) != 1 || s.Ringing() {
		t.Errorf("ringing/stop")
	}
	if left := s.List(); len(left) != 1 || left[0].Kind != "alarm" {
		t.Errorf("left %+v", left)
	}
}

func TestTools(t *testing.T) {
	store.Dir = t.TempDir()
	s := New()
	r := &tools.Registry{}
	s.AddTools(r)
	if out := r.Call("set_timer", `{"kind":"timer","minutes":0.5,"time":"","label":""}`, tools.Context{}); !strings.HasPrefix(out, "Timer, 30 Sekunden") {
		t.Errorf("set_timer: %s", out)
	}
	if out := r.Call("list_timers", `{}`, tools.Context{}); !strings.Contains(out, "#1 timer") {
		t.Errorf("list_timers: %s", out)
	}
	if out := r.Call("cancel_timer", `{"id":0}`, tools.Context{}); out != "Cancelled 1, stopped 0 ringing." {
		t.Errorf("cancel_timer: %s", out)
	}
}

func TestSpoken(t *testing.T) {
	for d, want := range map[time.Duration]string{
		90 * time.Minute: "1 Stunde und 30 Minuten", 3*time.Minute + 5*time.Second: "3 Minuten und 5 Sekunden",
		2 * time.Hour: "2 Stunden", 45 * time.Second: "45 Sekunden",
	} {
		if got := Spoken(d); got != want {
			t.Errorf("Spoken(%v) = %q", d, got)
		}
	}
}
