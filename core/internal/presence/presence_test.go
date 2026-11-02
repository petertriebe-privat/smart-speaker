package presence

import (
	"strings"
	"testing"
)

func TestMatchPerson(t *testing.T) {
	register := map[string]string{"alex": "alex", "kim": "kim", "jo smith": "jo"}
	byName := func(n string) string { return register[strings.ToLower(n)] }
	for _, c := range []struct{ id, friendly, want string }{
		{"person.alex", "", "alex"},                 // the id is the register's
		{"person.someone", "Kim", "kim"},            // the name is
		{"person.alex_smith", "Alex Smith", "alex"}, // HA's full name: the first name
		{"person.jo_smith", "Jo Smith", "jo"},       // a full name in the register wins
		{"person.unknown_one", "Unknown One", ""},
		{"person.", "", ""},
	} {
		if got := matchPerson(byName, c.id, c.friendly); got != c.want {
			t.Errorf("%s %q: got %q, want %q", c.id, c.friendly, got, c.want)
		}
	}
}
