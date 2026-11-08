package phone

import (
	"net"
	"strings"
	"sync"
	"testing"
	"time"

	"echo-assistant/internal/config"
	"echo-assistant/internal/live"
	"echo-assistant/internal/tools"
)

// fakeSpeaker: the speaker at home, as audio.Speaker is for the real one.
type fakeSpeaker struct {
	mu      sync.Mutex
	played  []int16
	tones   int
	freqs   []float64
	flushes int
	queued  time.Duration // what the real one would still hold
}

func (f *fakeSpeaker) Queue(pcm []int16) error {
	f.mu.Lock()
	f.played = append(f.played, pcm...)
	f.mu.Unlock()
	return nil
}

func (f *fakeSpeaker) Tone(freq float64, ms int, amp float64) {
	f.mu.Lock()
	f.tones++
	f.freqs = append(f.freqs, freq)
	f.mu.Unlock()
}

func (f *fakeSpeaker) Queued() time.Duration {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.queued
}

func (f *fakeSpeaker) Flush() {
	f.mu.Lock()
	f.played, f.flushes = nil, f.flushes+1
	f.mu.Unlock()
}

// rang: how often a tone of this frequency was played.
func (f *fakeSpeaker) rang(freq float64) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	n := 0
	for _, got := range f.freqs {
		if got == freq {
			n++
		}
	}
	return n
}

func (f *fakeSpeaker) peak() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	peak := 0
	for _, v := range f.played {
		peak = max(peak, int(v), -int(v))
	}
	return peak
}

func (f *fakeSpeaker) toneCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.tones
}

// waitFor: the tones are played in a goroutine (writing to the speaker may block, the phone's
// loop must not), so wait for them instead of assuming they are already there.
func waitFor(t *testing.T, what string, ok func() bool) {
	t.Helper()
	for deadline := time.Now().Add(2 * time.Second); time.Now().Before(deadline); {
		if ok() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Error("waited in vain for " + what)
}

// constant: n samples of one value, to tell the two directions apart in the mix.
func constant(v int16, n int) []int16 {
	pcm := make([]int16, n)
	for i := range pcm {
		pcm[i] = v
	}
	return pcm
}

func TestMicResamplerTo24kHz(t *testing.T) {
	var m MicResampler
	// 20 ms of microphone audio become 20 ms at 24 kHz, packet after packet.
	for i := 0; i < 3; i++ {
		if out := m.Process(sine(1000, micRate, micFrame, 8000)); len(out) != apiFrame {
			t.Fatalf("20 ms of 16 kHz gave %d samples at 24 kHz, want %d", len(out), apiFrame)
		}
	}
	// A 1 kHz tone keeps its level (the filter's gain is 1 in the telephone band); the first
	// packet still fills the filter, so measure a later one.
	var m2 MicResampler
	m2.Process(sine(1000, micRate, micFrame, 8000))
	out := m2.Process(sine(1000, micRate, micFrame, 8000))
	peak := 0
	for _, v := range out {
		peak = max(peak, int(v), -int(v))
	}
	if peak < 6500 || peak > 9500 {
		t.Errorf("1 kHz at 8000 came out with peak %d", peak)
	}
	// Above the telephone band nothing is left (it would fold back on the line).
	var m3 MicResampler
	m3.Process(sine(6000, micRate, micFrame, 8000))
	high := 0
	for _, v := range m3.Process(sine(6000, micRate, micFrame, 8000)) {
		high = max(high, int(v), -int(v))
	}
	if high > 800 {
		t.Errorf("6 kHz is still there with peak %d", high)
	}
}

func TestBridgeMixesTheCallAndAssistant(t *testing.T) {
	speaker := &fakeSpeaker{}
	busy := false
	b := newBridge(speaker, func() bool { return busy })

	// Off: everything is dropped and nothing is played.
	b.Call(constant(1000, apiFrame))
	b.Assistant(constant(500, apiFrame))
	if frame := b.frame(); frame != nil {
		t.Fatal("a frame while the bridge is off")
	}

	b.setMode(modeListen)
	// Less than the prefill: nothing yet (the room would hear chopped audio).
	b.Call(constant(1000, bridgePrefill/2))
	if frame := b.frame(); frame != nil {
		t.Fatal("played before the prefill was there")
	}
	// The other side and the assistant together, sample by sample.
	b.Call(constant(1000, apiFrame))
	b.Assistant(constant(500, apiFrame))
	frame := b.frame()
	if len(frame) != apiFrame {
		t.Fatalf("frame of %d samples", len(frame))
	}
	if frame[0] != 1500 {
		t.Errorf("mixed to %d, want 1500", frame[0])
	}
	// A conversation at the speaker: the call leaves the room alone.
	busy = true
	b.Call(constant(1000, apiFrame))
	if frame := b.frame(); frame != nil {
		t.Error("played into a conversation at the speaker")
	}
	busy = false
	// Switching off clears what is left, so a later call does not start with old audio.
	b.setMode(modeOff)
	b.setMode(modeListen)
	b.Call(constant(1000, bridgePrefill))
	if frame := b.frame(); frame == nil || frame[0] != 1000 {
		t.Errorf("after switching on again: %v", frame != nil)
	}
	// While the announcement plays, the call waits: the speaker takes one source at a time.
	b.setMode(modeOff)
	b.setMode(modeListen)
	b.announce(true)
	b.Call(constant(1000, bridgePrefill))
	if frame := b.frame(); frame != nil {
		t.Error("the call played into the announcement")
	}
	b.mu.Lock()
	b.quietUntil = time.Time{} // the tones are through
	b.mu.Unlock()
	b.Call(constant(1000, bridgePrefill))
	if frame := b.frame(); frame == nil {
		t.Error("the call stayed away after the announcement")
	}
	// Clipping cannot wrap around to the other sign.
	b.setMode(modeOff)
	b.setMode(modeListen)
	b.Call(constant(30000, bridgePrefill))
	b.Assistant(constant(30000, bridgePrefill))
	if frame := b.frame(); frame == nil || frame[0] != 32767 {
		t.Errorf("loud mix gave %v, want 32767", frame)
	}
}

// What piles up while the call waits (the announcement, here 1.4 s of it) must not stay as delay:
// the queue drains exactly as fast as RTP fills it, so the room would hear the call late for the
// rest of it - the first dial-in test heard it about a second late.
func TestBridgeDoesNotKeepABacklog(t *testing.T) {
	b := newBridge(&fakeSpeaker{}, nil)
	b.setMode(modeListen)
	b.announce(true)
	for i := 0; i < 70; i++ { // 1.4 s of RTP while the tones play
		b.Call(constant(1000, apiFrame))
		if frame := b.frame(); frame != nil {
			t.Fatal("the call played into the announcement")
		}
	}
	b.mu.Lock()
	b.quietUntil = time.Time{}
	queued := len(b.call)
	b.mu.Unlock()
	if queued > bridgeLag {
		t.Errorf("%d ms of the call queued after the announcement, want at most %d",
			queued*1000/apiRate, bridgeLag*1000/apiRate)
	}
	// The assistant's voice comes in bursts faster than real time: those are kept.
	b.Assistant(constant(500, apiRate/2))
	b.mu.Lock()
	kept := len(b.assistant)
	b.mu.Unlock()
	if kept != apiRate/2 {
		t.Errorf("kept %d samples of a burst of Jarvis' voice, want %d", kept, apiRate/2)
	}
}

// The speaker holding 1.5 s (a conversation's answer went into the same pipe): the call waits
// until it has caught up instead of running that late for the rest of it.
func TestBridgeLetsTheSpeakerCatchUp(t *testing.T) {
	speaker := &fakeSpeaker{queued: 1500 * time.Millisecond}
	b := newBridge(speaker, nil)
	b.setMode(modeListen)
	t.Cleanup(func() { b.setMode(modeOff) })
	for i := 0; i < 20; i++ {
		b.Call(constant(6000, apiFrame))
		time.Sleep(20 * time.Millisecond)
	}
	if speaker.peak() != 0 {
		t.Fatal("the call went into a speaker 1.5 s behind")
	}
	speaker.mu.Lock()
	speaker.queued = 0
	speaker.mu.Unlock()
	waitFor(t, "the call once the speaker caught up", func() bool {
		b.Call(constant(6000, apiFrame))
		return speaker.peak() == 6000
	})
}

func TestBridgePlaysEvery20ms(t *testing.T) {
	speaker := &fakeSpeaker{}
	b := newBridge(speaker, nil)
	b.setMode(modeListen)
	t.Cleanup(func() { b.setMode(modeOff) })
	for i := 0; i < 10; i++ { // 200 ms of call audio
		b.Call(constant(6000, apiFrame))
	}
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) && speaker.peak() == 0 {
		time.Sleep(20 * time.Millisecond)
	}
	if speaker.peak() != 6000 {
		t.Errorf("the room heard a peak of %d, want 6000", speaker.peak())
	}
}

func TestLineAddsTheMicrophoneToTheFrame(t *testing.T) {
	line, far, _, _ := newPair(t, 101)
	// Only the microphone, no voice of the assistant: it goes out without the jitter buffer.
	frames := 5
	for i := 0; i < frames; i++ {
		line.MicIn(sine(1000, micRate, micFrame, 9000))
	}
	loud := false
	for i := 0; i < 20 && !loud; i++ {
		for _, c := range readPacket(t, far).Payload {
			if v := decodeAlaw(c); v > 2000 || v < -2000 {
				loud = true
			}
		}
	}
	if !loud {
		t.Fatal("the microphone at home did not reach the line")
	}
	// It is sent at the rate it arrives: reading as many packets as were fed empties the queue
	// (one frame per packet), so nothing piles up during a long call.
	for i := 0; i < frames+3; i++ {
		readPacket(t, far)
	}
	line.mu.Lock()
	left := len(line.mic)
	line.mu.Unlock()
	if left > apiFrame {
		t.Errorf("%d samples of microphone audio piled up", left)
	}
	// A batch after the microphone's goroutine stalled (half a second at once) keeps only the
	// newest micLag: otherwise the far end would hear the room half a second late from then on.
	line.MicIn(sine(1000, micRate, micRate/2, 9000))
	line.mu.Lock()
	left = len(line.mic)
	line.mu.Unlock()
	if left > micLag {
		t.Errorf("%d ms of microphone audio queued after a stall, want at most %d",
			left*1000/apiRate, micLag*1000/apiRate)
	}
}

// talking: a call that is answered and talking, as a call the assistant makes for the user.
func (h *harness) talking(t *testing.T) *Line {
	t.Helper()
	from := h.reg.LocalAddr().(*net.UDPAddr)
	h.p.onRequest(invite("b1", "**610"), from)
	h.read(t) // 100
	h.read(t) // 180
	h.sess.set(live.Live)
	h.p.updateCall(time.Now())
	h.read(t) // 200 OK
	if h.p.call.stage != stageTalking {
		t.Fatalf("stage %v", h.p.call.stage)
	}
	return h.p.call.line
}

func TestListeningInAndTalkingAlong(t *testing.T) {
	h := newHarness(t, nil)
	h.talking(t)
	// Listening in: the call goes to the speaker, the microphone stays off, and the other side is
	// told that someone is listening (§ 201 StGB).
	if answer := h.p.SetBridge("listen"); !strings.Contains(answer, "listen") {
		t.Fatalf("listen: %q", answer)
	}
	if h.p.Bridge() != "listen" || !h.p.BridgeActive() {
		t.Fatalf("mode %q", h.p.Bridge())
	}
	if h.speaker.toneCount() == 0 {
		t.Error("the room was not told")
	}
	if len(h.sess.commentary) != 1 || !strings.Contains(h.sess.commentary[0], "§ 201") {
		t.Fatalf("commentary %q", h.sess.commentary)
	}
	if h.sess.Paused() {
		t.Error("Jarvis holds back although only someone is listening in")
	}
	// Talking along: a three-way call. The microphone is on the line, and the assistant stays in the
	// call: not held back, and he hears the room and the other side mixed in one stream.
	h.p.SetBridge("talk")
	if h.p.Bridge() != "talk" || h.sess.Paused() || !h.p.ConferenceActive() {
		t.Fatalf("mode %q, paused %v, three-way %v", h.p.Bridge(), h.sess.Paused(), h.p.ConferenceActive())
	}
	if len(h.sess.commentary) != 2 || !strings.Contains(h.sess.commentary[1], "three-way") {
		t.Fatalf("commentary %q", h.sess.commentary)
	}
	h.p.lineHeard(constant(3000, apiFrame)) // the other side waits for the microphone
	h.sess.mu.Lock()
	h.sess.fed = 0
	h.sess.mu.Unlock()
	mic := sine(1000, micRate, micFrame, 9000)
	h.p.Hear(mic)
	h.p.call.line.mu.Lock()
	queued := len(h.p.call.line.mic)
	h.p.call.line.mu.Unlock()
	if queued == 0 {
		t.Error("the microphone does not reach the line")
	}
	h.sess.mu.Lock()
	fed := h.sess.fed
	h.sess.mu.Unlock()
	if fed != 1 {
		t.Errorf("Jarvis was fed %d times for one microphone frame, want 1 (the mix)", fed)
	}
	// A conversation at the speaker takes the room back: the far end must not hear it.
	h.busy = true
	h.p.Hear(mic)
	h.p.call.line.mu.Lock()
	more := len(h.p.call.line.mic)
	h.p.call.line.mu.Unlock()
	if more != queued {
		t.Error("the microphone reaches the line during a conversation at the speaker")
	}
	h.busy = false
	// The user leaves the call again: The assistant goes on alone, knowing what was said.
	h.p.SetBridge("listen")
	if h.sess.Paused() || h.p.ConferenceActive() {
		t.Errorf("paused %v, three-way %v", h.sess.Paused(), h.p.ConferenceActive())
	}
	if len(h.sess.commentary) != 3 || !strings.Contains(h.sess.commentary[2], "left the call") {
		t.Fatalf("commentary %q", h.sess.commentary)
	}
	// Off: the call goes on without the room.
	h.p.SetBridge("off")
	if h.p.BridgeActive() || h.p.call.stage != stageTalking {
		t.Errorf("mode %q, stage %v", h.p.Bridge(), h.p.call.stage)
	}
	h.expectNothing(t) // no BYE: the call itself is none of the bridge's business
	// The call ends: the room is free again.
	h.p.SetBridge("listen")
	h.sess.end()
	h.p.updateCall(time.Now())
	if m := h.read(t); m.Method != "BYE" {
		t.Fatalf("BYE %+v", m)
	}
	if h.p.BridgeActive() {
		t.Error("the bridge outlived the call")
	}
}

func TestTheButtonWalksThroughTheModes(t *testing.T) {
	h := newHarness(t, nil)
	if h.p.CycleBridge() != "" {
		t.Fatal("the button took a call that does not exist")
	}
	h.talking(t)
	for _, want := range []string{"listen", "talk", "off", "listen"} {
		h.p.CycleBridge()
		if h.p.Bridge() != want {
			t.Fatalf("the button gave %q, want %q", h.p.Bridge(), want)
		}
	}
}

func TestDialInKeepsTheCallAndEndsTheSession(t *testing.T) {
	h := newHarness(t, config.Secrets{"PHONE_CALLERS": "0171 1234567"})
	from := h.reg.LocalAddr().(*net.UDPAddr)
	h.p.onRequest(invite("d1", "01711234567"), from)
	h.read(t)
	h.read(t)
	h.sess.set(live.Live)
	h.p.updateCall(time.Now())
	h.read(t) // 200 OK
	// Released for the room (PHONE_CALLERS alone is not enough: that list is about memory and
	// tools, not about the microphone at home).
	if got := h.p.allowSpeaker("0171 1234567"); !strings.Contains(got, "may now ask") {
		t.Fatalf("allow: %q", got)
	}
	// The caller asks for the speaker at home: the session goes, the call stays.
	if answer := h.p.ConnectSpeaker("talk"); !strings.Contains(answer, "both ways") {
		t.Fatalf("connect: %q", answer)
	}
	if h.p.call.stage != stageBridge || h.p.Bridge() != "talk" {
		t.Fatalf("stage %v, mode %q", h.p.call.stage, h.p.Bridge())
	}
	if h.sess.endSoon != 1 {
		t.Errorf("the session was not ended (%d)", h.sess.endSoon)
	}
	if h.speaker.toneCount() == 0 {
		t.Error("the room was not told")
	}
	// The session is over - the call is not: that is the point of it.
	h.sess.end()
	h.p.updateCall(time.Now())
	h.expectNothing(t)
	if h.p.call.stage != stageBridge {
		t.Fatalf("stage %v after the session ended", h.p.call.stage)
	}
	// The caller's audio still reaches the room, once the announcement has had the speaker.
	room := h.p.call.line.room
	room.mu.Lock()
	room.quietUntil = time.Time{}
	room.mu.Unlock()
	h.p.call.line.room.Call(constant(4000, bridgePrefill))
	if frame := h.p.call.line.room.frame(); frame == nil {
		t.Error("the call does not reach the room any more")
	}
	// Ended at home ("Verbindung beenden"): the call is hung up.
	if answer := h.p.SetBridge("off"); answer != "Ended." {
		t.Fatalf("off: %q", answer)
	}
	if m := h.read(t); m.Method != "BYE" {
		t.Fatalf("BYE %+v", m)
	}
	if h.p.InCall() || h.p.BridgeActive() {
		t.Errorf("stage %v, mode %q", h.p.call.stage, h.p.Bridge())
	}
}

func TestDialInOnlyForReleasedNumbers(t *testing.T) {
	h := newHarness(t, config.Secrets{"PHONE_CALLERS": "0171 1234567"})
	from := h.reg.LocalAddr().(*net.UDPAddr)
	h.p.onRequest(invite("g2", "03012345678"), from) // a guest
	h.read(t)
	h.read(t)
	h.sess.set(live.Live)
	h.p.updateCall(time.Now())
	h.read(t)
	if answer := h.p.ConnectSpeaker("talk"); !strings.Contains(answer, "not released") {
		t.Fatalf("a guest got the room: %q", answer)
	}
	// Being in PHONE_CALLERS is not enough either; only the list opens the room.
	if !h.p.allowed("0171 1234567") {
		t.Fatal("the test number should be a released caller")
	}
	if h.p.BridgeActive() || h.p.call.stage != stageTalking {
		t.Errorf("mode %q, stage %v", h.p.Bridge(), h.p.call.stage)
	}
	// A call the assistant makes itself is not a dial-in either.
	if answer := h.p.ConnectSpeaker("monitor"); !strings.Contains(answer, "not released") {
		t.Errorf("%q", answer)
	}
	if answer := h.p.ConnectSpeaker("listen"); !strings.Contains(answer, "monitor or talk") {
		t.Errorf("mode listen accepted: %q", answer)
	}
}

func TestBridgeCallHangsUpAfterTheMaximum(t *testing.T) {
	h := newHarness(t, config.Secrets{"PHONE_CALLERS": "0171 1234567"})
	from := h.reg.LocalAddr().(*net.UDPAddr)
	h.p.onRequest(invite("d2", "01711234567"), from)
	h.read(t)
	h.read(t)
	h.sess.set(live.Live)
	h.p.updateCall(time.Now())
	h.read(t)
	ack, _ := Parse("ACK sip:jarvis@127.0.0.1 SIP/2.0\r\nCall-ID: d2\r\nCSeq: 1 ACK\r\n\r\n")
	h.p.onRequest(ack, from) // else the 200 OK is repeated and the quiet below is not quiet
	h.p.allowSpeaker("0171 1234567")
	h.p.ConnectSpeaker("monitor")
	h.sess.end()
	started := h.p.call.bridgeSince
	h.p.updateCall(started.Add(bridgeMaxCall - time.Second))
	h.expectNothing(t)
	h.p.updateCall(started.Add(bridgeMaxCall + time.Second))
	if m := h.read(t); m.Method != "BYE" {
		t.Fatalf("BYE %+v", m)
	}
	if h.p.InCall() || h.p.BridgeActive() {
		t.Error("the call was not cleared")
	}
}

// fakeConversation: the conversation a tool was called from (tools.Conversation).
type fakeConversation struct {
	label string
	ends  int
}

func (f *fakeConversation) EndSoon()      { f.ends++ }
func (f *fakeConversation) Label() string { return f.label }

func TestSpeakerToolsBelongToTheirSide(t *testing.T) {
	h := newHarness(t, config.Secrets{"PHONE_CALLERS": "0171 1234567"})
	var r tools.Registry
	h.p.AddTools(&r)
	names := strings.Join(r.Names(), ",")
	if !strings.Contains(names, "speaker_call") || !strings.Contains(names, "connect_speaker") {
		t.Fatalf("tools %s", names)
	}
	// speaker_call is for the room and never for a guest; connect_speaker a guest may ask for
	// (a released number that is not in PHONE_CALLERS is a guest), and is refused by the list.
	if got := r.Call("speaker_call", `{"mode":"talk"}`, tools.Context{Channel: tools.Stranger}); !strings.Contains(got, "unknown") {
		t.Errorf("a guest called speaker_call: %q", got)
	}
	if got := r.Call("connect_speaker", `{"mode":"talk"}`, tools.Context{Channel: tools.Stranger}); !strings.Contains(got, "no call") {
		t.Errorf("connect_speaker for a guest: %q", got)
	}
	h.talking(t)
	// From home: the call goes to the speaker and the conversation at home gets out of the way.
	home := &fakeConversation{label: "speaker"}
	got := r.Call("speaker_call", `{"mode":"listen"}`, tools.Context{Channel: tools.Voice, Conversation: home})
	if !strings.Contains(got, "listen") || home.ends != 1 {
		t.Errorf("speaker_call from home: %q, ends %d", got, home.ends)
	}
	// In the call itself the other tool is the right one, and the wrong one says so.
	call := &fakeConversation{label: "phone"}
	if got := r.Call("speaker_call", `{"mode":"off"}`, tools.Context{Channel: tools.Voice, Conversation: call}); !strings.Contains(got, "connect_speaker") {
		t.Errorf("speaker_call in the call: %q", got)
	}
	if got := r.Call("connect_speaker", `{"mode":"talk"}`, tools.Context{Channel: tools.Voice, Conversation: home}); !strings.Contains(got, "speaker_call") {
		t.Errorf("connect_speaker from home: %q", got)
	}
	// An error from home leaves the conversation alone.
	h.p.SetBridge("off")
	before := home.ends
	if got := r.Call("speaker_call", `{"mode":"sideways"}`, tools.Context{Channel: tools.Voice, Conversation: home}); !strings.Contains(got, "Error") {
		t.Errorf("%q", got)
	}
	if home.ends != before {
		t.Error("the conversation at home ended although nothing was switched on")
	}
}

// answerOurCall: the other side picks up the call the assistant dialled.
func (h *harness) answerOurCall(t *testing.T) {
	t.Helper()
	invite := h.read(t)
	if invite.Method != "INVITE" {
		t.Fatalf("expected an INVITE: %+v", invite)
	}
	ok := "SIP/2.0 200 OK\r\nVia: " + invite.Header("via") + "\r\nFrom: " + invite.Header("from") +
		"\r\nTo: <sip:x@127.0.0.1>;tag=far\r\nCall-ID: " + invite.Header("call-id") + "\r\nCSeq: " +
		invite.Header("cseq") + "\r\nContact: <sip:x@127.0.0.1>\r\nContent-Type: application/sdp\r\n\r\n"
	m, _ := Parse(ok)
	m.Body = "v=0\r\nc=IN IP4 127.0.0.1\r\nm=audio 9 RTP/AVP 8\r\n"
	h.p.onResponse(m)
	h.read(t) // our ACK
}

// "Ruf die Praxis an, ich möchte mithören": the call the assistant makes is on the speaker from the
// pick-up, and the other side is told right after his greeting.
func TestListeningInFromTheStart(t *testing.T) {
	h := newHarness(t, nil)
	h.register(t)
	r := &tools.Registry{}
	h.p.AddTools(r)
	out := r.Call("phone_call", `{"to": "07161 123456", "task": "einen Tisch für zwei um acht reservieren", "listen": true}`,
		tools.Context{Channel: tools.Voice})
	if !strings.Contains(out, "audible on the speaker") {
		t.Fatalf("phone_call: %q", out)
	}
	h.p.dialIfRequested(time.Now())
	h.answerOurCall(t)
	if h.p.Bridge() != "listen" {
		t.Fatalf("after the pick-up the call is %q, want listen", h.p.Bridge())
	}
	waitFor(t, "the tones at home", func() bool { return h.speaker.toneCount() >= 2 })
	h.sess.set(live.Live)
	h.p.updateCall(time.Now())
	h.sess.mu.Lock()
	notes := strings.Join(h.sess.commentary, " | ")
	h.sess.mu.Unlock()
	if !strings.Contains(notes, "§ 201") {
		t.Errorf("the other side was not told: %q", notes)
	}
	// The button goes on from there: talking along, then off.
	if got := h.p.CallButton(200 * time.Millisecond); !strings.Contains(got, "talk") {
		t.Errorf("short press while listening: %q, want talk", got)
	}
}

// "Ruf die Praxis an, ich rede mit": the microphone at home is on the line from the pick-up, and
// the assistant knows it is a three-way call.
func TestTalkingAlongFromTheStart(t *testing.T) {
	h := newHarness(t, nil)
	h.register(t)
	r := &tools.Registry{}
	h.p.AddTools(r)
	out := r.Call("phone_call", `{"to": "07161 123456", "task": "einen Termin vereinbaren", "talk": true}`,
		tools.Context{Channel: tools.Voice})
	if !strings.Contains(out, "talks along") {
		t.Fatalf("phone_call: %q", out)
	}
	h.p.dialIfRequested(time.Now())
	h.answerOurCall(t)
	if h.p.Bridge() != "talk" {
		t.Fatalf("after the pick-up the call is %q, want talk", h.p.Bridge())
	}
	h.sess.set(live.Live)
	h.p.updateCall(time.Now())
	h.sess.mu.Lock()
	notes := strings.Join(h.sess.commentary, " | ")
	h.sess.mu.Unlock()
	if !strings.Contains(notes, "§ 201") || !strings.Contains(notes, "three-way") {
		t.Errorf("the session was not told: %q", notes)
	}
	if !h.p.ConferenceActive() {
		t.Error("no three-way call: the assistant does not hear the room")
	}
}

// On the phone's app there is no speaker at home: phone_call has no listen or talk there.
func TestCallToolAwayHasNoListening(t *testing.T) {
	away := CallTool(false, nil)
	if strings.Contains(away.Parameters, "listen") || strings.Contains(away.Parameters, "talk") {
		t.Errorf("parameters %s", away.Parameters)
	}
	home := CallTool(true, nil)
	if !strings.Contains(home.Parameters, `"talk"`) || !strings.Contains(home.Parameters, `"listen"`) {
		t.Errorf("parameters %s", home.Parameters)
	}
	var r tools.Registry
	r.Add(away) // the schema is valid JSON
	r.Add(home)
}

// speaker_call without a call here: another Echo's call comes here (CallElsewhere), and without
// one either the usual answer.
func TestSpeakerCallTakesTheCallOfAnotherEcho(t *testing.T) {
	h := newHarness(t, nil)
	r := &tools.Registry{}
	h.p.AddTools(r)
	elsewhere := ""
	h.p.deps.CallElsewhere = func(mode string) (string, bool) {
		if elsewhere == "" {
			return "", false
		}
		return "The call at " + elsewhere + " is " + mode + " here now.", true
	}
	home := &fakeConversation{label: "speaker"}
	if got := r.Call("speaker_call", `{"mode":"listen"}`, tools.Context{Channel: tools.Voice, Conversation: home}); !strings.Contains(got, "no call") {
		t.Errorf("without any call: %q", got)
	}
	elsewhere = "Jarvis Küche"
	got := r.Call("speaker_call", `{"mode":"talk"}`, tools.Context{Channel: tools.Voice, Conversation: home})
	if !strings.Contains(got, "Jarvis Küche is talk here") || home.ends != 1 {
		t.Errorf("another Echo's call: %q, ends %d", got, home.ends)
	}
	// A call here goes first.
	h.register(t)
	h.talking(t)
	if got := r.Call("speaker_call", `{"mode":"listen"}`, tools.Context{Channel: tools.Voice}); strings.Contains(got, "Küche") {
		t.Errorf("a call here went to the other Echo: %q", got)
	}
}

func TestCallingSomeoneToTalkYourself(t *testing.T) {
	h := newHarness(t, nil)
	h.register(t)
	said := make(chan string, 4)
	h.p.deps.Say = func(text string) { said <- text }
	// The number comes from the model (a contact of Google); it is dialled at once.
	answer := h.p.dialForMe("07161 123456", "", tools.Context{Channel: tools.Voice})
	if !strings.Contains(answer, "Dialling") || !strings.Contains(answer, "07161123456") {
		t.Fatalf("dial_for_me: %q", answer)
	}
	h.p.dialIfRequested(time.Now())
	if h.p.call.stage != stageDialing || !h.p.call.direct {
		t.Fatalf("stage %v, direct %v", h.p.call.stage, h.p.call.direct)
	}
	// No session is started for it: nothing to pay, nobody listening in.
	if len(h.sess.started) != 0 {
		t.Errorf("a session was started: %+v", h.sess.started)
	}
	// It rings at the other end: the ring shows it and the speaker rings along, every five
	// seconds, and not while a conversation at the speaker is still running.
	if !h.p.Dialling() {
		t.Error("not dialling")
	}
	h.busy = true
	h.p.updateCall(time.Now())
	h.busy = false
	if h.speaker.rang(ringbackFreq) != 0 {
		t.Error("the ringback talked into the conversation at the speaker")
	}
	start := h.p.call.since
	h.p.updateCall(start)
	waitFor(t, "the first ringback", func() bool { return h.speaker.rang(ringbackFreq) == 1 })
	h.p.updateCall(start.Add(time.Second)) // too early for the second one
	time.Sleep(50 * time.Millisecond)
	if n := h.speaker.rang(ringbackFreq); n != 1 {
		t.Errorf("rang %d times after one second, want 1", n)
	}
	h.p.updateCall(start.Add(ringbackEvery + time.Second))
	waitFor(t, "the second ringback", func() bool { return h.speaker.rang(ringbackFreq) == 2 })
	// They pick up: the call is on the speaker and the microphone, and the room is told.
	h.answerOurCall(t)
	if h.p.call.stage != stageBridge || h.p.Bridge() != "talk" {
		t.Fatalf("stage %v, mode %q", h.p.call.stage, h.p.Bridge())
	}
	if h.p.Dialling() {
		t.Error("still dialling after the pick-up")
	}
	if h.speaker.flushes != 1 {
		t.Errorf("the queued ringback was not dropped (%d flushes)", h.speaker.flushes)
	}
	if h.speaker.rang(880) == 0 {
		t.Error("no tones at the speaker")
	}
	// The ring follows the voices of the call: nothing while the line is silent, bands once it
	// plays into the room.
	if levels := h.p.Spectrum(); levels != [7]float64{} {
		t.Errorf("the ring is lit without audio: %v", levels)
	}
	room := h.p.call.line.room
	room.mu.Lock()
	room.quietUntil = time.Time{}
	room.mu.Unlock()
	for i := 0; i < 60; i++ { // ~1 s of a 1 kHz voice from the line
		room.Call(sine(1000, apiRate, apiFrame, 9000))
		if pcm := room.frame(); pcm != nil {
			room.spec.Feed(pcm)
		}
	}
	if levels := h.p.Spectrum(); levels == [7]float64{} {
		t.Error("the ring stays dark while the call plays")
	}
	mic := sine(1000, micRate, micFrame, 9000)
	h.p.Hear(mic)
	h.p.call.line.mu.Lock()
	queued := len(h.p.call.line.mic)
	h.p.call.line.mu.Unlock()
	if queued == 0 {
		t.Error("the microphone does not reach the line")
	}
	// The session timers must not touch this call: no session is open at all.
	h.p.updateCall(time.Now())
	h.expectNothing(t)
	// Ended at home, and the activity log keeps that the user called.
	if answer := h.p.SetBridge("off"); answer != "Ended." {
		t.Fatalf("off: %q", answer)
	}
	if m := h.read(t); m.Method != "BYE" {
		t.Fatalf("BYE %+v", m)
	}
	select {
	case ev := <-h.events:
		if ev[1] != "phone_self" || !strings.Contains(ev[3], "selbst mit 07161123456") {
			t.Errorf("activity log %q", ev)
		}
	case <-time.After(time.Second):
		t.Error("the call is not in the activity log")
	}
	// Their own conversation is not reported back to them: there is no transcript and no task.
	if len(h.p.reports) != 0 {
		t.Errorf("%d reports about the user's own call", len(h.p.reports))
	}
	h.p.reportIfDue(time.Now().Add(2 * reportGrace))
	select {
	case run := <-h.runs:
		t.Errorf("a report went out: %v", run)
	default:
	}
}

func TestCallingSomeoneWhoDoesNotAnswer(t *testing.T) {
	h := newHarness(t, nil)
	h.register(t)
	said := make(chan string, 4)
	h.p.deps.Say = func(text string) { said <- text }
	h.p.dialForMe("07161 123456", "", tools.Context{Channel: tools.Voice})
	h.p.dialIfRequested(time.Now())
	h.read(t) // the INVITE
	started := h.p.call.since
	h.p.updateCall(started.Add(ringTimeout - time.Second))
	h.expectNothing(t)
	h.p.updateCall(started.Add(ringTimeout + time.Second))
	if m := h.read(t); m.Method != "CANCEL" {
		t.Fatalf("CANCEL %+v", m)
	}
	select {
	case text := <-said:
		if !strings.Contains(text, "nimmt nicht ab") {
			t.Errorf("notice %q", text)
		}
	case <-time.After(time.Second):
		t.Error("the room was not told")
	}
	// A refused call is said at home too, not sent as a message.
	h2 := newHarness(t, nil)
	h2.register(t)
	h2.p.deps.Say = func(text string) { said <- text }
	h2.p.dialForMe("07161 123456", "", tools.Context{Channel: tools.Voice})
	h2.p.dialIfRequested(time.Now())
	invite := h2.read(t)
	busy, _ := Parse("SIP/2.0 486 Busy Here\r\nVia: " + invite.Header("via") + "\r\nFrom: " + invite.Header("from") +
		"\r\nTo: <sip:x@127.0.0.1>;tag=far\r\nCall-ID: " + invite.Header("call-id") + "\r\nCSeq: " + invite.Header("cseq") + "\r\n\r\n")
	h2.p.onResponse(busy)
	h2.read(t) // our ACK
	select {
	case text := <-said:
		if !strings.Contains(text, "besetzt") {
			t.Errorf("notice %q", text)
		}
	case <-time.After(time.Second):
		t.Error("the room was not told")
	}
	if h2.p.InCall() {
		t.Error("the call was not cleared")
	}
}

func TestCallingYourselfRefusesWhatItCannot(t *testing.T) {
	h := newHarness(t, nil)
	ctx := tools.Context{Channel: tools.Voice}
	// Not registered at the FritzBox yet.
	if got := h.p.dialForMe("07161 123456", "", ctx); !strings.Contains(got, "not registered") {
		t.Errorf("%q", got)
	}
	h.register(t)
	for _, bad := range []struct{ to, want string }{
		{"", "say who to call"},
		{"0900 123456", "premium"},
		{"0043 1 234567", "only German"}, // 0049 would be normalized to a German number
		{"12345", "not a person of the household"},
	} {
		if got := h.p.dialForMe(bad.to, "", ctx); !strings.Contains(got, bad.want) {
			t.Errorf("%q gave %q, want %q", bad.to, got, bad.want)
		}
	}
	// A device without a speaker cannot hand a call over.
	h.p.deps.Speaker = nil
	if got := h.p.dialForMe("07161 123456", "", ctx); !strings.Contains(got, "no speaker") {
		t.Errorf("%q", got)
	}
	h.p.deps.Speaker = h.speaker
	// While a call runs, a second one is refused instead of queued for half an hour.
	h.talking(t)
	if got := h.p.dialForMe("07161 123456", "", ctx); !strings.Contains(got, "already running") {
		t.Errorf("%q", got)
	}
}

func TestDialForMeEndsTheConversationThatAsked(t *testing.T) {
	h := newHarness(t, nil)
	h.register(t)
	var r tools.Registry
	h.p.AddTools(&r)
	home := &fakeConversation{label: "speaker"}
	if got := r.Call("dial_for_me", `{"to":"07161 123456"}`, tools.Context{Channel: tools.Voice, Conversation: home}); !strings.Contains(got, "Dialling") {
		t.Fatalf("%q", got)
	}
	if home.ends != 1 {
		t.Errorf("the conversation at home did not end (%d)", home.ends)
	}
	// From a phone call it is refused, and then nothing ends.
	call := &fakeConversation{label: "phone"}
	if got := r.Call("dial_for_me", `{"to":"07161 123456"}`, tools.Context{Channel: tools.Voice, Conversation: call}); !strings.Contains(got, "not from a phone call") {
		t.Errorf("%q", got)
	}
	if call.ends != 0 {
		t.Error("the phone call was ended")
	}
	// A guest on the phone does not get the tool at all.
	if got := r.Call("dial_for_me", `{"to":"07161 123456"}`, tools.Context{Channel: tools.Stranger}); !strings.Contains(got, "unknown") {
		t.Errorf("a guest called dial_for_me: %q", got)
	}
}

func TestBridgeNeedsACallAndASpeaker(t *testing.T) {
	h := newHarness(t, nil)
	if answer := h.p.SetBridge("listen"); !strings.Contains(answer, "no call") {
		t.Errorf("%q", answer)
	}
	if answer := h.p.SetBridge("sideways"); !strings.Contains(answer, "off, listen or talk") {
		t.Errorf("%q", answer)
	}
	// A device without a speaker refuses, and registers no tool for it.
	h.p.deps.Speaker = nil
	h.p.bridge = newBridge(nil, nil)
	h.talking(t)
	if answer := h.p.SetBridge("listen"); !strings.Contains(answer, "no speaker") {
		t.Errorf("%q", answer)
	}
	if h.p.BridgeActive() {
		t.Error("a call on a speaker that does not exist")
	}
}
