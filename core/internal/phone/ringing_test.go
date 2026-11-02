package phone

import (
	"net"
	"strconv"
	"testing"
	"time"
)

// registerWith: a registration whose 200 OK lists these contacts (the FritzBox lists every
// device registered with the account).
func (h *harness) registerWith(t *testing.T, contacts ...string) {
	t.Helper()
	h.p.updateRegistration(time.Now())
	m := h.read(t)
	text := "SIP/2.0 200 OK\r\nCall-ID: " + m.Header("call-id") + "\r\nCSeq: " + m.Header("cseq") + "\r\n"
	for _, c := range contacts {
		text += "Contact: " + c + "\r\n"
	}
	ok, _ := Parse(text + "\r\n")
	h.p.onResponse(ok)
}

func ownContact(expires int) string {
	return "<sip:jarvis@127.0.0.1:" + strconv.Itoa(sipPort) + ">;expires=" + strconv.Itoa(expires)
}

// Two Echos on one account: the expiry is our own binding's, not the first one's, and the other
// device is counted.
func TestRegistrationSeesOtherDevices(t *testing.T) {
	h := newHarness(t, nil)
	h.registerWith(t, "<sip:jarvis@192.0.2.7:5060>;expires=30", ownContact(120))
	if !h.p.Registered() || h.p.reg.others != 1 {
		t.Fatalf("registered %v, others %d", h.p.Registered(), h.p.reg.others)
	}
	if d := time.Until(h.p.reg.renewAt); d < 90*time.Second || d > 100*time.Second {
		t.Errorf("renew in %v (the other device's expiry?)", d)
	}
	h.p.reg.renewAt = time.Time{} // the renewal is due
	h.registerWith(t, ownContact(120))
	if h.p.reg.others != 0 {
		t.Errorf("others %d after the other one left", h.p.reg.others)
	}
}

func secondaryRinging(t *testing.T) (*harness, *net.UDPAddr) {
	t.Helper()
	h := newHarness(t, nil)
	h.p.SetSecondary(true)
	h.registerWith(t, "<sip:jarvis@192.0.2.7:5060>;expires=300", ownContact(300))
	from := h.reg.LocalAddr().(*net.UDPAddr)
	h.p.onRequest(invite("s1", "**610"), from)
	if m := h.read(t); m.Status != 100 {
		t.Fatalf("expected 100: %+v", m)
	}
	if m := h.read(t); m.Status != 180 {
		t.Fatalf("expected 180: %+v", m)
	}
	if len(h.sess.started) != 0 || !h.p.LineRinging() {
		t.Fatalf("a secondary device must ring along, not answer (started %d)", len(h.sess.started))
	}
	return h, from
}

func TestSecondaryRingsAlongAndTheButtonTakesTheCall(t *testing.T) {
	h, _ := secondaryRinging(t)
	h.p.updateCall(time.Now())
	h.expectNothing(t)
	time.Sleep(50 * time.Millisecond) // the tones play in a goroutine
	h.speaker.mu.Lock()
	tones := h.speaker.tones
	h.speaker.mu.Unlock()
	if tones != 2 {
		t.Errorf("%d tones while ringing", tones)
	}
	if !h.p.ButtonOwned() {
		t.Fatal("the button does not belong to the ringing call")
	}
	if got := h.p.CallButton(200 * time.Millisecond); got != "Answered here." {
		t.Fatalf("short press: %q", got)
	}
	if m := h.read(t); m.Status != 200 || m.Body == "" {
		t.Fatalf("200 OK with SDP: %+v", m)
	}
	if h.p.call.stage != stageBridge || h.p.Bridge() != "talk" || len(h.sess.started) != 0 {
		t.Errorf("stage %d, bridge %s, sessions %d", h.p.call.stage, h.p.Bridge(), len(h.sess.started))
	}
}

func TestSecondaryCancelledWhenTakenElsewhere(t *testing.T) {
	h, from := secondaryRinging(t)
	h.sess.open = true // a conversation here must go on
	cancel, _ := Parse("CANCEL sip:jarvis@127.0.0.1 SIP/2.0\r\nVia: SIP/2.0/UDP 127.0.0.1:5060;branch=z9hG4bK1\r\n" +
		"From: <sip:**610@127.0.0.1>;tag=a\r\nTo: <sip:jarvis@127.0.0.1>\r\nCall-ID: s1\r\nCSeq: 1 CANCEL\r\n\r\n")
	h.p.onRequest(cancel, from)
	if m := h.read(t); m.Status != 200 {
		t.Fatalf("200 for the CANCEL: %+v", m)
	}
	if m := h.read(t); m.Status != 487 {
		t.Fatalf("487 for the INVITE: %+v", m)
	}
	if h.p.InCall() || h.sess.closed != 0 {
		t.Errorf("in call %v, conversation closed %d", h.p.InCall(), h.sess.closed)
	}
	select {
	case e := <-h.events:
		t.Errorf("a missed call logged here: %v (the primary logs it)", e)
	default:
	}
}

// 22.10.2026: the owner called the landline from the phone paired with this Echo. Its call had the
// room (the bridge) from the dialling on; when the primary answered the landline, this secondary
// stopped ringing along and switched the phone call's bridge off.
func TestTakenElsewhereLeavesTheMobileCallsRoom(t *testing.T) {
	h, from := secondaryRinging(t)
	h.p.mu.Lock()
	h.p.mob.call = "/hfp/voicecall01" // dialling, no audio yet
	h.p.bridge.setMode(modeTalk)
	h.p.mu.Unlock()
	cancel, _ := Parse("CANCEL sip:jarvis@127.0.0.1 SIP/2.0\r\nVia: SIP/2.0/UDP 127.0.0.1:5060;branch=z9hG4bK1\r\n" +
		"From: <sip:**610@127.0.0.1>;tag=a\r\nTo: <sip:jarvis@127.0.0.1>\r\nCall-ID: s1\r\nCSeq: 1 CANCEL\r\n\r\n")
	h.p.onRequest(cancel, from)
	h.read(t)
	h.read(t)
	if h.p.InCall() || h.p.Bridge() != "talk" {
		t.Errorf("in call %v, bridge %s: the phone's call lost the room", h.p.InCall(), h.p.Bridge())
	}
}

func TestSecondaryAnswersWhenNobodyDoes(t *testing.T) {
	h, _ := secondaryRinging(t)
	h.p.updateCall(time.Now().Add(fallbackAnswer + time.Second))
	if len(h.sess.started) != 1 || !h.sess.started[0].Phone {
		t.Fatalf("Jarvis does not answer after %v", fallbackAnswer)
	}
}

func TestSecondaryAloneAnswersAtOnce(t *testing.T) {
	h := newHarness(t, nil)
	h.p.SetSecondary(true)
	h.registerWith(t, ownContact(300)) // the primary is off
	h.p.onRequest(invite("s2", "**610"), h.reg.LocalAddr().(*net.UDPAddr))
	h.read(t)
	h.read(t)
	if len(h.sess.started) != 1 || h.p.LineRinging() {
		t.Fatal("a secondary device alone on the account must answer as the primary")
	}
}

func TestSecondaryLongPressStopsRingingHere(t *testing.T) {
	h, _ := secondaryRinging(t)
	if got := h.p.CallButton(1500 * time.Millisecond); got != "Not taken here." {
		t.Fatalf("long press: %q", got)
	}
	if m := h.read(t); m.Status != 486 {
		t.Fatalf("486: %+v", m)
	}
	if h.p.InCall() {
		t.Error("still ringing")
	}
}
