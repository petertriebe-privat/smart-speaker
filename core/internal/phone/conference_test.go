package phone

import (
	"strings"
	"testing"
	"time"

	"echo-assistant/internal/live"
)

// mobileCallOnSpeaker: a call on the mobile phone that is answered and on the speaker.
func mobileCallOnSpeaker(t *testing.T) (*harness, *fakeHF, *fakeSCO) {
	t.Helper()
	h, hf, lines, _ := mobileHarness(t)
	h.p.mob.mode = "on"
	h.p.pollMobile(time.Now())
	h.p.pollMobile(time.Now())
	hf.set(hfCall{Path: "/hfp/adapter_phone/voicecall01", State: "active", Number: "07161 123456"})
	h.p.pollMobile(time.Now())
	if len(*lines) != 1 || !h.p.MobileActive() {
		t.Fatal("the call is not on the speaker")
	}
	return h, hf, (*lines)[0]
}

func TestAssistantJoinsACallOnTheSpeaker(t *testing.T) {
	h, _, line := mobileCallOnSpeaker(t)
	if !h.p.CallOnSpeaker() {
		t.Fatal("no call to join")
	}
	if got := h.p.StartConference(); !strings.Contains(got, "in the call") {
		t.Fatalf("start: %q", got)
	}
	req := h.sess.Request()
	if req.Conference == "" || !req.Phone || req.Person != h.p.people.Admin() {
		t.Fatalf("request %+v", req)
	}
	// He hears the room and the far end in ONE stream: one feed per microphone frame, however
	// much the far end sent.
	line.deliver(constant(3000, frameLen))
	line.deliver(constant(3000, frameLen))
	h.sess.mu.Lock()
	h.sess.fed = 0
	h.sess.mu.Unlock()
	for i := 0; i < 3; i++ {
		h.p.Hear(sine(400, micRate, micFrame, 2000))
	}
	h.sess.mu.Lock()
	fed := h.sess.fed
	h.sess.mu.Unlock()
	if fed != 3 {
		t.Errorf("fed %d times for 3 microphone frames, want 3", fed)
	}
	// His voice goes into the room and onto the phone's line.
	out := req.Line
	out.Queue(constant(9000, apiFrame*5))
	h.p.bridge.mu.Lock()
	room := len(h.p.bridge.assistant)
	h.p.bridge.mu.Unlock()
	if room != apiFrame*5 {
		t.Errorf("%d samples of his voice for the room, want %d", room, apiFrame*5)
	}
	before := line.written
	h.p.Hear(make([]int16, micFrame))
	if line.written <= before {
		t.Error("his voice did not reach the phone's line")
	}
	if !out.Speaking(0) {
		t.Error("he does not count as speaking")
	}
	// He leaves (end_call, silence): the call goes on.
	h.sess.end()
	h.p.CallEnded(live.Ended{Request: req})
	if h.p.ConferenceActive() || !h.p.MobileActive() {
		t.Errorf("after he left: conference %v, call %v", h.p.ConferenceActive(), h.p.MobileActive())
	}
}

func TestTheButtonDuringACall(t *testing.T) {
	h, hf, _ := mobileCallOnSpeaker(t)
	short, long := 200*time.Millisecond, 1500*time.Millisecond
	if !h.p.ButtonOwned() {
		t.Fatal("the button does not belong to the call")
	}
	if got := h.p.CallButton(short); !strings.Contains(got, "in the call") {
		t.Errorf("short press, call running: %q, want Jarvis joining", got)
	}
	if got := h.p.CallButton(short); !strings.Contains(got, "left") {
		t.Errorf("short press, Jarvis in it: %q, want him leaving", got)
	}
	if got := h.p.CallButton(long); got != "Ended." || len(hf.hungUp) != 1 {
		t.Errorf("long press: %q, hung up %v", got, hf.hungUp)
	}
	// Ringing: short answers, long rejects.
	hf.set(hfCall{Path: "/hfp/adapter_phone/voicecall02", State: "incoming"})
	h.p.pollMobile(time.Now())
	if got := h.p.CallButton(long); got != "Ended." || len(hf.hungUp) != 2 {
		t.Errorf("long press while ringing: %q, hung up %v", got, hf.hungUp)
	}
	if got := h.p.CallButton(short); !strings.HasPrefix(got, "Answered") {
		t.Errorf("short press while ringing: %q", got)
	}
	// Nothing going on: the button is the conversation's again.
	hf.set()
	h.p.pollMobile(time.Now())
	if h.p.ButtonOwned() {
		t.Error("the button still belongs to a call that is over")
	}
}

// The call ends while the assistant is in it: it leaves with it.
func TestAssistantLeavesWithTheCall(t *testing.T) {
	h, hf, _ := mobileCallOnSpeaker(t)
	h.p.StartConference()
	hf.set()
	h.p.pollMobile(time.Now())
	if h.p.ConferenceActive() {
		t.Error("the conference outlived the call")
	}
	waitFor(t, "his conversation closed", func() bool { h.sess.mu.Lock(); defer h.sess.mu.Unlock(); return h.sess.closed > 0 })
}
