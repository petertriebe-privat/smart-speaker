package live

import (
	"log"
	"time"

	"echo-assistant/internal/audio"
)

// The hold (#59): after the button or the wake word the device decides about the user's first
// utterance itself (a quick command like "Pause" or not). Until then GPT Live hears everything
// but its voice and its tool calls wait here: before, GPT Live greeted, said "ich pausiere"
// without delegating or started music to a "Pause" while the quick path ran the command
// (18.10.2026). A quick command closes the conversation and the held answer is dropped; anything
// else releases it and the answer plays as if it had never waited. GPT Live takes 1.5-2 s to
// answer anyway, about as long as the decision.

type heldCall struct{ name, arguments, callID string }

// Holding: GPT Live's voice and calls are held.
func (s *Session) Holding() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.held
}

// HoldFor: the hold ends d from now at the latest (the decision is near, or needs a moment more).
func (s *Session) HoldFor(d time.Duration) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.held {
		s.holdUntil = time.Now().Add(d)
	}
}

// Release ends the hold: what GPT Live said meanwhile plays, its calls run.
func (s *Session) Release(why string) {
	s.outMu.Lock()
	s.mu.Lock()
	if !s.held {
		s.mu.Unlock()
		s.outMu.Unlock()
		return
	}
	s.held = false
	audio, calls := s.heldAudio, s.heldCalls
	s.heldAudio, s.heldCalls = nil, nil
	s.lastActivity = time.Now() // the timers start now, not when the conversation began
	live := s.state == Live
	s.mu.Unlock()
	log.Printf("[Hold] released (%s): %d audio packets, %d calls", why, len(audio), len(calls))
	if live {
		voiced := false
		for _, pcm := range audio {
			// the silence GPT Live streamed before its answer would only delay it
			if voiced = voiced || peakOf(pcm) >= silencePeak; voiced {
				s.play(pcm)
			}
		}
	}
	s.outMu.Unlock()
	if s.hooks.HoldEnded != nil {
		s.hooks.HoldEnded()
	}
	for _, c := range calls {
		s.onFunctionCall(c.name, c.arguments, c.callID)
	}
}

// Discard ends the hold and drops what GPT Live said meanwhile (it answered the request the
// device then failed to do itself: "Ich setze fort" would be wrong). A note for GPT Live follows.
func (s *Session) Discard(why string) {
	s.mu.Lock()
	if !s.held {
		s.mu.Unlock()
		return
	}
	s.held = false
	n := len(s.heldAudio)
	s.heldAudio, s.heldCalls = nil, nil
	s.dropping, s.dropSince, s.dropQuietMs, s.dropMs = true, time.Now(), 0, 0
	s.lastActivity = time.Now()
	s.mu.Unlock()
	log.Printf("[Hold] discarded (%s): %d audio packets", why, n)
	if s.hooks.HoldEnded != nil {
		s.hooks.HoldEnded()
	}
}

// HeardUser: GPT Live has transcribed something the user said in this conversation.
func (s *Session) HeardUser() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.heardUser
}

// LiveAt: when the conversation became live, the moment GPT Live got the microphone audio that
// was recorded while it connected; zero while it is not live yet.
func (s *Session) LiveAt() time.Time {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.liveAt
}

// WaitLive waits up to d for the conversation to be live; false if it is not.
func (s *Session) WaitLive(d time.Duration) bool {
	for end := time.Now().Add(d); time.Now().Before(end); time.Sleep(100 * time.Millisecond) {
		switch s.State() {
		case Live:
			return true
		case Idle, Closing:
			return false
		}
	}
	return s.State() == Live
}

// holdExpired: the deadline passed without a decision.
func (s *Session) holdExpired(now time.Time) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.held && now.After(s.holdUntil)
}

// holdAudio keeps GPT Live's audio while held; false = play it now. Called with s.outMu held.
func (s *Session) holdAudio(pcm []int16) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.dropping {
		s.dropRest(pcm)
		return true
	}
	if !s.held {
		return false
	}
	if len(s.heldAudio) < 2000 { // far more than any held answer
		s.heldAudio = append(s.heldAudio, pcm)
	}
	return true
}

// holdCall keeps a tool call while held; false = run it now.
func (s *Session) holdCall(name, arguments, callID string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.held {
		return false
	}
	log.Printf("[Hold] %s held", name)
	s.heldCalls = append(s.heldCalls, heldCall{name, arguments, callID})
	return true
}

// After Discard GPT Live still streams the dropped answer in real time: only what was held so far
// was gone, the rest played from the middle of a sentence, and the answer to the device's own
// transcript came after it (28.10.2026). GPT Live marks no end of an answer, but it streams its
// pauses as quiet packets: the rest is dropped up to the first pause of discardQuiet (at most
// discardMax). Called with s.mu held.
const (
	discardQuiet = 500  // ms
	discardMax   = 10e3 // ms
)

func (s *Session) dropRest(pcm []int16) {
	ms := len(pcm) * 1000 / audio.SpeakerRate
	s.dropMs += ms
	if peakOf(pcm) < silencePeak {
		s.dropQuietMs += ms
	} else {
		s.dropQuietMs = 0
	}
	if s.dropQuietMs >= discardQuiet || time.Since(s.dropSince) > discardMax*time.Millisecond {
		s.dropping = false
		log.Printf("[Hold] the rest of the discarded answer dropped: %d ms", s.dropMs)
	}
}

func peakOf(pcm []int16) int {
	peak := 0
	for _, v := range pcm {
		peak = max(peak, int(v), -int(v))
	}
	return peak
}
