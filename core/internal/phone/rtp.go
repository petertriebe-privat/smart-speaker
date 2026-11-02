package phone

import (
	"crypto/rand"
	"encoding/binary"
	"log"
	"math"
	"net"
	"strings"
	"sync"
	"time"

	"echo-assistant/internal/prompts"
)

// The audio of a phone call as devices/atoms3r/src/phone/rtp.cpp: RTP over UDP, G.711 at 8 kHz,
// converted to and from the 24 kHz PCM of GPT Live. A goroutine sends a 20 ms packet every 20 ms
// (silence when nothing is queued) and one takes in the caller's packets and key presses
// (RFC 4733 telephone-event). *Line implements live.Output.

const (
	audible        = 300         // |sample| above this counts as speaking
	maxQueued      = apiRate * 4 // 4 s of the assistant's voice: GPT Live sends in real time, bursts stay small
	maxKeysQueued  = 32
	keyTone        = 5 // a pressed key: 100 ms tone, the RFC 4733 end packet three times,
	keyEnd         = 3 // then a pause before the next
	keyFrames      = keyTone + keyEnd + 5
	rtpHeaderBytes = 12
)

// Lower and upper DTMF frequencies of the keys "0123456789*#".
var (
	dtmfKeys = "0123456789*#"
	dtmfLow  = [12]float64{941, 697, 697, 697, 770, 770, 770, 852, 852, 852, 941, 941}
	dtmfHigh = [12]float64{1336, 1209, 1336, 1477, 1209, 1336, 1477, 1209, 1336, 1477, 1209, 1477}
)

// rtpPacket: a parsed RTP packet (RFC 3550); payload is the media without CSRC list, header
// extension and padding.
type rtpPacket struct {
	Type      uint8
	Marker    bool
	Sequence  uint16
	Timestamp uint32
	SSRC      uint32
	Payload   []byte
}

func parseRTP(p []byte) (rtpPacket, bool) {
	if len(p) < rtpHeaderBytes || p[0]>>6 != 2 {
		return rtpPacket{}, false
	}
	offset := rtpHeaderBytes + 4*int(p[0]&0x0F) // CSRC list
	if p[0]&0x10 != 0 {                         // header extension
		if len(p) < offset+4 {
			return rtpPacket{}, false
		}
		offset += 4 + 4*int(binary.BigEndian.Uint16(p[offset+2:]))
	}
	end := len(p)
	if p[0]&0x20 != 0 { // padding
		end -= int(p[len(p)-1])
	}
	if offset >= end {
		return rtpPacket{}, false
	}
	return rtpPacket{Type: p[1] & 0x7F, Marker: p[1]&0x80 != 0, Sequence: binary.BigEndian.Uint16(p[2:]),
		Timestamp: binary.BigEndian.Uint32(p[4:]), SSRC: binary.BigEndian.Uint32(p[8:]), Payload: p[offset:end]}, true
}

// buildRTP: header (version 2, no padding/extension/CSRC) plus payload.
func buildRTP(typ uint8, marker bool, seq uint16, stamp, ssrc uint32, payload []byte) []byte {
	p := make([]byte, rtpHeaderBytes+len(payload))
	p[0] = 0x80
	p[1] = typ
	if marker {
		p[1] |= 0x80
	}
	binary.BigEndian.PutUint16(p[2:], seq)
	binary.BigEndian.PutUint32(p[4:], stamp)
	binary.BigEndian.PutUint32(p[8:], ssrc)
	copy(p[rtpHeaderBytes:], payload)
	return p
}

// eventPayload: an RFC 4733 telephone-event payload (event, end bit, volume -10 dBm0, duration
// in timestamp units).
func eventPayload(event int, end bool, duration int) []byte {
	b := []byte{byte(event), 10, byte(duration >> 8), byte(duration)}
	if end {
		b[1] |= 0x80
	}
	return b
}

// parseEvent: the key of a telephone-event payload, 0 if it is no key or not the end packet
// (the end packet, sent three times, reports the key once per event).
func parseEvent(payload []byte) byte {
	if len(payload) < 4 || payload[1]&0x80 == 0 {
		return 0
	}
	if int(payload[0]) < len(dtmfKeys) {
		return dtmfKeys[payload[0]]
	}
	return 0
}

// Line: the audio of one call. Create it with NewLine, start it with Start when the call is
// answered, stop it with Stop. Before Start and after Stop, Queue drops the audio like a closed line.
type Line struct {
	port    int
	deliver func([]int16) // the caller's audio, 24 kHz (session.FeedLine)
	room    *bridge       // the call on the speaker at home (nil = never); drops what it gets while off
	jitter  int           // samples queued before the assistant's voice starts: against network jitter
	tail    time.Duration

	mu          sync.Mutex
	conn        *net.UDPConn
	stop        chan struct{}
	remote      *net.UDPAddr
	payload     uint8
	dtmf        int
	buf         []int16 // The assistant's voice, 24 kHz, not sent yet
	mic         []int16 // the microphone at home, 24 kHz, not sent yet (talking along)
	playing     bool
	lastAudible time.Time
	keys        []byte // pressed one after another
	key         byte   // the key being pressed, 0 = none
	keyFrame    int    // 20 ms frames since it started
	keyStamp    uint32

	upMic MicResampler // MicIn's caller only (the microphone's goroutine)

	// sender goroutine only
	down   Downsampler
	seq    uint16
	stamp  uint32
	ssrc   uint32
	first  bool
	feed   chan []int16
	digits chan byte
	up     Upsampler // receiver goroutine only
	lastEv uint32
	haveEv bool
	inband dtmfDetector
}

// listenIP: the address SIP and RTP listen on; nil = every interface (the FritzBox reaches the
// Echo over the WLAN). The tests set the loopback: a test binary on all interfaces makes the
// Windows firewall ask on every run, since each build is a new executable.
var listenIP net.IP

// micLag: the microphone at home queued at most on the way to the line; older audio is dropped.
var micLag = max(prompts.Int("PHONE_MIC_LAG_MS", 80)*apiRate/1000, 2*apiFrame)

// NewLine: deliver gets the caller's audio (24 kHz); port is the local RTP port; room is the
// bridge to the speaker at home (nil = none).
func NewLine(port int, deliver func([]int16), room *bridge) *Line {
	return &Line{port: port, deliver: deliver, room: room, dtmf: -1,
		jitter: prompts.Int("PHONE_JITTER_MS", 60) * apiRate / 1000,
		tail:   time.Duration(prompts.Int("ECHO_TAIL_MS", 300)) * time.Millisecond,
		digits: make(chan byte, 16)}
}

// Start begins the stream to the negotiated address and payload types (8 = A-law, 0 = µ-law;
// dtmf -1 = no telephone-event). On a running line (re-INVITE) only the target changes.
func (l *Line) Start(remote *net.UDPAddr, payload uint8, dtmf int) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.remote, l.payload, l.dtmf = remote, payload, dtmf
	if l.conn != nil {
		return nil
	}
	conn, err := net.ListenUDP("udp4", &net.UDPAddr{IP: listenIP, Port: l.port})
	if err != nil {
		return err
	}
	l.conn, l.stop = conn, make(chan struct{})
	l.buf, l.mic, l.playing, l.keys, l.key = nil, nil, false, nil, 0
	l.down, l.up, l.haveEv, l.first = Downsampler{}, Upsampler{}, false, true
	l.seq, l.stamp, l.ssrc = uint16(randomUint32()), randomUint32(), randomUint32()
	l.feed = make(chan []int16, 100)
	for len(l.digits) > 0 {
		<-l.digits
	}
	go l.sendLoop(conn, l.stop)
	go l.receiveLoop(conn, l.stop)
	go l.feedLoop(l.feed, l.stop)
	return nil
}

func randomUint32() uint32 {
	var b [4]byte
	rand.Read(b[:])
	return binary.BigEndian.Uint32(b[:])
}

// Stop ends the stream; the assistant's queued voice is dropped.
func (l *Line) Stop() {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.conn == nil {
		return
	}
	close(l.stop)
	l.conn.Close()
	l.conn, l.buf, l.mic, l.playing, l.keys, l.key = nil, nil, nil, false, nil, 0
}

// Active: the stream runs.
func (l *Line) Active() bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.conn != nil
}

// --- live.Output ----------------------------------------------------------------------------

// Queue takes the assistant's voice (24 kHz). Audio that does not fit into the queue is dropped.
func (l *Line) Queue(pcm []int16) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.conn == nil {
		return nil // no call (any more): dropped like on a closed line
	}
	if room := maxQueued - len(l.buf); len(pcm) > room {
		pcm = pcm[:max(room, 0)]
	}
	l.buf = append(l.buf, pcm...)
	l.room.Assistant(pcm) // while the call is on the speaker at home, he is audible there too
	return nil
}

// MicIn takes the microphone at home (16 kHz) onto the line: it is added to the assistant's voice in the
// frames that go out. Call it from one goroutine only (the microphone's). The queue keeps the
// newest micLag: it drains exactly as fast as the microphone fills it, so a batch after a stall
// of the microphone's goroutine (the wake word, the GC on the one core) would otherwise stay on
// the line as delay for the rest of the call.
func (l *Line) MicIn(pcm []int16) {
	up := l.upMic.Process(pcm)
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.conn == nil {
		return
	}
	l.mic = append(l.mic, up...)
	if over := len(l.mic) - micLag; over > 0 {
		l.mic = l.mic[:copy(l.mic, l.mic[over:])]
	}
}

// Queued: the voice waiting to be sent.
func (l *Line) Queued() time.Duration {
	l.mu.Lock()
	defer l.mu.Unlock()
	return time.Duration(len(l.buf)) * time.Second / apiRate
}

// Speaking: audible output within the last tail (at least ECHO_TAIL_MS, as the M5). By level,
// not by queued audio: GPT Live streams silence too, so the queue is never empty (with that test
// The assistant never counted as quiet on the phone and never hung up).
func (l *Line) Speaking(tail time.Duration) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	return !l.lastAudible.IsZero() && time.Since(l.lastAudible) < max(tail, l.tail)
}

// Flush drops the queued voice (the caller interrupted).
func (l *Line) Flush() {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.buf, l.playing = l.buf[:0], false
}

// Tone queues a sine tone (a beep) into the voice.
func (l *Line) Tone(freq float64, ms int, amp float64) {
	n := ms * apiRate / 1000
	pcm := make([]int16, n)
	for i := range pcm {
		pcm[i] = int16(amp * 32767 * math.Sin(2*math.Pi*freq*float64(i)/apiRate))
	}
	l.Queue(pcm)
}

// --- Keys -------------------------------------------------------------------------------------

// TakeDigit: a key the caller pressed ('0'..'9', '*', '#').
func (l *Line) TakeDigit() (byte, bool) {
	select {
	case d := <-l.digits:
		return d, true
	default:
		return 0, false
	}
}

// PressKeys presses keys ('0'..'9', '*', '#') on our side, one after another (~0.26 s each):
// RFC 4733 events if the call negotiated telephone-event, else tones in the audio. False if the
// line is not running or the keys do not fit into the queue.
func (l *Line) PressKeys(keys string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.conn == nil || len(l.keys)+len(keys) > maxKeysQueued {
		return false
	}
	for i := 0; i < len(keys); i++ {
		if !strings.ContainsRune(dtmfKeys, rune(keys[i])) {
			return false
		}
	}
	l.keys = append(l.keys, keys...)
	return true
}

// --- Goroutines -----------------------------------------------------------------------------

func (l *Line) sendLoop(conn *net.UDPConn, stop chan struct{}) {
	next := time.Now()
	for {
		next = next.Add(20 * time.Millisecond)
		wait := time.Until(next)
		if wait < -200*time.Millisecond {
			next = time.Now() // fell behind (suspended): no burst of packets
		} else if wait > 0 {
			timer := time.NewTimer(wait)
			select {
			case <-stop:
				timer.Stop()
				return
			case <-timer.C:
			}
		}
		select {
		case <-stop:
			return
		default:
		}
		l.sendFrame(conn)
	}
}

// sendFrame: one 20 ms packet of the assistant's voice, or of a pressed key, or silence.
func (l *Line) sendFrame(conn *net.UDPConn) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.conn != conn || l.remote == nil {
		return
	}
	if l.key == 0 && len(l.keys) > 0 {
		l.key, l.keys, l.keyFrame = l.keys[0], l.keys[1:], 0
	}
	// The assistant's voice of this frame (dropped while a key is pressed).
	var pcm [apiFrame]int16
	queued := len(l.buf)
	if !l.playing && queued >= l.jitter {
		l.playing = true
	}
	if l.playing {
		n := copy(pcm[:], l.buf)
		l.buf = l.buf[:copy(l.buf, l.buf[n:])]
		if queued < apiFrame {
			l.playing = false
		}
	}
	// Someone at home takes part in the call: their voice is added to his, without the jitter
	// buffer (the microphone arrives in real time, 20 ms per frame).
	if len(l.mic) > 0 {
		n := min(len(l.mic), apiFrame)
		for i := 0; i < n; i++ {
			pcm[i] = clamp16(float32(pcm[i]) + float32(l.mic[i]))
		}
		l.mic = l.mic[:copy(l.mic, l.mic[n:])]
	}
	if l.key != 0 {
		l.sendKeyLocked(conn)
		return
	}
	payload := make([]byte, 0, frameLen)
	heard := false
	for _, s := range l.down.Process(pcm[:]) {
		heard = heard || s > audible || s < -audible
		payload = append(payload, encodeSample(l.payload, s))
	}
	if heard {
		l.lastAudible = time.Now()
	}
	l.transmitLocked(conn, l.payload, l.first, l.stamp, payload)
	l.stamp += frameLen
	l.first = false
}

// sendKeyLocked: one 20 ms frame of a pressed key instead of the assistant's voice: an RFC 4733 event if
// the call negotiated telephone-event, else the two tones in the audio itself.
func (l *Line) sendKeyLocked(conn *net.UDPConn) {
	if l.keyFrame == 0 {
		l.keyStamp = l.stamp
	}
	event := strings.IndexByte(dtmfKeys, l.key)
	if l.dtmf >= 0 && l.keyFrame < keyTone+keyEnd {
		end := l.keyFrame >= keyTone
		duration := frameLen * (l.keyFrame + 1)
		if end {
			duration = frameLen * keyTone
		}
		l.transmitLocked(conn, uint8(l.dtmf), l.keyFrame == 0, l.keyStamp, eventPayload(event, end, duration))
	} else {
		payload := make([]byte, frameLen)
		for i := range payload {
			var tone float64
			if l.keyFrame < keyTone && l.dtmf < 0 {
				t := float64(l.keyFrame*frameLen+i) / lineRate
				tone = 6000 * (math.Sin(2*math.Pi*dtmfLow[event]*t) + math.Sin(2*math.Pi*dtmfHigh[event]*t))
			}
			payload[i] = encodeSample(l.payload, clamp16(float32(tone)))
		}
		l.transmitLocked(conn, l.payload, false, l.stamp, payload)
	}
	l.stamp += frameLen
	if l.keyFrame++; l.keyFrame == keyFrames {
		l.key = 0
	}
}

func (l *Line) transmitLocked(conn *net.UDPConn, typ uint8, marker bool, stamp uint32, payload []byte) {
	conn.WriteToUDP(buildRTP(typ, marker, l.seq, stamp, l.ssrc, payload), l.remote)
	l.seq++
}

func (l *Line) receiveLoop(conn *net.UDPConn, stop chan struct{}) {
	buf := make([]byte, 1500)
	for {
		n, _, err := conn.ReadFromUDP(buf)
		if err != nil {
			select {
			case <-stop:
			default:
				log.Printf("[RTP] receive: %v", err)
			}
			return
		}
		packet, ok := parseRTP(buf[:n])
		if !ok {
			continue
		}
		l.mu.Lock()
		payloadType, dtmfType := l.payload, l.dtmf
		l.mu.Unlock()
		switch {
		case packet.Type == payloadType:
			l.receiveAudio(packet.Payload, dtmfType < 0)
		case dtmfType >= 0 && int(packet.Type) == dtmfType:
			l.receiveEvent(packet)
		}
	}
}

// receiveAudio: the caller's audio, G.711 -> 24 kHz into the conversation. Without
// telephone-event the keys come in the audio itself: detected here.
func (l *Line) receiveAudio(payload []byte, inband bool) {
	l.mu.Lock()
	typ := l.payload
	l.mu.Unlock()
	pcm8 := make([]int16, len(payload))
	for i, c := range payload {
		pcm8[i] = decodeSample(typ, c)
	}
	if inband {
		for _, d := range l.inband.Feed(pcm8) {
			l.pushDigit(d)
		}
	}
	pcm24 := l.up.Process(pcm8)
	l.room.Call(pcm24) // the call on the speaker at home
	select {
	case l.feed <- pcm24:
	default: // the conversation does not keep up: dropped
	}
}

func (l *Line) receiveEvent(p rtpPacket) {
	if l.haveEv && p.Timestamp == l.lastEv {
		return
	}
	if d := parseEvent(p.Payload); d != 0 {
		l.lastEv, l.haveEv = p.Timestamp, true
		l.pushDigit(d)
	}
}

func (l *Line) pushDigit(d byte) {
	select {
	case l.digits <- d:
	default:
	}
}

func (l *Line) feedLoop(feed chan []int16, stop chan struct{}) {
	for {
		select {
		case <-stop:
			return
		case pcm := <-feed:
			if l.deliver != nil {
				l.deliver(pcm)
			}
		}
	}
}
