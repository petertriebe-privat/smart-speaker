package phone

import (
	"net"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"echo-assistant/internal/config"
	"echo-assistant/internal/live"
	"echo-assistant/internal/tools"
)

// The phone's app waited for the pick-up and takes the room at once (cmd/assistant/roomstream.go):
// the assistant has not greeted yet, so the greeting tells the other side and starts the
// three-way call, as for phone_call talk at home.
func TestRoomTakenAtThePickUp(t *testing.T) {
	h := newHarness(t, nil)
	h.register(t)
	r := &tools.Registry{}
	h.p.AddTools(r)
	r.Call("phone_call", `{"to": "07161 123456", "task": "einen Termin vereinbaren"}`, tools.Context{Channel: tools.Agent})
	h.p.dialIfRequested(time.Now())
	h.answerOurCall(t)
	var tones atomic.Int32
	remote := RemoteSpeaker(func([]int16) {}, func(float64, int, float64) { tones.Add(1) }, func() {})
	if got := h.p.MoveRoom(remote, "talk", func() {}); !strings.Contains(got, "talk at the other Echo") {
		t.Fatalf("move: %q", got)
	}
	if h.p.Bridge() != "talk" || tones.Load() != 2 {
		t.Fatalf("bridge %s, %d tones there", h.p.Bridge(), tones.Load())
	}
	h.sess.set(live.Live)
	h.p.updateCall(time.Now())
	h.sess.mu.Lock()
	notes := strings.Join(h.sess.commentary, " | ")
	h.sess.mu.Unlock()
	if !strings.Contains(notes, "§ 201") || !strings.Contains(notes, "three-way") || !h.p.ConferenceActive() {
		t.Errorf("after the greeting: conference %v, notes %q", h.p.ConferenceActive(), notes)
	}
}

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

// The owner dials in and asks for another room ("verbinde mich mit dem Arbeitszimmer"): that Echo
// fetches the call's room (OtherRoom, cmd/assistant/roomlink.go), and the call is the bridge there
// as it would be here.
func TestCallerConnectedWithAnotherEcho(t *testing.T) {
	h := newHarness(t, config.Secrets{"PHONE_CALLERS": "0160 9999999,0171 1234567"})
	from := h.reg.LocalAddr().(*net.UDPAddr)
	h.p.onRequest(invite("o1", "01711234567"), from)
	h.read(t)
	h.read(t)
	h.sess.set(live.Live)
	h.p.updateCall(time.Now())
	h.read(t) // 200 OK
	var r tools.Registry
	h.p.AddTools(&r)
	call := tools.Context{Channel: tools.Voice, Conversation: &fakeConversation{label: "phone"}}
	var asked []string
	var tones, frames atomic.Int32
	var gone atomic.Bool
	remote := RemoteSpeaker(func([]int16) { frames.Add(1) }, func(float64, int, float64) { tones.Add(1) }, func() {})
	h.p.deps.OtherRoom = func(room, mode string) (string, bool) {
		asked = append(asked, room+" "+mode)
		return h.p.ConnectSpeakerAt(mode, remote, func() { gone.Store(true) }), true // as the other Echo would
	}
	// Not released for the room: refused before another Echo is asked.
	if got := r.Call("connect_speaker", `{"mode":"talk","room":"Arbeitszimmer"}`, call); !strings.Contains(got, "not released") {
		t.Fatalf("unreleased: %q", got)
	}
	if len(asked) != 0 {
		t.Fatalf("another Echo was asked for an unreleased number: %v", asked)
	}
	h.p.allowSpeaker("0171 1234567")
	got := r.Call("connect_speaker", `{"mode":"talk","room":"Arbeitszimmer"}`, call)
	if !strings.Contains(got, "both ways") {
		t.Fatalf("connect: %q", got)
	}
	if h.p.call.stage != stageBridge || h.p.Bridge() != "talk" || !h.p.bridge.remote() {
		t.Fatalf("stage %v, mode %q, remote %v", h.p.call.stage, h.p.Bridge(), h.p.bridge.remote())
	}
	if h.sess.endSoon != 1 {
		t.Errorf("the session was not ended (%d)", h.sess.endSoon)
	}
	if tones.Load() != 2 || h.speaker.toneCount() != 0 {
		t.Errorf("tones there %d, here %d", tones.Load(), h.speaker.toneCount())
	}
	// The other room's microphone reaches the line, this one's does not.
	h.p.Hear(sine(400, micRate, micFrame, 3000))
	h.p.call.line.mu.Lock()
	here := len(h.p.call.line.mic)
	h.p.call.line.mu.Unlock()
	if here != 0 {
		t.Error("this Echo's microphone went onto the line")
	}
	h.p.HearRemote(sine(400, micRate, micFrame, 3000))
	h.p.call.line.mu.Lock()
	there := len(h.p.call.line.mic)
	h.p.call.line.mu.Unlock()
	if there == 0 {
		t.Error("the other room's microphone does not reach the line")
	}
	// The caller hangs up: the other Echo is told.
	bye, _ := Parse("BYE sip:jarvis@127.0.0.1 SIP/2.0\r\nVia: SIP/2.0/UDP 127.0.0.1:5060;branch=z9hG4bK9\r\n" +
		"From: <sip:01711234567@127.0.0.1>;tag=a\r\nTo: <sip:jarvis@127.0.0.1>;tag=b\r\nCall-ID: o1\r\nCSeq: 2 BYE\r\n\r\n")
	h.p.onRequest(bye, from)
	time.Sleep(50 * time.Millisecond)
	if h.p.bridge.remote() || !gone.Load() {
		t.Errorf("after the call: remote %v, told %v", h.p.bridge.remote(), gone.Load())
	}
	if len(asked) != 1 || asked[0] != "Arbeitszimmer talk" {
		t.Errorf("asked %v", asked)
	}
}

// The room asked for is this Echo's own: the call stays here, as without a room.
func TestCallerAsksForThisRoom(t *testing.T) {
	h := newHarness(t, config.Secrets{"PHONE_CALLERS": "0171 1234567"})
	from := h.reg.LocalAddr().(*net.UDPAddr)
	h.p.onRequest(invite("o2", "01711234567"), from)
	h.read(t)
	h.read(t)
	h.sess.set(live.Live)
	h.p.updateCall(time.Now())
	h.read(t)
	h.p.allowSpeaker("0171 1234567")
	var r tools.Registry
	h.p.AddTools(&r)
	h.p.deps.OtherRoom = func(room, mode string) (string, bool) { return "", false }
	got := r.Call("connect_speaker", `{"mode":"monitor","room":"Wohnzimmer"}`,
		tools.Context{Channel: tools.Voice, Conversation: &fakeConversation{label: "phone"}})
	if !strings.Contains(got, "you hear the microphone") || h.p.bridge.remote() || h.p.Bridge() != "monitor" {
		t.Errorf("%q, remote %v, mode %s", got, h.p.bridge.remote(), h.p.Bridge())
	}
}
