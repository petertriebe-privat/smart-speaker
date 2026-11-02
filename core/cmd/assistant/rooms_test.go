package main

import "testing"

func TestRoomMatches(t *testing.T) {
	for _, c := range []struct {
		room, name  string
		loose, want bool
	}{
		{"Küche", "Küche", false, true},
		{"in der Küche", "Küche", false, false},
		{"in der Küche", "Küche", true, true},
		{"Jarvis", "Jarvis 2", true, false},
		{"Jarvis 2", "Jarvis", true, false},
		{"jarvis 2", "Jarvis 2", false, true},
		{"Kinderzimmer", "", true, false},
	} {
		if got := roomMatches(c.room, c.name, c.loose); got != c.want {
			t.Errorf("%q / %q loose %v: %v", c.room, c.name, c.loose, got)
		}
	}
}
