package live

import (
	"log"
	"time"
)

// Barge-in on the phone's loudspeaker (#75): The assistant plays as media there (the call path sounded
// like a phone call), so the phone's echo cancellation does not hear him. The app gates its own
// microphone while he is audible and decides when the user speaks over him (AudioBridge: it knows
// what plays when; the core's speaker level ran ahead of the sound, its own detector took his echo
// for the user and cut him off every two seconds, 01.11.2026). It tells the core, which stops him.

// bargeQuiet: after the interruption GPT Live's audio still in flight is dropped.
const bargeQuiet = 1200 * time.Millisecond

// BargeIn: the user speaks over the assistant: it stops at once, and what GPT Live still sends of the old
// answer is dropped for a moment, until it has heard the user.
func (s *Session) BargeIn() {
	if !s.out().Speaking(0) && !s.barging() {
		return
	}
	log.Print("[Barge-in] the user speaks over the assistant")
	s.bargeUntil.Store(time.Now().Add(bargeQuiet).UnixMilli())
	s.out().Flush()
}

// barging: The assistant was interrupted a moment ago.
func (s *Session) barging() bool {
	return time.Now().UnixMilli() < s.bargeUntil.Load()
}
