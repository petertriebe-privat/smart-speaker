package occupancy

import (
	"testing"
	"time"
)

func TestRoomLogic(t *testing.T) {
	s := New(nil, nil)
	s.found = true
	var changes []bool
	s.OnChange = func(o bool) { changes = append(changes, o) }
	t0 := time.Now()
	// Someone comes in: motion flag with a positive motion value.
	s.tick(t0, true, motionFlag|presenceFlag, true, 500, 800)
	if !s.occupied || !s.trusted {
		t.Fatal("expected occupied and trusted")
	}
	// Quiet for less than the still-empty limit: stays occupied.
	s.tick(t0.Add(5*time.Minute), true, 0, true, 0, 0)
	if !s.occupied {
		t.Fatal("left too early")
	}
	// Walking out, then silence for the shorter limit.
	s.tick(t0.Add(6*time.Minute), true, 0, true, 0, -2000)
	s.tick(t0.Add(6*time.Minute+s.emptyAfter+time.Second), true, 0, true, 0, 0)
	if s.occupied {
		t.Fatal("expected empty")
	}
	if len(changes) != 2 || !changes[0] || changes[1] {
		t.Fatalf("changes: %v", changes)
	}
}
