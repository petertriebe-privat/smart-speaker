package live

import "testing"

func TestEndsSilently(t *testing.T) {
	for _, c := range []struct {
		req   Request
		heard bool
		want  bool
	}{
		{Request{Wake: true}, false, true},              // a false start: no greeting
		{Request{Wake: true, Again: true}, false, true}, // after "Wie bitte, Sir?" nobody answered
		{Request{Wake: true}, true, false},              // the user spoke
		{Request{}, false, false},                       // the button: the greeting
		{Request{Phone: true}, false, false},
		{Request{Wake: true, Conference: "Gast"}, false, false},
	} {
		if got := endsSilently(c.req, c.heard); got != c.want {
			t.Errorf("endsSilently(%+v, %v) = %v, want %v", c.req, c.heard, got, c.want)
		}
	}
}

func TestPromised(t *testing.T) {
	for _, s := range []string{
		"Hmm. Gleich hab ich's, einen Moment.", // #73
		"Einen Augenblick, ich blick kurz auf die Uhr.",
		"Moment bitte.",
		"Ich schaue nach.",
	} {
		if !promised.MatchString(s) {
			t.Errorf("not taken for a promise: %q", s)
		}
	}
	for _, s := range []string{
		"Es ist 18 Uhr 36, Sir.",
		"Ich frage mich, ob das reicht.",
		"Im Moment ist es trocken.",
		"Gute Nacht, Sir.",
	} {
		if promised.MatchString(s) {
			t.Errorf("taken for a promise: %q", s)
		}
	}
}
