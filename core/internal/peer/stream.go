package peer

import (
	"bufio"
	"bytes"
	"context"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// Audio over a TCP connection instead of UDP: the phone away from home reaches the house only
// through the home tunnel (internal/hometunnel, services/home-gateway), which carries TCP to the
// Echos' call port. The link starts as a signed call (rpc.go) whose JSON body names the link's
// session and key ("session", "key" in hex); its answer is an ordinary signed answer, and after it
// both sides write the same packets as over UDP (audio.go), each with a 2-byte length in front.
// The connection stays open until one side closes it; a side that hears nothing for streamIdle
// takes the other as gone (OnEnd).

const (
	streamIdle         = 15 * time.Second
	streamWriteTimeout = 2 * time.Second
)

type audioStream struct {
	conn   net.Conn
	mu     sync.Mutex  // one write at a time
	ready  atomic.Bool // the answer is out (the side that answers): frames before it are dropped
	closed atomic.Bool // closed here, or a write failed
}

func (a *audioStream) write(packet []byte) {
	if !a.ready.Load() || a.closed.Load() {
		return
	}
	frame := make([]byte, 2+len(packet))
	binary.BigEndian.PutUint16(frame, uint16(len(packet)))
	copy(frame[2:], packet)
	a.mu.Lock()
	defer a.mu.Unlock()
	a.conn.SetWriteDeadline(time.Now().Add(streamWriteTimeout))
	if _, err := a.conn.Write(frame); err != nil {
		a.conn.Close() // the reader ends and tells OnEnd
	}
}

func (a *audioStream) close() {
	a.closed.Store(true)
	a.conn.Close()
}

// readStream: the packets of the connection to the link's handlers, until it ends. An end that
// was not this side's Close (the other side gone, silent, or its connection broken) is OnEnd.
func (l *AudioLink) readStream(r io.Reader) {
	st := l.stream
	var size [2]byte
	buf := make([]byte, 1<<16)
	for {
		st.conn.SetReadDeadline(time.Now().Add(streamIdle))
		if _, err := io.ReadFull(r, size[:]); err != nil {
			break
		}
		n := int(binary.BigEndian.Uint16(size[:]))
		if _, err := io.ReadFull(r, buf[:n]); err != nil {
			break
		}
		if n >= 19 && string(buf[:2]) == audioMagic {
			l.receive(buf[:n])
		}
	}
	if st.closed.Swap(true) {
		return // closed here
	}
	st.conn.Close()
	l.mu.Lock()
	h := l.h
	l.mu.Unlock()
	if h.OnEnd != nil {
		h.OnEnd()
	}
}

// AudioHandler: a call that becomes an audio link (DialAudio). It answers like a Handler; keep
// false closes the connection after the answer. Frames it sends on link before the answer is out
// are dropped.
type AudioHandler func(body []byte, link *AudioLink) (answer []byte, keep bool)

// HandleAudio: path answers with h on this device and keeps the connection as an audio link.
func (s *Service) HandleAudio(path string, h AudioHandler) {
	s.rpc.mu.Lock()
	defer s.rpc.mu.Unlock()
	if s.rpc.streams == nil {
		s.rpc.streams = map[string]AudioHandler{}
	}
	s.rpc.streams[path] = h
}

func (s *Service) audioHandler(path string) AudioHandler {
	s.rpc.mu.Lock()
	defer s.rpc.mu.Unlock()
	return s.rpc.streams[path]
}

// streamOpen: what every body of an audio call carries.
type streamOpen struct {
	Session uint32 `json:"session"`
	Key     string `json:"key"`
}

func (s *Service) serveAudio(w http.ResponseWriter, r *http.Request, body []byte, h AudioHandler) {
	var q streamOpen
	if json.Unmarshal(body, &q) != nil {
		http.Error(w, "bad body", http.StatusBadRequest)
		return
	}
	key, err := hex.DecodeString(q.Key)
	if err != nil || len(key) < 16 {
		http.Error(w, "bad key", http.StatusBadRequest)
		return
	}
	hj, ok := w.(http.Hijacker)
	if !ok {
		http.Error(w, "no stream", http.StatusInternalServerError)
		return
	}
	conn, rw, err := hj.Hijack()
	if err != nil {
		return
	}
	conn.SetDeadline(time.Time{})
	l := &AudioLink{Session: q.Session, s: s, key: key, stream: &audioStream{conn: conn}}
	answer, keep := h(body, l)
	sig := s.sign(time.Now().UnixMilli(), randomID(), "answer "+r.URL.Path, answer)
	fmt.Fprintf(rw, "HTTP/1.1 200 OK\r\nContent-Type: application/json\r\n%s: %s\r\nContent-Length: %d\r\n\r\n",
		authHeader, sig, len(answer))
	rw.Write(answer)
	conn.SetWriteDeadline(time.Now().Add(streamWriteTimeout))
	if err := rw.Flush(); err != nil {
		conn.Close() // the reader below ends at once and tells OnEnd
	}
	if !keep {
		l.stream.close()
		return
	}
	l.stream.ready.Store(true)
	go l.readStream(rw.Reader)
}

// DialAudio: a call on p (one of Alive, or the primary behind the phone's home tunnel: Leader)
// that becomes an audio link over the same connection. session and key are the link's and must
// be in body as "session" and "key" (hex). The answer is the call's; the link is nil on an error.
func (s *Service) DialAudio(ctx context.Context, p Peer, path string, body []byte, session uint32,
	key []byte) (*AudioLink, []byte, error) {
	port := p.Port
	if port == 0 {
		port = s.port + 1
	}
	hostPort := net.JoinHostPort(p.Addr, strconv.Itoa(port))
	var d net.Dialer
	conn, err := d.DialContext(ctx, "tcp", hostPort)
	if err != nil {
		return nil, nil, err
	}
	fail := func(err error) (*AudioLink, []byte, error) {
		conn.Close()
		return nil, nil, err
	}
	conn.SetDeadline(time.Now().Add(15 * time.Second)) // until the answer is in
	req, err := http.NewRequest(http.MethodPost, "http://"+hostPort+path, bytes.NewReader(body))
	if err != nil {
		return fail(err)
	}
	req.Header.Set(authHeader, s.sign(time.Now().UnixMilli(), randomID(), path, body))
	if err := req.Write(conn); err != nil {
		return fail(err)
	}
	br := bufio.NewReader(conn)
	resp, err := http.ReadResponse(br, req)
	if err != nil {
		return fail(err)
	}
	out, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return fail(err)
	}
	if resp.StatusCode != http.StatusOK {
		return fail(fmt.Errorf("%s: %d %s", path, resp.StatusCode, strings.TrimSpace(string(out))))
	}
	if !s.verify(resp.Header.Get(authHeader), "answer "+path, out, time.Now()) {
		return fail(fmt.Errorf("%s: answer not signed", path))
	}
	conn.SetDeadline(time.Time{})
	l := &AudioLink{Session: session, s: s, key: key, stream: &audioStream{conn: conn}}
	l.stream.ready.Store(true)
	go l.readStream(br)
	return l, out, nil
}

// Streamed: the link goes over TCP (DialAudio, HandleAudio), not over UDP to a device in the LAN.
func (l *AudioLink) Streamed() bool { return l.stream != nil }

// Closed: a streamed link that is over (closed here, or the other side gone).
func (l *AudioLink) Closed() bool { return l.stream != nil && l.stream.closed.Load() }
