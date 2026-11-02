// Package peer: the Echos of one household find each other in the LAN and agree who answers
// "Hey Jarvis" (devices/echo/docs/18-several-echos.md, #62).
//
// Every device sends UDP datagrams to the LAN's broadcast address (or to configured addresses):
// a heartbeat every few seconds, and a claim when it heard the wake word. Each datagram is signed
// with HMAC-SHA256 under PEER_KEY (secrets.h): the link starts conversations and later carries
// state and audio, so nothing unsigned counts. Format, one datagram:
//
//	J1 <hex HMAC of the JSON> <JSON Message>
//
// Kept this simple so the M5 can speak it too.
//
// Wake arbitration: the device that heard the wake word starts its conversation at once (no
// delay for the user) and sends a claim with the microphone's level in the second before. Every
// device that heard it too does the same. A device with a conversation open answers a claim
// with its own level over that second (busy): if the user speaks to it, it keeps them. After
// ClaimWindow each claimant decides with the same rule on the same data: the loudest wins (the
// nearest), ties by priority, then by name. The others drop their new conversation before it
// says anything.
package peer

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"log"
	"net"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	magic = "J1"
	// ClaimWindow: how long a claimant waits for the other claims.
	ClaimWindow = 250 * time.Millisecond
	// sameWake: claims this close in time are the same "Hey Jarvis". The M5's model detects up to
	// ~2 s after the Echo's (19.10.2026): 1.5 s let both answer.
	sameWake = 3 * time.Second
	// maxSkew: older or newer datagrams are dropped (replay); the clocks run on SNTP.
	maxSkew = 10 * time.Second
	// heartbeatEvery and aliveFor: a peer counts as present this long after its last heartbeat.
	heartbeatEvery = 5 * time.Second
	aliveFor       = 3 * heartbeatEvery
	// levelWindow: the second before a detection, in which the level is measured.
	levelWindow = time.Second
)

// Message: one datagram's content.
type Message struct {
	Kind    string  `json:"kind"` // "hb" heartbeat, "wake" claim
	From    string  `json:"from"`
	At      int64   `json:"at"`   // unix ms: when sent (hb), the detection (wake)
	Run     string  `json:"run"`  // the sender's process (a restart counts Seq from 1 again)
	Seq     uint64  `json:"seq"`  // per sender and run, against replays
	Prio    int     `json:"prio"` // priority of the sender (ties)
	ID      string  `json:"id,omitempty"`
	Level   float64 `json:"level,omitempty"` // wake: microphone RMS in the second before At
	Score   float64 `json:"score,omitempty"` // wake: the detector's peak
	Busy    bool    `json:"busy,omitempty"`  // wake: the sender has a conversation open, it answers a claim
	State   string  `json:"state,omitempty"` // hb: idle, conversation, call, music
	Room    string  `json:"room,omitempty"`  // hb
	Primary bool    `json:"primary,omitempty"`
	Version string  `json:"version,omitempty"`
	Port    int     `json:"port,omitempty"`    // hb: the calls' HTTP port (rpc.go)
	Epoch   int     `json:"epoch,omitempty"`   // hb: of the primary role (a takeover raises it)
	NoLead  bool    `json:"no_lead,omitempty"` // hb: never takes the primary role (the M5, #66)
	// hb: the presence sensor in the sender's room (the M5's); nil = it has none.
	Occupied *bool `json:"occupied,omitempty"`
}

// Peer: another device as its heartbeats show it.
type Peer struct {
	Name, Room, State, Version string
	Primary                    bool
	NoLead                     bool  // never the primary: left out of the election (failover.go)
	Occupied                   *bool // its room's presence sensor; nil = none
	Addr                       string
	Port                       int // its calls' HTTP port
	Epoch                      int
	Priority                   int
	Seen                       time.Time
}

// Service: this device's end of the link. Fill the fields, then Run.
type Service struct {
	Name     string
	Room     string
	Priority int
	Primary  bool // the role: set before Run, then with SetRole
	Epoch    int
	NoLead   bool // never takes the primary role (the heartbeat says so, as the M5's)
	Version  string
	// Level: the microphone's RMS here between from and to; nil = 0.
	Level func(from, to time.Time) float64
	// Busy: a conversation is open here (for longer than the claim that is answered).
	Busy func() bool
	// State: for the heartbeat ("idle", "conversation", ...).
	State func() string
	// Outvoiced: this device won a wake word, and a louder claim of the same words came after
	// the window (a device whose model detects later: the M5, ~1 s). Drop the conversation.
	Outvoiced func(why string)

	key     []byte
	port    int
	targets []*net.UDPAddr

	run   string      // this process
	rpc   rpc         // calls between the devices (rpc.go)
	audio audioSocket // audio between the devices (audio.go)

	mu     sync.Mutex
	conn   *net.UDPConn
	seq    uint64
	peers  map[string]*Peer
	seen   map[string]seen // per sender
	claims []Message       // received recently
	won    *Message        // this device's last claim, if it won (a late louder one still takes it)
	remote string          // the primary behind the phone's home tunnel (SetRemote)
}

type seen struct {
	run string
	seq uint64
}

// New: key is PEER_KEY; targets are addresses ("host" or "host:port"), empty = the broadcast
// address. Without a key the link is off (nil).
func New(key string, port int, targets []string) *Service {
	if len(key) < 16 {
		return nil
	}
	s := &Service{key: []byte(key), port: port, run: randomID(), peers: map[string]*Peer{}, seen: map[string]seen{}}
	for _, t := range targets {
		if !strings.Contains(t, ":") {
			t += ":" + strconv.Itoa(port)
		}
		if a, err := net.ResolveUDPAddr("udp4", t); err == nil {
			s.targets = append(s.targets, a)
		} else {
			log.Printf("[Peer] %s: %v", t, err)
		}
	}
	if len(s.targets) == 0 {
		s.targets = []*net.UDPAddr{{IP: net.IPv4bcast, Port: port}}
	}
	return s
}

// Run listens and sends heartbeats until ctx ends.
func (s *Service) Run(ctx context.Context) {
	go s.serveRPC(ctx)
	for ctx.Err() == nil {
		conn, err := listen(s.port)
		if err != nil {
			log.Printf("[Peer] port %d: %v", s.port, err)
			select {
			case <-ctx.Done():
			case <-time.After(30 * time.Second):
			}
			continue
		}
		log.Printf("[Peer] listening on %d as %q", s.port, s.Name)
		s.mu.Lock()
		s.conn = conn
		s.mu.Unlock()
		done := make(chan struct{})
		go s.heartbeats(ctx, done)
		s.receive(conn)
		close(done)
		conn.Close()
	}
}

func (s *Service) heartbeats(ctx context.Context, done chan struct{}) {
	t := time.NewTicker(heartbeatEvery)
	defer t.Stop()
	for {
		s.heartbeat()
		select {
		case <-ctx.Done():
			return
		case <-done:
			return
		case <-t.C:
		}
	}
}

// heartbeat: one, now.
func (s *Service) heartbeat() {
	state := ""
	if s.State != nil {
		state = s.State()
	}
	s.mu.Lock()
	primary, epoch := s.Primary, s.Epoch
	s.mu.Unlock()
	s.send(Message{Kind: "hb", State: state, Room: s.Room, Primary: primary, Epoch: epoch, Version: s.Version, Port: s.port + 1,
		NoLead: s.NoLead})
}

// SetRole: this device's role from now on (the heartbeats tell the others).
func (s *Service) SetRole(primary bool, epoch int) {
	s.mu.Lock()
	s.Primary, s.Epoch = primary, epoch
	s.mu.Unlock()
	s.heartbeat()
}

func (s *Service) receive(conn *net.UDPConn) {
	buf := make([]byte, 4096)
	for {
		n, from, err := conn.ReadFromUDP(buf)
		if err != nil {
			log.Printf("[Peer] receive: %v", err)
			return
		}
		m, ok := s.open(buf[:n], time.Now())
		if !ok || m.From == s.Name {
			continue
		}
		s.handle(m, from)
	}
}

// seal: the datagram of m.
func (s *Service) seal(m Message) []byte {
	body, _ := json.Marshal(m)
	mac := hmac.New(sha256.New, s.key)
	mac.Write(body)
	return []byte(magic + " " + hex.EncodeToString(mac.Sum(nil)) + " " + string(body))
}

// open: the message of a datagram, if it is signed with our key, fresh and not seen before.
func (s *Service) open(data []byte, now time.Time) (Message, bool) {
	var m Message
	parts := strings.SplitN(string(data), " ", 3)
	if len(parts) != 3 || parts[0] != magic {
		return m, false
	}
	got, err := hex.DecodeString(parts[1])
	if err != nil {
		return m, false
	}
	mac := hmac.New(sha256.New, s.key)
	mac.Write([]byte(parts[2]))
	if !hmac.Equal(got, mac.Sum(nil)) || json.Unmarshal([]byte(parts[2]), &m) != nil || m.From == "" {
		return m, false
	}
	if d := now.Sub(time.UnixMilli(m.At)); m.Kind == "hb" && (d > maxSkew || d < -maxSkew) ||
		m.Kind == "wake" && (d > maxSkew+sameWake || d < -maxSkew) {
		return m, false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if last, ok := s.seen[m.From]; ok && last.run == m.Run && m.Seq <= last.seq {
		return m, false // a replay
	}
	s.seen[m.From] = seen{m.Run, m.Seq}
	return m, true
}

func (s *Service) send(m Message) {
	s.mu.Lock()
	conn := s.conn
	s.seq++
	m.Seq, m.Run, m.From, m.Prio = s.seq, s.run, s.Name, s.Priority
	if m.At == 0 {
		m.At = time.Now().UnixMilli()
	}
	direct := s.directLocked()
	s.mu.Unlock()
	if conn == nil {
		return
	}
	data := s.seal(m)
	for _, t := range append(s.targets, direct...) {
		if _, err := conn.WriteToUDP(data, t); err != nil {
			log.Printf("[Peer] send to %s: %v", t, err)
		}
	}
}

// directFor: how long a device that went quiet still gets every datagram directly.
const directFor = 10 * time.Minute

// directLocked: the devices seen lately, each once, that are no target already. Every datagram
// goes to them directly as well (the receiver drops the second copy by its sequence number): the
// Echo Dot 2's Wi-Fi loses the LAN's broadcasts after every second group key renewal, for ten
// minutes each (20.10.2026); it took over a primary that was fine and answered the wake word
// together with the M5.
func (s *Service) directLocked() []*net.UDPAddr {
	var out []*net.UDPAddr
	for _, p := range s.peers {
		ip := net.ParseIP(p.Addr)
		if ip == nil || time.Since(p.Seen) > directFor {
			continue
		}
		dup := false
		for _, t := range s.targets {
			dup = dup || t.IP.Equal(ip)
		}
		if !dup {
			out = append(out, &net.UDPAddr{IP: ip, Port: s.port})
		}
	}
	return out
}

func (s *Service) handle(m Message, from *net.UDPAddr) {
	now := time.Now()
	s.mu.Lock()
	p := s.peers[m.From]
	if p == nil {
		p = &Peer{Name: m.From}
		s.peers[m.From] = p
	}
	// A device that is new or back gets a heartbeat at once: a starting one knows its partners
	// within a second, not after heartbeatEvery (the role is decided at the start).
	news := now.Sub(p.Seen) > aliveFor
	if news {
		log.Printf("[Peer] %s at %s", m.From, from.IP)
	}
	p.Seen, p.Addr, p.Priority = now, from.IP.String(), m.Prio
	if m.Kind == "hb" {
		p.Room, p.State, p.Primary, p.Version, p.Port, p.Epoch = m.Room, m.State, m.Primary, m.Version, m.Port, m.Epoch
		p.NoLead, p.Occupied = m.NoLead, m.Occupied
	}
	if m.Kind == "wake" {
		// For the arbitration's tuning: who, how loud, how late.
		log.Printf("[Peer] claim from %s: level %.0f, score %.2f, busy %v, %d ms old", m.From, m.Level, m.Score,
			m.Busy, now.UnixMilli()-m.At)
		s.claims = append(s.claims, m)
		// keep the last few seconds
		s.claims = slices.DeleteFunc(s.claims, func(c Message) bool {
			return now.Sub(time.UnixMilli(c.At)) > maxSkew
		})
	}
	why := s.lateClaim(m, now)
	s.mu.Unlock()
	if news {
		go s.heartbeat()
	}
	if why != "" && s.Outvoiced != nil {
		go s.Outvoiced(why)
	}
	if m.Kind == "wake" && !m.Busy && s.Busy != nil && s.Busy() {
		// Someone heard "Jarvis" while a conversation runs here: how loud was it here?
		at := time.UnixMilli(m.At)
		s.send(Message{Kind: "wake", ID: m.ID, At: m.At, Busy: true, Level: s.level(at)})
	}
}

// lateClaim (s.mu held): a claim m after this device's won claim, of the same words and louder,
// takes them while the words are at most sameWake old; the reason, "" if it does not. The M5 has
// the same rule (devices/atoms3r/src/system/peer.cpp lateClaim).
func (s *Service) lateClaim(m Message, now time.Time) string {
	w := s.won
	if w == nil || m.Kind != "wake" || m.Busy {
		return ""
	}
	if now.Sub(time.UnixMilli(w.At)) > sameWake {
		s.won = nil
		return ""
	}
	d := time.UnixMilli(m.At).Sub(time.UnixMilli(w.At))
	if d > sameWake || d < -sameWake || !beats(m, *w) {
		return ""
	}
	s.won = nil
	return m.From + " louder, late (" + strconv.FormatFloat(m.Level, 'f', 0, 64) + " > " +
		strconv.FormatFloat(w.Level, 'f', 0, 64) + ")"
}

func (s *Service) level(at time.Time) float64 {
	if s.Level == nil {
		return 0
	}
	return s.Level(at.Add(-levelWindow), at)
}

// Alive: the peers heard from recently.
func (s *Service) Alive() []Peer {
	if s == nil {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []Peer
	for _, p := range s.peers {
		if time.Since(p.Seen) < aliveFor {
			out = append(out, *p)
		}
	}
	slices.SortFunc(out, func(a, b Peer) int { return strings.Compare(a.Name, b.Name) })
	return out
}

// Claim: this device heard the wake word (score) at `at`. Returns whether it answers: at once
// when no peer is present, else after ClaimWindow by the rule of the package comment.
func (s *Service) Claim(score float64, at time.Time) (bool, string) {
	if s == nil || len(s.Alive()) == 0 {
		return true, "alone"
	}
	own := Message{Kind: "wake", ID: randomID(), At: at.UnixMilli(), Level: s.level(at), Score: score}
	s.send(own)
	own.From, own.Prio = s.Name, s.Priority
	time.Sleep(ClaimWindow)
	s.mu.Lock()
	others := slices.Clone(s.claims)
	won, why := decide(own, others)
	s.won = nil
	if won {
		s.won = &own
	}
	s.mu.Unlock()
	return won, why
}

// Heard: the highest score another device claimed a wake word with around `at` (0 = none did).
// The same "Jarvis" heard in two rooms was most likely really said (wake word clips, #78).
func (s *Service) Heard(at time.Time) float64 {
	if s == nil {
		return 0
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	best := 0.0
	for _, c := range s.claims {
		if d := time.UnixMilli(c.At).Sub(at); c.From != s.Name && d <= sameWake && d >= -sameWake {
			best = max(best, c.Score)
		}
	}
	return best
}

// decide: whether own wins against the claims of the same wake word in others, and why.
func decide(own Message, others []Message) (bool, string) {
	why := "loudest"
	for _, o := range others {
		d := time.UnixMilli(o.At).Sub(time.UnixMilli(own.At))
		if o.From == own.From || d > sameWake || d < -sameWake || o.Busy && o.ID != own.ID {
			continue // a conversation's answer counts only for the claim it answers
		}
		if beats(o, own) {
			what := "louder"
			if o.Busy {
				what = "louder, in a conversation"
			}
			return false, o.From + " " + what + " (" + strconv.FormatFloat(o.Level, 'f', 0, 64) + " > " +
				strconv.FormatFloat(own.Level, 'f', 0, 64) + ")"
		}
	}
	return true, why
}

// beats: a wins over b.
func beats(a, b Message) bool {
	if a.Level != b.Level {
		return a.Level > b.Level
	}
	if a.Prio != b.Prio {
		return a.Prio > b.Prio
	}
	return a.From < b.From
}

func randomID() string {
	b := make([]byte, 6)
	rand.Read(b)
	return hex.EncodeToString(b)
}
