package phone

import (
	"net"
	"sync/atomic"
	"testing"
	"time"

	"echo-assistant/internal/live"
)

func TestRoomAtAnotherEcho(t *testing.T) {
	h := newHarness(t, nil)
	from := h.reg.LocalAddr().(*net.UDPAddr)
	h.p.onRequest(invite("r1", "**610"), from)
	h.read(t)
	h.read(t)
	h.sess.set(live.Live)
	h.p.updateCall(time.Now()) // answered: The assistant talks
	h.read(t)
	if !h.p.CallForRoom() {
		t.Fatal("a running call must be offered to another Echo")
	}
	var tones, frames atomic.Int32
	var gone atomic.Bool
	remote := RemoteSpeaker(func([]int16) { frames.Add(1) },
		func(float64, int, float64) { tones.Add(1) }, func() {})
	if got := h.p.MoveRoom(remote, "talk", func() { gone.Store(true) }); got == "" || got[:5] == "Error" {
		t.Fatalf("move: %s", got)
	}
	if h.p.Bridge() != "talk" || !h.p.bridge.remote() {
		t.Fatalf("bridge %s, remote %v", h.p.Bridge(), h.p.bridge.remote())
	}
	if tones.Load() != 2 {
		t.Errorf("%d announcement tones at the other Echo", tones.Load())
	}
	h.speaker.mu.Lock()
	local := h.speaker.tones
	h.speaker.mu.Unlock()
	if local != 0 {
		t.Errorf("%d tones here, the room is elsewhere", local)
	}
	time.Sleep(2*announceTone + 100*time.Millisecond)        // the call waits while the tones play
	h.p.bridge.Call(make([]int16, 2*apiFrame+bridgePrefill)) // the other side speaks
	time.Sleep(150 * time.Millisecond)
	if frames.Load() == 0 {
		t.Error("the call does not play at the other Echo")
	}
	// The caller hangs up: the room is home again and the other Echo is told.
	bye, _ := Parse("BYE sip:jarvis@127.0.0.1 SIP/2.0\r\nVia: SIP/2.0/UDP 127.0.0.1:5060;branch=z9hG4bK9\r\n" +
		"From: <sip:**610@127.0.0.1>;tag=a\r\nTo: <sip:jarvis@127.0.0.1>;tag=b\r\nCall-ID: r1\r\nCSeq: 2 BYE\r\n\r\n")
	h.p.onRequest(bye, from)
	time.Sleep(50 * time.Millisecond)
	if h.p.bridge.remote() || !gone.Load() {
		t.Errorf("after the call: remote %v, told %v", h.p.bridge.remote(), gone.Load())
	}
}
