package phone

import (
	"bytes"
	"math"
	"net"
	"sync"
	"testing"
	"time"
)

func TestRTPHeader(t *testing.T) {
	payload := []byte{1, 2, 3}
	p := buildRTP(8, true, 0x1234, 0xAABBCCDD, 0x01020304, payload)
	if !bytes.Equal(p[:12], []byte{0x80, 0x88, 0x12, 0x34, 0xAA, 0xBB, 0xCC, 0xDD, 1, 2, 3, 4}) {
		t.Errorf("header % x", p[:12])
	}
	got, ok := parseRTP(p)
	if !ok || got.Type != 8 || !got.Marker || got.Sequence != 0x1234 || got.Timestamp != 0xAABBCCDD ||
		got.SSRC != 0x01020304 || !bytes.Equal(got.Payload, payload) {
		t.Errorf("parsed %+v", got)
	}
	if _, ok := parseRTP(p[:11]); ok {
		t.Error("short packet parsed")
	}
	if _, ok := parseRTP(buildRTP(0, false, 1, 1, 1, nil)); ok {
		t.Error("packet without payload parsed")
	}
	bad := append([]byte(nil), p...)
	bad[0] = 0x40
	if _, ok := parseRTP(bad); ok {
		t.Error("version 1 parsed")
	}
}

func TestRTPCsrcExtensionPadding(t *testing.T) {
	// 1 CSRC, an extension of one word, 2 bytes of padding.
	p := []byte{0x80 | 0x10 | 0x20 | 1, 8, 0, 1, 0, 0, 0, 2, 0, 0, 0, 3}
	p = append(p, 0, 0, 0, 9)       // CSRC
	p = append(p, 0xBE, 0xDE, 0, 1) // extension header, length 1
	p = append(p, 7, 7, 7, 7)       // extension word
	p = append(p, 0xAA, 0xBB, 0xCC, 0, 2)
	got, ok := parseRTP(p)
	if !ok || !bytes.Equal(got.Payload, []byte{0xAA, 0xBB, 0xCC}) {
		t.Errorf("parsed %+v %v", got, ok)
	}
}

func TestTelephoneEvent(t *testing.T) {
	end := eventPayload(11, true, 800)
	if !bytes.Equal(end, []byte{11, 0x8A, 0x03, 0x20}) {
		t.Errorf("end packet % x", end)
	}
	if parseEvent(end) != '#' || parseEvent(eventPayload(0, true, 800)) != '0' || parseEvent(eventPayload(10, true, 1)) != '*' {
		t.Error("keys")
	}
	if parseEvent(eventPayload(5, false, 160)) != 0 {
		t.Error("a packet before the end reports a key")
	}
	if parseEvent(eventPayload(12, true, 160)) != 0 || parseEvent([]byte{1, 2}) != 0 {
		t.Error("flash/garbage reports a key")
	}
}

func TestDtmfDetector(t *testing.T) {
	var d dtmfDetector
	var found []byte
	for k := range dtmfKeys {
		tone := make([]int16, 4*frameLen)
		for i := range tone {
			tt := float64(i) / lineRate
			tone[i] = int16(5000 * (math.Sin(2*math.Pi*dtmfLow[k]*tt) + math.Sin(2*math.Pi*dtmfHigh[k]*tt)))
		}
		found = append(found, d.Feed(tone)...)
		found = append(found, d.Feed(make([]int16, 2*frameLen))...) // pause
	}
	if string(found) != dtmfKeys {
		t.Errorf("found %q, want %q", found, dtmfKeys)
	}
	// Speech-like single tone and silence: nothing.
	var e dtmfDetector
	if got := e.Feed(sine(1000, lineRate, 800, 8000)); len(got) != 0 {
		t.Errorf("single tone: %q", got)
	}
	if got := e.Feed(make([]int16, 800)); len(got) != 0 {
		t.Errorf("silence: %q", got)
	}
}

// fixture: a Line sending to a local socket (the "far end").
func newPair(t *testing.T, dtmf int) (*Line, *net.UDPConn, *[]int16, *sync.Mutex) {
	t.Helper()
	far, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { far.Close() })
	probe, _ := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	port := probe.LocalAddr().(*net.UDPAddr).Port
	probe.Close()
	var mu sync.Mutex
	var heard []int16
	line := NewLine(port, func(pcm []int16) { mu.Lock(); heard = append(heard, pcm...); mu.Unlock() }, nil)
	if err := line.Start(far.LocalAddr().(*net.UDPAddr), payloadPCMA, dtmf); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(line.Stop)
	return line, far, &heard, &mu
}

func readPacket(t *testing.T, far *net.UDPConn) rtpPacket {
	t.Helper()
	buf := make([]byte, 1500)
	far.SetReadDeadline(time.Now().Add(2 * time.Second))
	n, _, err := far.ReadFromUDP(buf)
	if err != nil {
		t.Fatal(err)
	}
	p, ok := parseRTP(buf[:n])
	if !ok {
		t.Fatalf("not RTP: % x", buf[:n])
	}
	return p
}

func TestLineSendsSilenceEvery20ms(t *testing.T) {
	line, far, _, _ := newPair(t, 101)
	first := readPacket(t, far)
	second := readPacket(t, far)
	if first.Type != payloadPCMA || !first.Marker || second.Marker || len(first.Payload) != frameLen {
		t.Errorf("packets %+v %+v", first, second)
	}
	if second.Sequence != first.Sequence+1 || second.Timestamp != first.Timestamp+frameLen || second.SSRC != first.SSRC {
		t.Errorf("sequence/timestamp: %+v %+v", first, second)
	}
	if first.Payload[0] != encodeAlaw(0) {
		t.Errorf("not silence: %x", first.Payload[0])
	}
	if line.Speaking(0) {
		t.Error("speaking while silent")
	}
}

func TestLineSendsVoiceAfterJitterBuffer(t *testing.T) {
	line, far, _, _ := newPair(t, 101)
	voice := sine(500, apiRate, apiRate/2, 12000) // 0.5 s
	line.Queue(voice)
	if line.Queued() < 400*time.Millisecond {
		t.Errorf("queued %v", line.Queued())
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
		t.Fatal("no voice on the line")
	}
	if !line.Speaking(0) {
		t.Error("not speaking")
	}
	line.Flush()
	if line.Queued() != 0 {
		t.Error("flush")
	}
}

func TestLinePressKeysAsEvents(t *testing.T) {
	line, far, _, _ := newPair(t, 101)
	if !line.PressKeys("5") || line.PressKeys("x") || line.PressKeys("1111111111111111111111111111111111") {
		t.Fatal("PressKeys validation")
	}
	var events []rtpPacket
	for i := 0; i < 40 && len(events) < keyTone+keyEnd; i++ {
		if p := readPacket(t, far); p.Type == 101 {
			events = append(events, p)
		}
	}
	if len(events) != keyTone+keyEnd {
		t.Fatalf("%d event packets", len(events))
	}
	if !events[0].Marker || events[1].Marker {
		t.Error("marker only on the first")
	}
	for _, e := range events {
		if e.Timestamp != events[0].Timestamp || e.Payload[0] != 5 {
			t.Fatalf("event %+v", e)
		}
	}
	if parseEvent(events[keyTone-1].Payload) != 0 || parseEvent(events[keyTone].Payload) != '5' || parseEvent(events[len(events)-1].Payload) != '5' {
		t.Error("end bit")
	}
}

func TestLinePressKeysAsTonesWithoutTelephoneEvent(t *testing.T) {
	line, far, _, _ := newPair(t, -1)
	line.PressKeys("1")
	var tone []int16
	for i := 0; i < 14; i++ {
		p := readPacket(t, far)
		if p.Type != payloadPCMA {
			t.Fatalf("type %d", p.Type)
		}
		for _, c := range p.Payload {
			tone = append(tone, decodeAlaw(c))
		}
	}
	var d dtmfDetector
	if got := d.Feed(tone); string(got) != "1" {
		t.Errorf("tones decode as %q", got)
	}
}

func TestLineReceivesAudioAndKeys(t *testing.T) {
	line, far, heard, mu := newPair(t, 101)
	to := &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1), Port: line.port}
	tone := sine(1000, lineRate, 8*frameLen, 10000)
	for i := 0; i < 8; i++ {
		payload := make([]byte, frameLen)
		for j := range payload {
			payload[j] = encodeAlaw(tone[i*frameLen+j])
		}
		far.WriteToUDP(buildRTP(payloadPCMA, i == 0, uint16(i), uint32(i*frameLen), 7, payload), to)
	}
	// The end packet of key 7, three times (one event).
	for i := 0; i < 3; i++ {
		far.WriteToUDP(buildRTP(101, false, uint16(100+i), 5000, 7, eventPayload(7, true, 800)), to)
	}
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		mu.Lock()
		n := len(*heard)
		mu.Unlock()
		if n >= 8*apiFrame {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	mu.Lock()
	got := append([]int16(nil), (*heard)...)
	mu.Unlock()
	if len(got) != 8*apiFrame {
		t.Fatalf("%d samples at 24 kHz, want %d", len(got), 8*apiFrame)
	}
	if r := rms(got[apiFrame:]); r < 5000 || r > 9000 {
		t.Errorf("caller's level %.0f", r)
	}
	if d, ok := line.TakeDigit(); !ok || d != '7' {
		t.Errorf("digit %q %v", d, ok)
	}
	if _, ok := line.TakeDigit(); ok {
		t.Error("the repeated end packets gave more than one key")
	}
}

func TestLineStoppedDropsAudio(t *testing.T) {
	line, _, _, _ := newPair(t, -1)
	line.Stop()
	line.Stop()
	if line.Active() || line.Queue(make([]int16, 100)) != nil || line.Queued() != 0 || line.PressKeys("1") {
		t.Error("stopped line takes audio")
	}
}

func TestLineQueueIsLimited(t *testing.T) {
	line, _, _, _ := newPair(t, -1)
	line.Queue(make([]int16, 10*apiRate))
	if line.Queued() > 4*time.Second {
		t.Errorf("queued %v", line.Queued())
	}
}
