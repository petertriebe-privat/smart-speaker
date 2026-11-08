package peer

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Calls between the Echos (phase 4 of devices/echo/docs/18-several-echos.md): a secondary device asks
// the primary one to run the tools that change the shared state (memory, tasks, people), to
// take a finished conversation's transcript, and for the prompt's context. HTTP on PEER_PORT+1;
// request and answer carry
//
//	X-Jarvis-Auth: <unix ms> <nonce> <hex HMAC-SHA256 of "<ms> <nonce> <path> <body>">
//
// under PEER_KEY. A request older than maxSkew or with a nonce seen before is refused.

const authHeader = "X-Jarvis-Auth"

// Handler: one call's answer to its JSON body.
type Handler func(body []byte) ([]byte, error)

// ErrNoPrimary: no primary device is reachable.
var ErrNoPrimary = errors.New("no primary device reachable")

type rpc struct {
	mu       sync.Mutex
	handlers map[string]Handler
	streams  map[string]AudioHandler // calls that become audio links (stream.go)
	nonces   map[string]time.Time
}

// Handle: path ("/tool") answers with h on this device.
func (s *Service) Handle(path string, h Handler) {
	s.rpc.mu.Lock()
	defer s.rpc.mu.Unlock()
	if s.rpc.handlers == nil {
		s.rpc.handlers = map[string]Handler{}
	}
	s.rpc.handlers[path] = h
}

func (s *Service) sign(ms int64, nonce, path string, body []byte) string {
	mac := hmac.New(sha256.New, s.key)
	fmt.Fprintf(mac, "%d %s %s ", ms, nonce, path)
	mac.Write(body)
	return strconv.FormatInt(ms, 10) + " " + nonce + " " + hex.EncodeToString(mac.Sum(nil))
}

// verify: whether header signs path and body, fresh and not seen before.
func (s *Service) verify(header, path string, body []byte, now time.Time) bool {
	parts := strings.Fields(header)
	if len(parts) != 3 {
		return false
	}
	ms, err := strconv.ParseInt(parts[0], 10, 64)
	if err != nil {
		return false
	}
	if d := now.Sub(time.UnixMilli(ms)); d > maxSkew || d < -maxSkew {
		return false
	}
	if !hmac.Equal([]byte(s.sign(ms, parts[1], path, body)), []byte(header)) {
		return false
	}
	s.rpc.mu.Lock()
	defer s.rpc.mu.Unlock()
	if s.rpc.nonces == nil {
		s.rpc.nonces = map[string]time.Time{}
	}
	key := path + " " + parts[1]
	if _, seen := s.rpc.nonces[key]; seen {
		return false
	}
	for k, at := range s.rpc.nonces {
		if now.Sub(at) > 2*maxSkew {
			delete(s.rpc.nonces, k)
		}
	}
	s.rpc.nonces[key] = now
	return true
}

func (s *Service) serveHTTP(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
	if err != nil || r.Method != http.MethodPost || !s.verify(r.Header.Get(authHeader), r.URL.Path, body, time.Now()) {
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	}
	// An audio link stays with the device that takes it, primary or not: that one plays it or
	// passes the audio on itself (stream.go).
	if h := s.audioHandler(r.URL.Path); h != nil {
		s.serveAudio(w, r, body, h)
		return
	}
	if r.Header.Get(relayHeader) != "" && !s.isPrimary() {
		s.relay(w, r, body)
		return
	}
	s.rpc.mu.Lock()
	h := s.rpc.handlers[r.URL.Path]
	s.rpc.mu.Unlock()
	if h == nil {
		http.Error(w, "unknown", http.StatusNotFound)
		return
	}
	out, err := h(body)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.Header().Set(authHeader, s.sign(time.Now().UnixMilli(), randomID(), "answer "+r.URL.Path, out))
	w.Write(out)
}

// serveRPC: the HTTP side of the link, until ctx ends.
func (s *Service) serveRPC(ctx context.Context) {
	srv := &http.Server{Addr: bindHost + ":" + strconv.Itoa(s.port+1), Handler: http.HandlerFunc(s.serveHTTP),
		ReadHeaderTimeout: 5 * time.Second}
	go func() {
		<-ctx.Done()
		srv.Close()
	}()
	for ctx.Err() == nil {
		if err := srv.ListenAndServe(); err != nil && ctx.Err() == nil {
			log.Printf("[Peer] calls on %d: %v", s.port+1, err)
			time.Sleep(30 * time.Second)
		}
	}
}

// Leader: the primary device among the peers present (nil: none, or this one is it). On the phone
// away from home (no heartbeats) the primary behind the home tunnel (SetRemote).
func (s *Service) Leader() *Peer {
	if s == nil || s.isPrimary() {
		return nil
	}
	for _, p := range s.Alive() {
		if p.Primary {
			return &p
		}
	}
	s.mu.Lock()
	remote := s.remote
	s.mu.Unlock()
	if host, port, err := net.SplitHostPort(remote); err == nil {
		n, _ := strconv.Atoi(port)
		return &Peer{Name: "home", Primary: true, Addr: host, Port: n}
	}
	return nil
}

// SetRemote: the primary's calls behind the phone's home tunnel ("127.0.0.1:port",
// internal/hometunnel). The gateway hands them to an Echo, which passes them on to the primary when
// it is not the primary itself (relayHeader).
func (s *Service) SetRemote(hostPort string) {
	s.mu.Lock()
	s.remote = hostPort
	s.mu.Unlock()
}

// relayHeader: a call from outside the LAN (the phone over the gateway): the device that takes it
// passes it on to the primary unless it is the primary.
const relayHeader = "X-Jarvis-Relay"

// relay: the call on the primary for a device that is not; its answer signed by this one.
func (s *Service) relay(w http.ResponseWriter, r *http.Request, body []byte) {
	out, err := s.CallPrimary(r.Context(), r.URL.Path, body)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}
	w.Header().Set(authHeader, s.sign(time.Now().UnixMilli(), randomID(), "answer "+r.URL.Path, out))
	w.Write(out)
}

func (s *Service) isPrimary() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.Primary
}

var client = &http.Client{Timeout: 20 * time.Second}

// CallPrimary: path on the primary device with the JSON body; its answer.
func (s *Service) CallPrimary(ctx context.Context, path string, body []byte) ([]byte, error) {
	p := s.Leader()
	if p == nil {
		return nil, ErrNoPrimary
	}
	return s.CallPeer(ctx, *p, path, body)
}

// CallPeer: path on the device p (one of Alive) with the JSON body; its answer.
func (s *Service) CallPeer(ctx context.Context, p Peer, path string, body []byte) ([]byte, error) {
	port := p.Port
	if port == 0 {
		port = s.port + 1
	}
	return s.call(ctx, net.JoinHostPort(p.Addr, strconv.Itoa(port)), path, body)
}

func (s *Service) call(ctx context.Context, hostPort, path string, body []byte) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, "http://"+hostPort+path, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set(authHeader, s.sign(time.Now().UnixMilli(), randomID(), path, body))
	s.mu.Lock()
	if hostPort == s.remote {
		req.Header.Set(relayHeader, "1")
	}
	s.mu.Unlock()
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	out, err := io.ReadAll(io.LimitReader(resp.Body, 32<<20)) // the state's mirror is a few MB
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("%s: %d %s", path, resp.StatusCode, strings.TrimSpace(string(out)))
	}
	if !s.verify(resp.Header.Get(authHeader), "answer "+path, out, time.Now()) {
		return nil, fmt.Errorf("%s: answer not signed", path)
	}
	return out, nil
}
