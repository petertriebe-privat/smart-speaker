package phone

import "time"

// Speakerphone: the way out of the microphone for the intercom between two Echos
// (cmd/assistant/intercom.go) - the same gain control and gate as a call's microphone (mic.go). The
// other room's voice comes back from this room's speaker into its microphone; what the DSP leaves
// of it would reach the other room half a second late, so while the other room is audible here
// the microphone is turned down hard. One goroutine feeds Out (the microphone's), any feeds
// Played.
type Speakerphone struct {
	chain *micChain
	echo  roomEcho
}

func NewSpeakerphone() *Speakerphone { return &Speakerphone{chain: newMicChain()} }

// Out: a block of this microphone (16 kHz) the way it should go to the other room.
func (s *Speakerphone) Out(pcm []int16) []int16 { return s.chain.process(pcm, s.echo.active()) }

// Played: a frame of the other room goes to this speaker, which plays it after later.
func (s *Speakerphone) Played(pcm []int16, later time.Duration) {
	peak := 0
	for _, v := range pcm {
		peak = max(peak, int(v), -int(v))
	}
	s.echo.heard(peak, later)
}
