// Package phone: The assistant as an IP phone of the FritzBox, as devices/atoms3r/src/phone/phone.cpp and
// rtp.cpp (SIP over UDP in the LAN, SIP_USER / SIP_PASSWORD in the secrets). He registers at
// the registrar and answers every call, talking through a GPT Live session whose audio runs over
// the call (*Line, a live.Output). Released numbers (PHONE_CALLERS; without a list only the
// FritzBox's internal phones "**6xx") get the assistant with memory and all tools, optionally only after
// a PIN typed on the keypad (PHONE_PIN, ends with #). Anyone else is a guest: no memory, no tools
// but hanging up; the assistant takes a message and the user gets a report. The caller hears it ring
// until the session is up. A hang-up on either side ends both.
// The assistant also calls (phone_call): a person of the household as itself, or anyone else for the
// user (a reservation: the far end gets no memory and no tools, the one who asked a report
// afterwards); and he hangs up himself (end_call, registered by package live).
//
// Contract for main: create it with New, register its tools with AddTools, start Run only when
// this Echo is the primary device (the M5 uses the same SIP account), and pass the end of every
// conversation to CallEnded (from live.Hooks.OnEnd).
package phone

import (
	"context"
	"log"
	"net"
	"strconv"
	"strings"
	"sync"
	"time"

	"echo-assistant/internal/config"
	"echo-assistant/internal/live"
	"echo-assistant/internal/people"
	"echo-assistant/internal/prompts"
	"echo-assistant/internal/textutil"
)

// defaultRegistrar: a FritzBox's default address (the M5's SIP_REGISTRAR default).
const defaultRegistrar = "192.168.178.1"

// Session: what the phone needs from the conversation; *live.Session satisfies it.
type Session interface {
	Start(live.Request) bool
	FeedLine([]int16)
	Close()
	Commentary(string)
	Open() bool
	OnPhone() bool
	Request() live.Request
	State() live.State
	SetPaused(bool)
	Paused() bool
	EndSoon()
}

// Deps: what lives elsewhere. Every field may be nil (the feature is then skipped).
type Deps struct {
	// DisplayName: this speaker's name in the From header ("Jarvis Küche" <sip:...>): the FritzBox
	// and the called party show it (#65). "" = the SIP user alone.
	DisplayName string
	// CallerName: the name of an address-book contact for a normalized phone number ("" if none);
	// google.Account.CallerName. Called while an incoming call rings, from a non-household number.
	CallerName func(number string) string
	// LogEvent: a contact without a conversation for the activity log (memory.LogEvent; channels
	// memory.PhoneIn / PhoneOut / CallFor / Guest): missed calls, failed calls.
	LogEvent func(person, channel, title, summary string)
	// RunNow: queues a one-off agent task (tasks.Scheduler.RunNow): the report on a finished call
	// to someone else or from a guest. kind is "prompt".
	RunNow func(kind, title, text, deliver, person string)
	// Message: sends plain text to a person ("" = the admin) on their chat channel
	// (people.Register.Message): the notice that a call the assistant made did not go through.
	Message func(person, text string) string
	// TelegramConfigured: whether Telegram works; chooses how a call report is delivered and what
	// phone_call tells the model. nil = assume yes.
	TelegramConfigured func() bool
	// Delivery: overrides how a call report reaches a person ("" = the admin): a deliver value of
	// tasks.RunNow ("telegram", "email", "call", ...). nil = Telegram if the person has an
	// account, else mail, else a call back (as the M5).
	Delivery func(person string) string
	// Speaker: the speaker at home (audio.Speaker), for a call on the speaker (bridge.go).
	// nil = the speaker tools refuse: no listening in, no intercom.
	Speaker Speaker
	// OtherRoom: connect_speaker with a room: elsewhere false when room is this speaker's (then
	// the call goes on its own room), else that Echo is asked to fetch the call's room in mode
	// (monitor or talk) and answer is for the tool. nil = only this speaker's room.
	OtherRoom func(room, mode string) (answer string, elsewhere bool)
	// HomeBusy: a conversation at the speaker runs. The call then leaves the room alone: the
	// speaker belongs to that conversation and the far end must not hear it. nil = never busy.
	HomeBusy func() bool
	// Say: speaks a German text at home (agent.Announcer.Speak, text to speech without a GPT Live
	// session). A call the user makes themselves (dial_for_me) has no conversation that could tell
	// them that nobody answers. nil = they are told by message instead.
	Say func(text string)
	// Speech: German text to speech, 24 kHz PCM (openai.Speech): the announcements of a call on
	// the mobile phone, made once and kept (mobile_ring.go). nil = spoken through Say each time.
	Speech func(text string) ([]int16, error)
	// Duck: the music goes down while the mobile phone rings or a call on it runs (true), and back
	// up after (false). nil = the music is left alone.
	Duck func(on bool)
	// HomeNote: a system note into the conversation at the speaker (live.Session.Commentary):
	// the mobile phone rings while someone talks with the assistant. nil = it only shows on the ring.
	HomeNote func(text string)
	// Judge: how far a call for the user got, from its task and transcript: the answer to
	// CALL_OUTCOME_INSTRUCTIONS (parseOutcome), "" on an error. Blocks a few seconds; called
	// outside the lock. nil = no retries after an answered call (followup.go).
	Judge func(task, transcript string) string
}

// stage of a call: incoming Connecting (ringing while GPT Live starts) or Pin, then Talking;
// outgoing Dialing (GPT Live starts while it rings at the other end), then Talking; Waiting: a
// call came in during a conversation at home, it rings until that one is over. Bridge: the call
// belongs to the speaker and the microphone at home, GPT Live is over (bridge.go). Ringing: a
// secondary device rings along while the primary one answers (ringing.go).
type stage int

const (
	stageNone stage = iota
	stageWaiting
	stageRinging
	stagePin
	stageConnecting
	stageDialing
	stageTalking
	stageBridge
)

// Settings of the M5's config.h.
var (
	sipPort        = prompts.Int("SIP_PORT", 5060)
	rtpPort        = prompts.Int("PHONE_RTP_PORT", 40000)
	registerSecs   = prompts.Int("PHONE_REGISTER_S", 300)  // registration renewed after 80 % of this
	retryDelay     = ms("PHONE_RETRY_MS", 60000)           // registration failed: next try
	connectTimeout = ms("PHONE_CONNECT_TIMEOUT_MS", 20000) // it rings at most this long while GPT Live starts
	pinTimeout     = ms("PHONE_PIN_TIMEOUT_MS", 30000)
	ringTimeout    = ms("PHONE_RING_TIMEOUT_MS", 30000)    // The assistant calls: nobody picks up after this long
	callWait       = ms("PHONE_CALL_WAIT_MS", 30*60*1000)  // a requested call waits for a running conversation
	waitingFor     = ms("PHONE_WAITING_MS", 60000)         // a call during a conversation at home rings this long
	bridgeMaxCall  = ms("PHONE_BRIDGE_MAX_MS", 60*60*1000) // a call that is only the bridge hangs up after this
	bridgeWait     = ms("PHONE_BRIDGE_WAIT_MS", 20000)     // dial_for_me waits this long for a running call
	fallbackAnswer = ms("PHONE_FALLBACK_ANSWER_MS", 20000) // a secondary answers a call nobody took after this
	lineRingEvery  = ms("PHONE_RING_EVERY_MS", 4000)       // a secondary rings along: two tones this often
)

func ms(name string, def int) time.Duration {
	return time.Duration(prompts.Int(name, def)) * time.Millisecond
}

type registration struct {
	callID, tag string
	cseq        int
	renewAt     time.Time // zero = now
	sentAt      time.Time
	until       time.Time // when the registration runs out
	waiting     bool
	registered  bool
	authSent    bool
	challenge   Challenge
	others      int // other devices registered with the same account (the FritzBox rings them all)
}

// pendingReport: after a call for someone else (or from a guest): the agent tells whoever asked
// how it went.
type pendingReport struct {
	line     *Line
	number   string
	task     string
	person   string // a household call: who was called (the transcript's "Du")
	asker    string // who asked, "" = the admin
	since    time.Time
	closedAt time.Time // first tick with the session closed
	followup bool      // a call for the user outside the household, or its call back (followup.go)
	callback bool      // they called back
	byCall   bool      // the report goes to the asker by a call
}

type callRequest struct {
	number, reason, person, by string
	name                       string // dial_for_me: who is called, for the notice at home
	stranger                   bool
	direct                     bool       // dial_for_me: straight to the speaker, no GPT Live session
	probe                      *echoProbe // the echo test (echotest.go)
	listen                     bool       // phone_call listen: on the speaker from the pick-up
	reportCall                 bool       // phone_call report "call": the result by a call back
	retry                      bool       // a retry of a call that did not reach its goal (followup.go)
	at                         time.Time
}

// nameOrNumber: who was to be called, for a notice at home.
func (r *callRequest) nameOrNumber() string {
	if r.name != "" {
		return r.name
	}
	return r.number
}

// Phone: the SIP client.
type Phone struct {
	user, password, registrar, pin string
	callers                        []string
	owner                          string
	people                         *people.Register
	session                        Session
	deps                           Deps
	bridge                         *bridge   // the call on the speaker and microphone at home
	mic                            *micChain // the microphone at home on its way to the line
	// secondary: another Echo is the primary device; this one rings along and answers only on
	// the button, or when it is the only one registered (SetSecondary, ringing.go).
	secondary bool

	mu       sync.Mutex // everything below
	conn     *net.UDPConn
	localIP  string
	regAddr  *net.UDPAddr
	reg      registration
	call     call
	invite   *Message // the INVITE of an incoming call, kept for the final responses sent later
	wanted   *callRequest
	reports  []*pendingReport
	speakers []speakerEntry // released for the speaker and microphone at home (access.go)
	// followups: the calls for the user outside the household, per number (followup.go)
	followups []*followup
	finished  map[*Line]finishedCall
	mob       mobileState // the mobile phone's call over Bluetooth (mobile.go)
	clips     clipStore   // its announcements (mobile_ring.go), own lock
	conf      *conference // The assistant in the call on the speaker (conference.go)

	hf     handsfree      // oFono; nil = no mobile phone (EnableMobile)
	newSCO func() scoLine // the audio of a mobile call
}

type finishedCall struct {
	ended live.Ended
	at    time.Time
}

// New: secrets are SIP_USER, SIP_PASSWORD, SIP_REGISTRAR (default: a FritzBox's address),
// PHONE_CALLERS (comma separated), PHONE_PIN, PHONE_OWNER.
func New(secrets config.Secrets, register *people.Register, session Session, deps Deps) *Phone {
	p := &Phone{
		user: secrets.String("SIP_USER"), password: secrets.String("SIP_PASSWORD"),
		registrar: secrets.String("SIP_REGISTRAR"), pin: secrets.String("PHONE_PIN"),
		owner:  textutil.PhoneNormalized(secrets.String("PHONE_OWNER")),
		people: register, session: session, deps: deps,
		finished: map[*Line]finishedCall{},
	}
	if p.registrar == "" {
		p.registrar = defaultRegistrar
	}
	p.bridge = newBridge(deps.Speaker, deps.HomeBusy)
	p.mic = newMicChain()
	p.bridge.over = p.micReport
	p.loadSpeakerAccess()
	p.loadFollowups()
	for _, entry := range strings.Split(secrets.String("PHONE_CALLERS"), ",") {
		if n := textutil.PhoneNormalized(entry); n != "" {
			p.callers = append(p.callers, n)
		}
	}
	p.reg.callID = randomHex(12) + "@assistant"
	p.reg.tag = randomHex(4)
	return p
}

// Configured: SIP_USER and SIP_PASSWORD are set.
func (p *Phone) Configured() bool { return p.user != "" && p.password != "" }

// Registered: the registrar knows us (calls can come in and go out).
func (p *Phone) Registered() bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.reg.registered
}

// InCall: ringing or talking.
func (p *Phone) InCall() bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.call.stage != stageNone
}

// Status: registration and call, for the console.
func (p *Phone) Status() string {
	if !p.Configured() {
		return "[Phone] not set up (SIP_USER / SIP_PASSWORD in the secrets)"
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	state, number := "NOT registered", "none"
	if p.reg.registered {
		state = "registered"
	}
	if p.call.stage != stageNone {
		number = p.call.number
	}
	return "[Phone] " + state + " at " + p.registrar + ", call: " + number
}

// Run registers, listens and runs the calls until ctx ends. Start it as a goroutine on every
// Echo: the FritzBox rings all devices registered with the account, and a secondary one only
// rings along (SetSecondary). It returns at once if the phone is not configured.
func (p *Phone) Run(ctx context.Context) {
	if !p.Configured() {
		log.Print("[Phone] not set up (SIP_USER / SIP_PASSWORD)")
		return
	}
	for ctx.Err() == nil {
		if err := p.serve(ctx); err != nil {
			log.Printf("[Phone] %v", err)
		}
		select {
		case <-ctx.Done():
		case <-time.After(5 * time.Second):
		}
	}
}

// localIPFor: the address of this host towards the registrar.
func localIPFor(to *net.UDPAddr) (string, error) {
	c, err := net.DialUDP("udp4", nil, to)
	if err != nil {
		return "", err
	}
	defer c.Close()
	return c.LocalAddr().(*net.UDPAddr).IP.String(), nil
}

type datagram struct {
	data []byte
	from *net.UDPAddr
}

// serve: one binding of the SIP port; returns when the address changed (register again), the
// socket failed or ctx ended.
func (p *Phone) serve(ctx context.Context) error {
	regAddr, err := net.ResolveUDPAddr("udp4", net.JoinHostPort(p.registrar, strconv.Itoa(sipPort)))
	if err != nil {
		return err
	}
	ip, err := localIPFor(regAddr)
	if err != nil {
		return err
	}
	conn, err := net.ListenUDP("udp4", &net.UDPAddr{IP: listenIP, Port: sipPort})
	if err != nil {
		return err
	}
	p.mu.Lock()
	p.conn, p.localIP, p.regAddr = conn, ip, regAddr
	p.reg.renewAt, p.reg.waiting = time.Time{}, false
	p.mu.Unlock()
	defer func() {
		p.mu.Lock()
		if p.call.stage != stageNone {
			p.hangUp("shutdown")
		}
		p.conn = nil
		p.reg.registered = false
		p.mu.Unlock()
		conn.Close()
	}()

	packets := make(chan datagram, 64)
	go func() {
		buf := make([]byte, 4096)
		for {
			n, from, err := conn.ReadFromUDP(buf)
			if err != nil {
				close(packets)
				return
			}
			packets <- datagram{append([]byte(nil), buf[:n]...), from}
		}
	}()
	log.Printf("[Phone] listening on %s:%d, registrar %s", ip, sipPort, p.registrar)

	tick := time.NewTicker(100 * time.Millisecond)
	defer tick.Stop()
	lastCheck := time.Now()
	for {
		select {
		case <-ctx.Done():
			return nil
		case d, ok := <-packets:
			if !ok {
				return net.ErrClosed
			}
			if msg, ok := Parse(string(d.data)); ok {
				p.mu.Lock()
				if msg.Request {
					p.onRequest(msg, d.from)
				} else {
					p.onResponse(msg)
				}
				p.mu.Unlock()
			}
		case now := <-tick.C:
			p.mu.Lock()
			p.updateRegistration(now)
			p.retryIfDue(now)
			p.dialIfRequested(now)
			p.updateCall(now)
			p.reportIfDue(now)
			p.mu.Unlock()
			if now.Sub(lastCheck) > 10*time.Second {
				lastCheck = now
				if current, err := localIPFor(regAddr); err != nil || current != ip {
					return nil // new address: bind and register again
				}
			}
		}
	}
}

// --- Transport --------------------------------------------------------------------------------

func (p *Phone) send(text string, to *net.UDPAddr) {
	if p.conn == nil || to == nil {
		return
	}
	p.conn.WriteToUDP([]byte(text), to)
}

func (p *Phone) contact() string {
	return "<sip:" + p.user + "@" + p.localIP + ":" + strconv.Itoa(sipPort) + ">"
}

func (p *Phone) via() string {
	return "SIP/2.0/UDP " + p.localIP + ":" + strconv.Itoa(sipPort) + ";branch=z9hG4bK" + randomHex(8) + ";rport"
}

// --- Registration -----------------------------------------------------------------------------

// from: this speaker's address with its display name, for the From header.
func (p *Phone) from() string {
	address := "<sip:" + p.user + "@" + p.registrar + ">"
	name := strings.NewReplacer(`"`, "", `\`, "", "\r", "", "\n", "").Replace(strings.TrimSpace(p.deps.DisplayName))
	if name == "" {
		return address
	}
	return `"` + name + `" ` + address
}

func (p *Phone) sendRegister(withAuth bool) {
	r := &p.reg
	r.cseq++
	uri := "sip:" + p.registrar
	address := "<sip:" + p.user + "@" + p.registrar + ">"
	text := "REGISTER " + uri + " SIP/2.0\r\nVia: " + p.via() + "\r\nMax-Forwards: 70\r\nFrom: " + p.from() + ";tag=" +
		r.tag + "\r\nTo: " + address + "\r\nCall-ID: " + r.callID + "\r\nCSeq: " + strconv.Itoa(r.cseq) +
		" REGISTER\r\nContact: " + p.contact() + "\r\nExpires: " + strconv.Itoa(registerSecs) +
		"\r\nUser-Agent: smart-speaker\r\n"
	if withAuth {
		text += r.challenge.Authorization(p.user, p.password, "REGISTER", uri)
	}
	text += "Content-Length: 0\r\n\r\n"
	p.send(text, p.regAddr)
	r.authSent, r.waiting, r.sentAt = withAuth, true, time.Now()
}

func (p *Phone) onRegisterResponse(m *Message) {
	r := &p.reg
	// A repeated answer to an earlier REGISTER (e.g. the 401 before we sent the password).
	if m.Status < 200 || m.CSeqNumber() != r.cseq {
		return
	}
	r.waiting = false
	if (m.Status == 401 || m.Status == 407) && !r.authSent {
		r.challenge = parseChallenge(m)
		p.sendRegister(true)
		return
	}
	if m.Status == 200 {
		// The answer lists every device registered with this account (several Echos, all of
		// them ring): ours gives the expiry, the others tell whether this one is alone.
		all := bindings(m)
		own, others := "", len(all)
		for _, b := range all {
			if uriOf(b) == uriOf(p.contact()) {
				own, others = b, len(all)-1
			}
		}
		if own == "" && len(all) == 1 { // a registrar that rewrites the contact: the one is ours
			own, others = all[0], 0
		}
		if others != r.others {
			log.Printf("[Phone] other devices registered with this account: %d", others)
		}
		r.others = others
		expires, _ := strconv.Atoi(digestParam(own, "expires"))
		if expires <= 0 {
			expires, _ = strconv.Atoi(m.Header("expires"))
		}
		if expires <= 0 {
			expires = registerSecs
		}
		if !r.registered {
			log.Printf("[Phone] registered at the FritzBox as %s (%d s)", p.user, expires)
		}
		r.registered = true
		r.until = time.Now().Add(time.Duration(expires) * time.Second)
		r.renewAt = time.Now().Add(time.Duration(max(30, expires*8/10)) * time.Second)
		return
	}
	// A second 401 after sending the password: the password (or user) is wrong.
	hint := ""
	if m.Status == 401 || m.Status == 403 {
		hint = " (check SIP_USER / SIP_PASSWORD)"
	}
	log.Printf("[Phone] registration refused: %d %s%s", m.Status, m.Reason, hint)
	r.registered = false
	r.renewAt = time.Now().Add(retryDelay)
}

// bindings: the contacts of a REGISTER's 200 OK, one per header or comma separated.
func bindings(m *Message) []string {
	var out []string
	for _, h := range m.All("contact") {
		for _, b := range strings.Split(h, ",") {
			if b = strings.TrimSpace(b); b != "" {
				out = append(out, b)
			}
		}
	}
	return out
}

func (p *Phone) updateRegistration(now time.Time) {
	r := &p.reg
	if r.registered && now.After(r.until) {
		log.Print("[Phone] registration expired")
		r.registered = false
	}
	if r.waiting && now.Sub(r.sentAt) > 5*time.Second { // no answer: the FritzBox is unreachable
		r.waiting = false
		r.renewAt = now.Add(retryDelay)
		log.Print("[Phone] the FritzBox does not answer the registration")
	}
	if !r.waiting && (r.renewAt.IsZero() || !now.Before(r.renewAt)) {
		p.sendRegister(false)
	}
}
