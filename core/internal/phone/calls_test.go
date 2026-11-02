package phone

import (
	"net"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"echo-assistant/internal/config"
	"echo-assistant/internal/live"
	"echo-assistant/internal/people"
	"echo-assistant/internal/store"
	"echo-assistant/internal/tools"
)

func TestParseMediaAndSDP(t *testing.T) {
	offer := "v=0\r\no=- 1 1 IN IP4 192.0.2.1\r\ns=-\r\nc=IN IP4 192.0.2.1\r\nt=0 0\r\n" +
		"m=audio 7078 RTP/AVP 9 0 8 101\r\na=rtpmap:9 G722/8000\r\na=rtpmap:8 PCMA/8000\r\n" +
		"a=rtpmap:101 telephone-event/8000\r\na=fmtp:101 0-16\r\n"
	m, ok := ParseMedia(offer)
	if !ok || m.IP.String() != "192.0.2.1" || m.Port != 7078 || m.Payload != 8 || m.DTMF != 101 {
		t.Fatalf("media %+v %v", m, ok)
	}
	// µ-law only, no keys.
	m, ok = ParseMedia("c=IN IP4 192.0.2.5\r\nm=audio 4000 RTP/AVP 0\r\n")
	if !ok || m.Payload != 0 || m.DTMF != -1 || m.Addr().String() != "192.0.2.5:4000" {
		t.Fatalf("media %+v %v", m, ok)
	}
	// telephone-event described but not offered in the m= line: not used.
	if m, _ = ParseMedia("c=IN IP4 192.0.2.5\r\nm=audio 4000 RTP/AVP 8\r\na=rtpmap:101 telephone-event/8000\r\n"); m.DTMF != -1 {
		t.Errorf("dtmf %d", m.DTMF)
	}
	for _, bad := range []string{"", "m=audio 4000 RTP/AVP 9\r\nc=IN IP4 192.0.2.5\r\n", "m=audio 4000 RTP/AVP 8\r\n",
		"c=IN IP4 0.0.0.0\r\nm=audio 4000 RTP/AVP 8\r\n", "c=IN IP4 192.0.2.5\r\nm=audio 0 RTP/AVP 8\r\n"} {
		if _, ok := ParseMedia(bad); ok {
			t.Errorf("%q accepted", bad)
		}
	}

	answer := BuildAnswer("192.0.2.10", 40000, 99, 8, 101)
	want := "v=0\r\no=assistant 99 1 IN IP4 192.0.2.10\r\ns=assistant\r\nc=IN IP4 192.0.2.10\r\nt=0 0\r\nm=audio 40000 RTP/AVP 8 101\r\n" +
		"a=rtpmap:8 PCMA/8000\r\na=rtpmap:101 telephone-event/8000\r\na=fmtp:101 0-16\r\na=ptime:20\r\na=sendrecv\r\n"
	if answer != want {
		t.Errorf("answer:\n%q\nwant\n%q", answer, want)
	}
	if plain := BuildAnswer("192.0.2.10", 40000, 99, 0, -1); strings.Contains(plain, "telephone") || !strings.Contains(plain, "RTP/AVP 0\r\na=rtpmap:0 PCMU/8000") {
		t.Errorf("plain answer %q", plain)
	}
	// Our own offer parses: A-law first, keys 101.
	if m, ok := ParseMedia(BuildOffer("192.0.2.10", 40000, 1)); !ok || m.Payload != 8 || m.DTMF != 101 || m.Port != 40000 {
		t.Errorf("offer %+v %v", m, ok)
	}
}

// --- A fake session and registrar ---------------------------------------------------------

type fakeSession struct {
	mu          sync.Mutex
	open        bool
	state       live.State
	req         live.Request
	started     []live.Request
	commentary  []string
	closed      int
	fed         int
	paused      bool
	pauses      []bool
	endSoon     int
	refuseStart bool
}

func (f *fakeSession) Start(r live.Request) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.open || f.refuseStart {
		return false
	}
	f.open, f.state, f.req = true, live.Connecting, r
	f.started = append(f.started, r)
	return true
}
func (f *fakeSession) FeedLine([]int16) { f.mu.Lock(); f.fed++; f.mu.Unlock() }
func (f *fakeSession) Close() {
	f.mu.Lock()
	f.closed++
	f.state = live.Closing
	f.mu.Unlock()
}
func (f *fakeSession) Commentary(s string) {
	f.mu.Lock()
	f.commentary = append(f.commentary, s)
	f.mu.Unlock()
}
func (f *fakeSession) Open() bool    { f.mu.Lock(); defer f.mu.Unlock(); return f.open }
func (f *fakeSession) OnPhone() bool { f.mu.Lock(); defer f.mu.Unlock(); return f.open && f.req.Phone }
func (f *fakeSession) Request() live.Request {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.req
}
func (f *fakeSession) State() live.State {
	f.mu.Lock()
	defer f.mu.Unlock()
	if !f.open {
		return live.Idle
	}
	return f.state
}
func (f *fakeSession) SetPaused(on bool) {
	f.mu.Lock()
	f.paused = on
	f.pauses = append(f.pauses, on)
	f.mu.Unlock()
}
func (f *fakeSession) Paused() bool { f.mu.Lock(); defer f.mu.Unlock(); return f.paused }
func (f *fakeSession) EndSoon()     { f.mu.Lock(); f.endSoon++; f.mu.Unlock() }

func (f *fakeSession) set(s live.State) { f.mu.Lock(); f.state = s; f.mu.Unlock() }
func (f *fakeSession) end()             { f.mu.Lock(); f.open, f.state = false, live.Idle; f.mu.Unlock() }

// harness: a Phone whose SIP socket and "registrar" are local sockets.
type harness struct {
	p       *Phone
	sess    *fakeSession
	speaker *fakeSpeaker
	busy    bool         // a conversation at the speaker runs (HomeBusy)
	reg     *net.UDPConn // the registrar / the other side
	events  chan [4]string
	runs    chan [5]string
}

func newHarness(t *testing.T, secrets config.Secrets) *harness {
	t.Helper()
	rtpPort = freePort(t)
	reg, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatal(err)
	}
	own, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { reg.Close(); own.Close() })
	h := &harness{sess: &fakeSession{}, speaker: &fakeSpeaker{}, reg: reg,
		events: make(chan [4]string, 8), runs: make(chan [5]string, 8)}
	if secrets == nil {
		secrets = config.Secrets{}
	}
	secrets["SIP_USER"], secrets["SIP_PASSWORD"], secrets["SIP_REGISTRAR"] = "jarvis", "secret", "127.0.0.1"
	store.Dir = t.TempDir()
	h.p = New(secrets, people.New(secrets), h.sess, Deps{
		LogEvent: func(person, channel, title, summary string) { h.events <- [4]string{person, channel, title, summary} },
		RunNow: func(kind, title, text, deliver, person string) {
			h.runs <- [5]string{kind, title, text, deliver, person}
		},
		CallerName: func(number string) string { return "" },
		Speaker:    h.speaker,
		HomeBusy:   func() bool { return h.busy },
	})
	h.p.conn, h.p.localIP, h.p.regAddr = own, "127.0.0.1", reg.LocalAddr().(*net.UDPAddr)
	return h
}

func freePort(t *testing.T) int {
	c, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	return c.LocalAddr().(*net.UDPAddr).Port
}

func (h *harness) read(t *testing.T) *Message {
	t.Helper()
	buf := make([]byte, 4096)
	h.reg.SetReadDeadline(time.Now().Add(2 * time.Second))
	n, _, err := h.reg.ReadFromUDP(buf)
	if err != nil {
		t.Fatalf("nothing sent: %v", err)
	}
	m, ok := Parse(string(buf[:n]))
	if !ok {
		t.Fatalf("not SIP: %q", buf[:n])
	}
	return m
}

func (h *harness) expectNothing(t *testing.T) {
	t.Helper()
	h.reg.SetReadDeadline(time.Now().Add(100 * time.Millisecond))
	if n, _, err := h.reg.ReadFromUDP(make([]byte, 4096)); err == nil {
		t.Fatalf("unexpected packet of %d bytes", n)
	}
}

func (h *harness) register(t *testing.T) {
	t.Helper()
	h.p.updateRegistration(time.Now())
	m := h.read(t)
	if m.Method != "REGISTER" || m.Header("authorization") != "" {
		t.Fatalf("first REGISTER: %+v", m)
	}
	challenge := "SIP/2.0 401 Unauthorized\r\nVia: " + m.Header("via") + "\r\nCall-ID: " + m.Header("call-id") + "\r\nCSeq: " +
		m.Header("cseq") + "\r\nWWW-Authenticate: Digest realm=\"fritz.box\", nonce=\"n1\", qop=\"auth\"\r\nContent-Length: 0\r\n\r\n"
	resp, _ := Parse(challenge)
	h.p.onResponse(resp)
	m = h.read(t)
	auth := m.Header("authorization")
	if m.Method != "REGISTER" || !strings.Contains(auth, `username="jarvis"`) || !strings.Contains(auth, "response=") {
		t.Fatalf("second REGISTER: %+v", m)
	}
	ok, _ := Parse("SIP/2.0 200 OK\r\nCall-ID: " + m.Header("call-id") + "\r\nCSeq: " + m.Header("cseq") + "\r\nContact: <sip:jarvis@127.0.0.1>;expires=120\r\n\r\n")
	h.p.onResponse(ok)
}

func TestRegistrationWithDigest(t *testing.T) {
	h := newHarness(t, nil)
	if h.p.Registered() {
		t.Fatal("registered before")
	}
	h.register(t)
	if !h.p.Registered() {
		t.Fatal("not registered")
	}
	// The expires of the contact set the renewal (80 %).
	if d := time.Until(h.p.reg.renewAt); d < 90*time.Second || d > 100*time.Second {
		t.Errorf("renew in %v", d)
	}
	h.p.updateRegistration(time.Now())
	h.expectNothing(t)
	h.p.updateRegistration(time.Now().Add(97 * time.Second)) // time to renew
	if m := h.read(t); m.Method != "REGISTER" {
		t.Errorf("renewal %+v", m)
	}
	// A wrong password: the second 401 is final, retry later.
	h2 := newHarness(t, nil)
	h2.p.updateRegistration(time.Now())
	m := h2.read(t)
	resp, _ := Parse("SIP/2.0 401 Unauthorized\r\nCall-ID: " + m.Header("call-id") + "\r\nCSeq: " + m.Header("cseq") +
		"\r\nWWW-Authenticate: Digest realm=\"r\", nonce=\"n\"\r\n\r\n")
	h2.p.onResponse(resp)
	m = h2.read(t)
	resp, _ = Parse("SIP/2.0 401 Unauthorized\r\nCall-ID: " + m.Header("call-id") + "\r\nCSeq: " + m.Header("cseq") +
		"\r\nWWW-Authenticate: Digest realm=\"r\", nonce=\"n2\"\r\n\r\n")
	h2.p.onResponse(resp)
	h2.expectNothing(t)
	if h2.p.Registered() || !h2.p.reg.renewAt.After(time.Now().Add(30*time.Second)) {
		t.Error("refused registration not handled")
	}
}

func invite(callID, from string) *Message {
	m, _ := Parse("INVITE sip:jarvis@127.0.0.1 SIP/2.0\r\nVia: SIP/2.0/UDP 127.0.0.1:5060;branch=z9hG4bK1\r\nFrom: <sip:" + from +
		"@127.0.0.1>;tag=a\r\nTo: <sip:jarvis@127.0.0.1>\r\nCall-ID: " + callID + "\r\nCSeq: 1 INVITE\r\n" +
		"Contact: <sip:" + from + "@127.0.0.1:5060>\r\nContent-Type: application/sdp\r\nContent-Length: 0\r\n\r\n")
	m.Body = "v=0\r\nc=IN IP4 127.0.0.1\r\nm=audio 9 RTP/AVP 8 101\r\na=rtpmap:101 telephone-event/8000\r\n"
	return m
}

func TestIncomingCallFromTheHousehold(t *testing.T) {
	h := newHarness(t, nil)
	from := h.reg.LocalAddr().(*net.UDPAddr)
	h.p.onRequest(invite("c1", "**610"), from)
	if m := h.read(t); m.Status != 100 {
		t.Fatalf("expected 100 Trying: %+v", m)
	}
	if m := h.read(t); m.Status != 180 || !strings.Contains(m.Header("to"), ";tag=") {
		t.Fatalf("expected 180 Ringing with tag: %+v", m)
	}
	if len(h.sess.started) != 1 {
		t.Fatal("session not started")
	}
	r := h.sess.started[0]
	if !r.Phone || r.Stranger || r.Reason != "" || r.Caller != "**610" || r.Line == nil {
		t.Errorf("request %+v", r)
	}
	// Still ringing while GPT Live starts; a second call is busy.
	h.p.updateCall(time.Now())
	h.expectNothing(t)
	h.sess.set(live.Connecting)
	h.p.onRequest(invite("c2", "**611"), from)
	if m := h.read(t); m.Status != 486 {
		t.Fatalf("expected 486: %+v", m)
	}
	// The session is up: answer with SDP.
	h.sess.set(live.Live)
	h.p.updateCall(time.Now())
	ok := h.read(t)
	if ok.Status != 200 || !strings.Contains(ok.Body, "m=audio "+itoa(rtpPort)+" RTP/AVP 8 101") {
		t.Fatalf("200 OK: %+v", ok)
	}
	if h.p.call.stage != stageTalking || !r.Line.(*Line).Active() {
		t.Error("not talking")
	}
	// The 200 OK is repeated until the ACK.
	h.p.updateCall(time.Now().Add(600 * time.Millisecond))
	if m := h.read(t); m.Status != 200 {
		t.Errorf("retransmission %+v", m)
	}
	ack, _ := Parse("ACK sip:jarvis@127.0.0.1 SIP/2.0\r\nCall-ID: c1\r\nCSeq: 1 ACK\r\n\r\n")
	h.p.onRequest(ack, from)
	if !h.p.call.acked {
		t.Error("ack")
	}
	// A conversation that ends: BYE.
	h.sess.end()
	h.p.updateCall(time.Now())
	bye := h.read(t)
	if bye.Method != "BYE" || bye.Header("call-id") != "c1" {
		t.Fatalf("BYE %+v", bye)
	}
	if h.p.InCall() || r.Line.(*Line).Active() {
		t.Error("call not cleared")
	}
	// No report for a household caller.
	if len(h.p.reports) != 0 {
		t.Error("report for the household")
	}
}

func TestGuestCallGetsAReport(t *testing.T) {
	h := newHarness(t, config.Secrets{"PHONE_CALLERS": "0171 1234567"})
	from := h.reg.LocalAddr().(*net.UDPAddr)
	h.p.onRequest(invite("g1", "03012345678"), from)
	h.read(t)
	h.read(t)
	r := h.sess.started[0]
	if !r.Stranger || r.Person != "" {
		t.Fatalf("guest request %+v", r)
	}
	h.sess.set(live.Live)
	h.p.updateCall(time.Now())
	h.read(t) // 200
	h.p.updateCall(time.Now())
	if len(h.sess.commentary) != 0 {
		t.Errorf("a guest call must not be greeted by phone: %v", h.sess.commentary)
	}
	// The caller hangs up.
	bye, _ := Parse("BYE sip:jarvis@127.0.0.1 SIP/2.0\r\nVia: SIP/2.0/UDP 127.0.0.1:5060\r\nFrom: <sip:x@127.0.0.1>;tag=a\r\nTo: <sip:jarvis@127.0.0.1>;tag=b\r\nCall-ID: g1\r\nCSeq: 2 BYE\r\n\r\n")
	h.p.onRequest(bye, from)
	if m := h.read(t); m.Status != 200 {
		t.Fatalf("BYE answer %+v", m)
	}
	if h.sess.closed != 1 || len(h.p.reports) != 1 {
		t.Fatalf("closed %d, reports %d", h.sess.closed, len(h.p.reports))
	}
	// The report waits for the transcript, then goes to the agent.
	h.sess.end()
	h.p.reportIfDue(time.Now())
	select {
	case run := <-h.runs:
		t.Fatalf("report without transcript: %v", run)
	default:
	}
	h.p.mu.Lock()
	h.p.mu.Unlock()
	h.p.finished[r.Line.(*Line)] = finishedCall{ended: live.Ended{Request: r, Transcript: "Gegenüber: hallo"}, at: time.Now()}
	h.p.reportIfDue(time.Now())
	select {
	case run := <-h.runs:
		if run[0] != "prompt" || run[1] != "Anruf von 03012345678" || !strings.Contains(run[2], "Gegenüber: hallo") ||
			!strings.Contains(run[2], "Number: 03012345678") || run[3] != "call" { // the admin of the test has neither Telegram nor mail
			t.Errorf("report %q", run)
		}
	case <-time.After(time.Second):
		t.Fatal("no report")
	}
	if len(h.p.reports) != 0 {
		t.Error("report kept")
	}
}

func TestReportWithoutTranscriptAfterGrace(t *testing.T) {
	h := newHarness(t, nil)
	line := NewLine(rtpPort, nil, nil)
	h.p.reports = []*pendingReport{{line: line, number: "0301", task: "book", since: time.Now()}}
	now := time.Now()
	h.p.reportIfDue(now)
	h.p.reportIfDue(now.Add(reportGrace + time.Second))
	select {
	case run := <-h.runs:
		if run[1] != "Anruf bei 0301" || !strings.Contains(run[2], "Task: book") {
			t.Errorf("report %q", run)
		}
	case <-time.After(time.Second):
		t.Fatal("no report")
	}
}

func TestPinCall(t *testing.T) {
	h := newHarness(t, config.Secrets{"PHONE_PIN": "12"})
	from := h.reg.LocalAddr().(*net.UDPAddr)
	h.p.onRequest(invite("p1", "**610"), from)
	h.read(t)
	h.read(t)
	if len(h.sess.started) != 0 || h.p.call.stage != stagePin {
		t.Fatal("PIN stage expected before the session")
	}
	if m := h.read(t); m.Status != 200 {
		t.Fatalf("answered first: %+v", m)
	}
	line := h.p.call.line
	// The caller types 1 2 # (RFC 4733 end packets).
	to := &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1), Port: rtpPort}
	for i, key := range []int{1, 2, 11} {
		h.reg.WriteToUDP(buildRTP(101, false, uint16(i), uint32(1000*(i+1)), 3, eventPayload(key, true, 800)), to)
	}
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) && len(line.digits) < 3 {
		time.Sleep(10 * time.Millisecond)
	}
	h.p.updateCall(time.Now())
	if len(h.sess.started) != 1 || h.p.call.stage != stageConnecting {
		t.Fatalf("session after the PIN: %d, stage %d", len(h.sess.started), h.p.call.stage)
	}
	// A wrong PIN hangs up.
	h.sess.end()
	h.p.updateCall(time.Now())
	h.read(t) // BYE
	h.p.onRequest(invite("p2", "**610"), from)
	h.read(t)
	h.read(t)
	h.read(t) // 200
	line = h.p.call.line
	for i, key := range []int{9, 11} {
		h.reg.WriteToUDP(buildRTP(101, false, uint16(i), uint32(5000*(i+1)), 3, eventPayload(key, true, 800)), to)
	}
	deadline = time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) && len(line.digits) < 2 {
		time.Sleep(10 * time.Millisecond)
	}
	h.p.updateCall(time.Now())
	if m := h.read(t); m.Method != "BYE" || h.p.InCall() {
		t.Errorf("wrong PIN: %+v", m)
	}
}

func TestOutgoingCallToAPerson(t *testing.T) {
	h := newHarness(t, config.Secrets{"PHONE_OWNER": "0171 1234567"})
	h.p.reg.registered = true
	out := h.p.placeCall("", "wake up", tools.Context{Channel: tools.Agent})
	if !strings.HasPrefix(out, "Calling") || !strings.Contains(out, "01711234567") {
		t.Fatalf("tool result %q", out)
	}
	h.p.dialIfRequested(time.Now())
	inv := h.read(t)
	if inv.Method != "INVITE" || !strings.Contains(inv.Body, "RTP/AVP 8 0 101") || inv.URI != "sip:01711234567@127.0.0.1" {
		t.Fatalf("INVITE %+v", inv)
	}
	if len(h.sess.started) != 1 || h.sess.started[0].Reason != "wake up" || !h.sess.started[0].Phone {
		t.Fatalf("session %+v", h.sess.started)
	}
	// 401: the INVITE again with the password and CSeq 2, after an ACK of the failure.
	resp := func(status int, reason, extra string, cseq int) *Message {
		m, _ := Parse("SIP/2.0 " + itoa(status) + " " + reason + "\r\nCall-ID: " + inv.Header("call-id") + "\r\nFrom: " + inv.Header("from") +
			"\r\nTo: " + inv.Header("to") + ";tag=r\r\nCSeq: " + itoa(cseq) + " INVITE\r\n" + extra + "Content-Length: 0\r\n\r\n")
		return m
	}
	h.p.onResponse(resp(401, "Unauthorized", "WWW-Authenticate: Digest realm=\"r\", nonce=\"n\"\r\n", 1))
	if ack := h.read(t); ack.Method != "ACK" {
		t.Fatalf("ACK %+v", ack)
	}
	again := h.read(t)
	if again.Method != "INVITE" || again.Header("authorization") == "" || again.CSeqNumber() != 2 {
		t.Fatalf("authenticated INVITE %+v", again)
	}
	h.p.onResponse(resp(180, "Ringing", "", 2))
	if !h.p.call.ringing {
		t.Error("ringing")
	}
	// Picked up.
	answer := resp(200, "OK", "Contact: <sip:x@127.0.0.1:5060>\r\nContent-Type: application/sdp\r\n", 2)
	answer.Body = "v=0\r\nc=IN IP4 127.0.0.1\r\nm=audio 9 RTP/AVP 8\r\n"
	h.p.onResponse(answer)
	if ack := h.read(t); ack.Method != "ACK" || ack.CSeqNumber() != 2 {
		t.Fatalf("ACK of the 200 %+v", ack)
	}
	if h.p.call.stage != stageTalking || !h.p.call.line.Active() {
		t.Fatal("not talking")
	}
	h.p.onResponse(answer) // the 200 again: our ACK again
	if ack := h.read(t); ack.Method != "ACK" {
		t.Fatalf("repeated ACK %+v", ack)
	}
	// Once the session is live the assistant says why it calls.
	h.sess.set(live.Live)
	h.p.updateCall(time.Now())
	if len(h.sess.commentary) != 1 {
		t.Fatalf("commentary %v", h.sess.commentary)
	}
	// press_keys works on this call.
	h.sess.mu.Lock()
	h.sess.req.Phone = true
	h.sess.mu.Unlock()
	if got := h.p.pressKeys("1 2#"); got != "Pressed 12#." {
		t.Errorf("press_keys %q", got)
	}
	if got := h.p.pressKeys("a"); !strings.HasPrefix(got, "Error") {
		t.Errorf("press_keys a: %q", got)
	}
	// The user (the person called is the asker's own: no report) hangs up.
	bye, _ := Parse("BYE sip:jarvis@127.0.0.1 SIP/2.0\r\nVia: SIP/2.0/UDP 127.0.0.1:5060\r\nFrom: <sip:x@127.0.0.1>;tag=r\r\nTo: " +
		inv.Header("from") + "\r\nCall-ID: " + inv.Header("call-id") + "\r\nCSeq: 1 BYE\r\n\r\n")
	h.p.onRequest(bye, h.reg.LocalAddr().(*net.UDPAddr))
	if m := h.read(t); m.Status != 200 || h.p.InCall() {
		t.Errorf("after BYE: %+v", m)
	}
}

func TestOutgoingCallNobodyAnswers(t *testing.T) {
	h := newHarness(t, nil)
	h.p.reg.registered = true
	h.p.dial(&callRequest{number: "0301234567", reason: "x", stranger: true, at: time.Now()})
	inv := h.read(t)
	h.p.updateCall(time.Now().Add(ringTimeout + time.Second))
	cancel := h.read(t)
	if cancel.Method != "CANCEL" || cancel.CSeqNumber() != 1 || cancel.Header("via") != inv.Header("via") {
		t.Fatalf("CANCEL %+v", cancel)
	}
	if h.sess.closed != 1 {
		t.Error("session not closed")
	}
	select {
	case ev := <-h.events:
		if ev[1] != "call_for" {
			t.Errorf("event %v", ev)
		}
	case <-time.After(time.Second):
		t.Fatal("failed call not logged")
	}
	// The 487 gets its ACK.
	m, _ := Parse("SIP/2.0 487 Request Terminated\r\nCall-ID: " + inv.Header("call-id") + "\r\nTo: " + inv.Header("to") + ";tag=r\r\nCSeq: 1 INVITE\r\n\r\n")
	h.p.onResponse(m)
	if ack := h.read(t); ack.Method != "ACK" {
		t.Fatalf("ACK %+v", ack)
	}
	if h.p.call.callID != "" {
		t.Error("call kept")
	}
}

func TestWaitingCall(t *testing.T) {
	h := newHarness(t, nil)
	h.sess.open, h.sess.state = true, live.Live // a conversation at home
	from := h.reg.LocalAddr().(*net.UDPAddr)
	h.p.onRequest(invite("w1", "**610"), from)
	h.read(t)
	h.read(t)
	if h.p.call.stage != stageWaiting {
		t.Fatal("not waiting")
	}
	h.p.updateCall(time.Now())
	if len(h.sess.commentary) != 1 || !strings.Contains(h.sess.commentary[0], "calling on the phone") {
		t.Errorf("commentary %v", h.sess.commentary)
	}
	// The caller gives up: 487 and a note for the activity log.
	cancel, _ := Parse("CANCEL sip:jarvis@127.0.0.1 SIP/2.0\r\nVia: SIP/2.0/UDP 127.0.0.1:5060\r\nFrom: <sip:**610@127.0.0.1>;tag=a\r\nTo: <sip:jarvis@127.0.0.1>\r\nCall-ID: w1\r\nCSeq: 1 CANCEL\r\n\r\n")
	h.p.onRequest(cancel, from)
	if m := h.read(t); m.Status != 200 {
		t.Fatalf("CANCEL answer %+v", m)
	}
	if m := h.read(t); m.Status != 487 {
		t.Fatalf("487 %+v", m)
	}
	if h.sess.closed != 0 {
		t.Error("the conversation at home was closed")
	}
	select {
	case ev := <-h.events:
		if ev[1] != "phone_in" || ev[2] != "Verpasster Anruf" {
			t.Errorf("event %v", ev)
		}
	case <-time.After(time.Second):
		t.Fatal("missed call not logged")
	}
	// A waiting call is answered when the conversation is over.
	h.sess.end()
	h.p.onRequest(invite("w2", "**610"), from)
	h.read(t)
	h.read(t)
	if len(h.sess.started) != 1 {
		t.Fatal("not picked up at once")
	}
}

func TestPlaceCallRules(t *testing.T) {
	h := newHarness(t, nil)
	ctx := tools.Context{Channel: tools.Voice}
	if got := h.p.placeCall("0301234567", "reserve a table for two", ctx); !strings.Contains(got, "not registered") {
		t.Errorf("unregistered: %q", got)
	}
	h.p.reg.registered = true
	for to, want := range map[string]string{
		"0900 123456 7": "premium", "+41 44 1234567": "German", "123": "complete phone number", "0301234567": "describe the task",
	} {
		if got := h.p.placeCall(to, "short", ctx); !strings.HasPrefix(got, "Error") || !strings.Contains(got, want) {
			t.Errorf("%s: %q", to, got)
		}
	}
	// A person without a number.
	if got := h.p.placeCall("", "wake up", ctx); !strings.HasPrefix(got, "Error: no phone number registered") {
		t.Errorf("no number: %q", got)
	}
	got := h.p.placeCall("0301234567", "reserve a table for two at eight", ctx)
	if !strings.HasPrefix(got, "Calling 0301234567 on the user's behalf. It rings in a moment. The result is reported afterwards by Telegram.") {
		t.Errorf("stranger: %q", got)
	}
	if h.p.wanted == nil || !h.p.wanted.stranger {
		t.Error("not queued")
	}
	// A running conversation delays it.
	h.sess.open = true
	if got := h.p.request("0301234567", "x", true, "", ""); !strings.Contains(got, "once the current conversation is over") {
		t.Errorf("busy: %q", got)
	}
	h.p.dialIfRequested(time.Now())
	h.expectNothing(t)
	h.p.dialIfRequested(time.Now().Add(callWait + time.Minute))
	if h.p.wanted != nil {
		t.Error("stale request kept")
	}
}

func TestToolsAreRegistered(t *testing.T) {
	h := newHarness(t, nil)
	var r tools.Registry
	h.p.AddTools(&r)
	names := strings.Join(r.Names(), ",")
	if !strings.Contains(names, "phone_call") || !strings.Contains(names, "press_keys") || strings.Contains(names, "end_call") {
		t.Errorf("tools %s", names)
	}
	if got := len(r.Describe(tools.Context{Channel: tools.Stranger})); got != 2 && got != 1 {
		t.Errorf("stranger tools %d", got)
	}
	// Not registered at the FritzBox: a clear error.
	if got := r.Call("phone_call", `{"to":"","task":"x"}`, tools.Context{Channel: tools.Voice}); !strings.Contains(got, "not registered") {
		t.Errorf("phone_call: %q", got)
	}
	if got := r.Call("press_keys", `{"keys":"1"}`, tools.Context{Channel: tools.Voice}); got != "Error: no phone call is running." {
		t.Errorf("press_keys: %q", got)
	}
	// Without SIP credentials nothing is registered.
	var empty tools.Registry
	New(config.Secrets{}, &people.Register{}, h.sess, Deps{}).AddTools(&empty)
	if len(empty.Names()) != 0 {
		t.Error("tools without a configured phone")
	}
}

func itoa(n int) string { return strconv.Itoa(n) }
