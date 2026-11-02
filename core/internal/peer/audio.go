package peer

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/binary"
	"log"
	"math"
	"net"
	"strconv"
	"sync"
)

// Audio between two Echos (phase 7: a call on one Echo heard and joined at another). UDP on
// PEER_PORT+2, one datagram per 20 ms frame:
//
//	"A1" | 8 bytes HMAC-SHA256 (session key) | session u32 | seq u32 | kind | payload
//
// kinds: 'p' PCM s16le, 't' a tone (freq f32, ms u16, amp f32), 'f' flush, 'e' end. The session
// key is random per link and passes over the signed calls (rpc.go). Late or repeated frames are
// dropped (seq).

const audioMagic = "A1"

// Audio kinds.
const (
	AudioPCM   = 'p'
	AudioTone  = 't'
	AudioFlush = 'f'
	AudioEnd   = 'e'
)

// AudioLink: one direction pair with another device. Set the On* before frames arrive.
type AudioLink struct {
	Session uint32
	OnPCM   func(pcm []int16)
	OnTone  func(freq float64, ms int, amp float64)
	OnFlush func()
	OnEnd   func()

	s      *Service
	key    []byte
	remote *net.UDPAddr
	mu     sync.Mutex
	seq    uint32
	last   uint32 // highest seq received
}

type audioSocket struct {
	mu    sync.Mutex
	conn  *net.UDPConn
	links map[uint32]*AudioLink
}

// AudioPort: where this device receives audio.
func (s *Service) AudioPort() int { return s.port + 2 }

// NewAudioLink: a link to host:port under key; frames for session go to its On* functions.
func (s *Service) NewAudioLink(session uint32, key []byte, host string, port int) (*AudioLink, error) {
	if err := s.audioListen(); err != nil {
		return nil, err
	}
	addr, err := net.ResolveUDPAddr("udp4", net.JoinHostPort(host, strconv.Itoa(port)))
	if err != nil {
		return nil, err
	}
	l := &AudioLink{Session: session, s: s, key: key, remote: addr}
	s.audio.mu.Lock()
	s.audio.links[session] = l
	s.audio.mu.Unlock()
	return l, nil
}

// Close: the link takes no more frames (the other side is told with AudioEnd by the caller).
func (l *AudioLink) Close() {
	l.s.audio.mu.Lock()
	delete(l.s.audio.links, l.Session)
	l.s.audio.mu.Unlock()
}

func (s *Service) audioListen() error {
	s.audio.mu.Lock()
	defer s.audio.mu.Unlock()
	if s.audio.conn != nil {
		return nil
	}
	conn, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.ParseIP(bindHost), Port: s.AudioPort()})
	if err != nil {
		return err
	}
	s.audio.conn, s.audio.links = conn, map[uint32]*AudioLink{}
	go s.audioReceive(conn)
	return nil
}

func (s *Service) audioReceive(conn *net.UDPConn) {
	buf := make([]byte, 8192)
	for {
		n, _, err := conn.ReadFromUDP(buf)
		if err != nil {
			log.Printf("[Peer] audio: %v", err)
			return
		}
		if n < 19 || string(buf[:2]) != audioMagic {
			continue
		}
		session := binary.BigEndian.Uint32(buf[10:14])
		s.audio.mu.Lock()
		l := s.audio.links[session]
		s.audio.mu.Unlock()
		if l != nil {
			l.receive(buf[:n])
		}
	}
}

func (l *AudioLink) mac(body []byte) []byte {
	m := hmac.New(sha256.New, l.key)
	m.Write(body)
	return m.Sum(nil)[:8]
}

func (l *AudioLink) receive(data []byte) {
	body := data[10:]
	if !hmac.Equal(data[2:10], l.mac(body)) {
		return
	}
	seq := binary.BigEndian.Uint32(body[4:8])
	l.mu.Lock()
	if seq <= l.last && l.last-seq < 1<<30 {
		l.mu.Unlock()
		return // late or repeated
	}
	l.last = seq
	l.mu.Unlock()
	kind, payload := body[8], body[9:]
	switch kind {
	case AudioPCM:
		if l.OnPCM != nil {
			pcm := make([]int16, len(payload)/2)
			for i := range pcm {
				pcm[i] = int16(binary.LittleEndian.Uint16(payload[2*i:]))
			}
			l.OnPCM(pcm)
		}
	case AudioTone:
		if l.OnTone != nil && len(payload) >= 10 {
			freq := math.Float32frombits(binary.LittleEndian.Uint32(payload))
			ms := binary.LittleEndian.Uint16(payload[4:])
			amp := math.Float32frombits(binary.LittleEndian.Uint32(payload[6:]))
			l.OnTone(float64(freq), int(ms), float64(amp))
		}
	case AudioFlush:
		if l.OnFlush != nil {
			l.OnFlush()
		}
	case AudioEnd:
		if l.OnEnd != nil {
			l.OnEnd()
		}
	}
}

// Send: one frame of kind to the other side.
func (l *AudioLink) Send(kind byte, payload []byte) {
	l.mu.Lock()
	l.seq++
	seq := l.seq
	l.mu.Unlock()
	body := make([]byte, 9+len(payload))
	binary.BigEndian.PutUint32(body[0:], l.Session)
	binary.BigEndian.PutUint32(body[4:], seq)
	body[8] = kind
	copy(body[9:], payload)
	packet := append(append([]byte(audioMagic), l.mac(body)...), body...)
	l.s.audio.mu.Lock()
	conn := l.s.audio.conn
	l.s.audio.mu.Unlock()
	if conn != nil {
		conn.WriteToUDP(packet, l.remote)
	}
}

// SendPCM: audio samples.
func (l *AudioLink) SendPCM(pcm []int16) {
	payload := make([]byte, 2*len(pcm))
	for i, v := range pcm {
		binary.LittleEndian.PutUint16(payload[2*i:], uint16(v))
	}
	l.Send(AudioPCM, payload)
}

// SendTone: a tone the other side plays itself.
func (l *AudioLink) SendTone(freq float64, ms int, amp float64) {
	payload := make([]byte, 10)
	binary.LittleEndian.PutUint32(payload, math.Float32bits(float32(freq)))
	binary.LittleEndian.PutUint16(payload[4:], uint16(ms))
	binary.LittleEndian.PutUint32(payload[6:], math.Float32bits(float32(amp)))
	l.Send(AudioTone, payload)
}
