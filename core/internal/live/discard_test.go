package live

import (
	"testing"

	"echo-assistant/internal/audio"
)

// packet: 100 ms of GPT Live's audio at the given level.
func packet(level int16) []int16 {
	pcm := make([]int16, audio.SpeakerRate/10)
	for i := range pcm {
		pcm[i] = level
	}
	return pcm
}

func TestDiscardDropsTheRestOfTheAnswer(t *testing.T) {
	s := &Session{held: true}
	if !s.holdAudio(packet(5000)) {
		t.Fatal("held audio played")
	}
	s.Discard("test")
	// the dropped answer streams on, with a short pause between words
	for _, level := range []int16{5000, 5000, 0, 0, 5000, 5000} {
		if !s.holdAudio(packet(level)) {
			t.Fatalf("the rest of the discarded answer played (level %d)", level)
		}
	}
	// its end: a pause of discardQuiet, then the next answer plays
	for i := 0; i < discardQuiet/100; i++ {
		s.holdAudio(packet(0))
	}
	if s.holdAudio(packet(5000)) {
		t.Fatal("the next answer after the pause was dropped")
	}
}
