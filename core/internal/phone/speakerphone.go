package phone

import (
	"log"
	"strings"
	"time"
)

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

// Report: the way out and what came in, for the log at the end ("" when nothing went either way).
func (s *Speakerphone) Report() string { return joinReports(s.chain.report(), s.echo.report()) }

// micReport: the log of the microphone's way onto the line when the bridge goes off, so a call
// the other side hardly heard shows why (gate shut, or the room too quiet for the gain control).
func (p *Phone) micReport() {
	if r := joinReports(p.mic.report(), p.bridge.echo.report()); r != "" {
		log.Printf("[Mic] %s", r)
	}
}

func joinReports(parts ...string) string {
	var out []string
	for _, s := range parts {
		if s != "" {
			out = append(out, s)
		}
	}
	return strings.Join(out, "; ")
}
