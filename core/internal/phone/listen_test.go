package phone

import (
	"strings"
	"testing"
	"time"

	"echo-assistant/internal/tools"
)

// "Ich will mithören" before the assistant's call is picked up (19.10.2026: speaker_call answered "no
// call is running" while it rang): the call is audible from the pick-up.
func TestListenBeforeThePickUp(t *testing.T) {
	h := newHarness(t, nil)
	h.p.reg.registered = true
	if got := h.p.placeCall("0301234567", "ask whether the windows are cleaned for free", tools.Context{Channel: tools.Agent}); !strings.HasPrefix(got, "Calling") {
		t.Fatalf("call: %q", got)
	}
	// still waiting to be dialled
	if got := h.p.SetBridge("listen"); !strings.Contains(got, "from the pick-up") || !h.p.wanted.listen {
		t.Fatalf("before dialling: %q", got)
	}
	h.p.wanted.listen = false
	h.p.dialIfRequested(time.Now())
	inv := h.read(t)
	// ringing
	if got := h.p.SetBridge("listen"); !strings.Contains(got, "from the pick-up") || !h.p.call.listen {
		t.Fatalf("while ringing: %q", got)
	}
	answer, _ := Parse("SIP/2.0 200 OK\r\nCall-ID: " + inv.Header("call-id") + "\r\nFrom: " + inv.Header("from") +
		"\r\nTo: " + inv.Header("to") + ";tag=r\r\nCSeq: 1 INVITE\r\nContact: <sip:x@127.0.0.1:5060>\r\n" +
		"Content-Type: application/sdp\r\nContent-Length: 0\r\n\r\n")
	answer.Body = "v=0\r\nc=IN IP4 127.0.0.1\r\nm=audio 9 RTP/AVP 8\r\n"
	h.p.onResponse(answer)
	if h.p.Bridge() != "listen" {
		t.Errorf("after the pick-up the bridge is %s", h.p.Bridge())
	}
}
