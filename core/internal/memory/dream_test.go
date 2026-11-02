package memory

import "testing"

func TestEvidenceDays(t *testing.T) {
	times := map[string]bool{"2026-10-01 10:00": true, "2026-10-02 16:28": true, "2026-10-02 18:00": true}
	got := evidenceDays([]string{"[2026-10-01 10:00]", " 2026-10-02 16:28 ", "2026-10-02 18:00", "2026-10-05 09:00", "short"}, times)
	if got != 2 {
		t.Errorf("evidenceDays = %d, want 2", got)
	}
}

func TestDreamChange(t *testing.T) {
	if got := dreamChange(200, 150); got != "200 → 150 (-25 %)" {
		t.Error(got)
	}
	if got := dreamChange(0, 3); got != "0 → 3" {
		t.Error(got)
	}
}

func TestAddKeywords(t *testing.T) {
	got := addKeywords([]string{"a"}, "a, b, , c")
	if len(got) != 3 || got[1] != "b" || got[2] != "c" {
		t.Errorf("got %v", got)
	}
}
