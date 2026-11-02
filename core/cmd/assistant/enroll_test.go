package main

import (
	"math"
	"strings"
	"testing"
)

func TestEnrollArgs(t *testing.T) {
	for _, c := range []struct {
		in      []string
		session string
		count   int
		ok      bool
	}{
		{[]string{"anna", "near"}, "near", enrollDefaultTakes, true},
		{[]string{"anna", "far", "25"}, "far", 25, true},
		{[]string{"anna", "negative"}, "negative", enrollDefaultSeconds, true},
		{[]string{"anna", "negative", "60"}, "negative", 60, true},
		{[]string{"anna"}, "", 0, false},
		{[]string{"Anna", "near"}, "", 0, false},      // lower case only
		{[]string{"../x", "near"}, "", 0, false},      // no paths
		{[]string{"anna", "near", "0"}, "", 0, false}, // at least one take
		{[]string{"anna", "near", "x"}, "", 0, false},
	} {
		_, session, count, err := enrollArgs(c.in)
		if (err == nil) != c.ok || c.ok && (session != c.session || count != c.count) {
			t.Errorf("%v: session %q count %d err %v", c.in, session, count, err)
		}
	}
}

func TestGuideSteps(t *testing.T) {
	seen := map[string]bool{}
	for _, s := range guideSteps {
		if !enrollName.MatchString(s.session) || seen[s.session] {
			t.Errorf("session %q: invalid or twice", s.session)
		}
		seen[s.session] = true
		free := strings.HasPrefix(s.session, "negative")
		if free != (s.takes == 0) || free != (s.seconds > 0) {
			t.Errorf("%s: negative sessions are free talk (seconds), the others takes", s.session)
		}
		if s.say == "" {
			t.Errorf("%s: no announcement", s.session)
		}
		// a stock-model "Hey Jarvis" in an announcement would wake the other devices
		if strings.Contains(strings.ToLower(s.say+s.after), "hey jarvis") {
			t.Errorf("%s: says the old wake word", s.session)
		}
	}
}

func TestBeepShare(t *testing.T) {
	tone := func(hz float64) []int16 {
		pcm := make([]int16, 320) // one 20 ms chunk
		for i := range pcm {
			pcm[i] = int16(3000 * math.Sin(2*math.Pi*hz*float64(i)/16000))
		}
		return pcm
	}
	if s := beepShare(tone(enrollBeepHz)); s < 0.9 {
		t.Errorf("the beep itself: %.2f", s)
	}
	for _, hz := range []float64{150, 300, 600, 1500} { // a voice's fundamentals and formants
		if s := beepShare(tone(hz)); s > 0.1 {
			t.Errorf("%.0f Hz counted as the beep: %.2f", hz, s)
		}
	}
	if beepShare(make([]int16, 320)) != 0 || beepShare(nil) != 0 {
		t.Error("silence")
	}
}

func TestVoiceThreshold(t *testing.T) {
	if got := voiceThreshold(nil); got != 50 {
		t.Errorf("no history: %d", got)
	}
	if got := voiceThreshold([]int{10, 12, 9, 11, 300}); got != 50 {
		t.Errorf("a quiet room keeps the floor of 50: %d", got)
	}
	// music at ~100 in BMicsOut: three times the median, one loud chunk does not count
	if got := voiceThreshold([]int{90, 100, 110, 100, 2000}); got != 300 {
		t.Errorf("music: %d", got)
	}
}
