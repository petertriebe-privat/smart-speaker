package phone

import (
	"log"
	"net"
	"strconv"
	"strings"
	"time"

	"echo-assistant/internal/live"
	"echo-assistant/internal/memory"
	"echo-assistant/internal/prompts"
	"echo-assistant/internal/textutil"
)

var (
	_ live.Output = (*Line)(nil)
	_ Session     = (*live.Session)(nil)
)

// call: the state of the one call (incoming or outgoing); the zero value is "no call".
type call struct {
	stage     stage
	outgoing  bool
	callID    string
	local     string // From / To headers with tags
	remote    string
	target    string // request URI of in-dialog requests
	route     string
	signal    *net.UDPAddr // the FritzBox
	media     Media
	number    string
	pin       string
	reason    string
	name      string // the caller in the register or address book (incoming)
	person    string // who is on the line for sure (people id), "" = not a person of the household
	requester string // outgoing: who asked for the call (reports go to him), "" = the admin
	stranger  bool   // phone_call to someone else, or a guest calling: no memory, no tools, a report after
	ok        string // incoming: our 200 OK, repeated until the ACK
	ack       string // outgoing: our ACK, repeated when the 200 OK comes again

	answered, acked, greet, ringing, authSent bool
	announced                                 bool       // Waiting: the conversation at home was told
	direct                                    bool       // dial_for_me: the user talks themselves, GPT Live is not in it
	probe                                     *echoProbe // the echo test (echotest.go): no room, no session
	listen                                    bool       // phone_call listen: on the speaker at home from the pick-up
	reportCall                                bool       // outgoing for the user: the report by a call back
	callback                                  bool       // incoming: a number the assistant called for the user calls back
	history                                   string     // the earlier calls with this number, as context (followup.go)

	since, okSentAt time.Time
	bridgeSince     time.Time // stageBridge: the call belongs to the room at home since then
	ringbackAt      time.Time // direct: when the speaker at home last rang along; Ringing: the last tones
	okInterval      time.Duration
	cseq            int    // number of our last request in the dialog
	inviteCseq      int    // outgoing: the INVITE's (CANCEL and the ACK of a failure use them)
	inviteHeader    string // incoming: the INVITE's CSeq header, to tell a repeated INVITE
	branch          string // outgoing: the INVITE's
	challenge       Challenge
	line            *Line
}

func newCall() call { return call{media: Media{DTMF: -1}} }

func (c *call) callerText() string {
	if c.name != "" {
		return c.name
	}
	return "die Nummer " + c.number
}

// calledText: who was called, for a notice at home about a call the user makes themselves.
func (c *call) calledText() string {
	if c.name != "" {
		return c.name
	}
	return c.number
}

// --- Who may call ---------------------------------------------------------------------------

// allowed: whether the assistant serves this number fully (a person of the household, a number of
// PHONE_CALLERS, or without a list the FritzBox's internal phones "**6xx"); everyone else is a guest.
func (p *Phone) allowed(number string) bool {
	caller := textutil.PhoneNormalized(number)
	if caller == "" {
		return false
	}
	if p.people.ByPhone(caller) != "" {
		return true
	}
	internal := strings.HasPrefix(caller, "**") // the FritzBox's own phones
	if len(p.callers) == 0 {
		return internal
	}
	for _, entry := range p.callers {
		if entry == caller || (entry == "**" && internal) {
			return true
		}
	}
	return false
}

// --- Answering and dialling -----------------------------------------------------------------

func (p *Phone) sdpAnswer() string {
	c := &p.call
	return BuildAnswer(p.localIP, rtpPort, c.since.Unix(), c.media.Payload, c.media.DTMF)
}

func (p *Phone) reply(status int, reason, body string) {
	if p.invite == nil {
		return
	}
	p.send(buildResponse(p.invite, status, reason, p.call.local, p.contact(), body), p.call.signal)
}

func (p *Phone) startLine() error {
	c := &p.call
	return c.line.Start(c.media.Addr(), c.media.Payload, c.media.DTMF)
}

func codecName(payload uint8) string {
	if payload == payloadPCMA {
		return "A-law"
	}
	return "u-law"
}

func (p *Phone) answer() {
	c := &p.call
	c.ok = buildResponse(p.invite, 200, "OK", c.local, p.contact(), p.sdpAnswer())
	p.send(c.ok, c.signal)
	c.answered = true
	c.okSentAt = time.Now()
	c.okInterval = 500 * time.Millisecond
	if err := p.startLine(); err != nil {
		log.Printf("[Phone] RTP port %d not available: %v", rtpPort, err)
		p.hangUp("no RTP port")
		return
	}
	log.Printf("[Phone] answered (%s, RTP %s:%d)", codecName(c.media.Payload), c.media.IP, c.media.Port)
}

// request builds the conversation's Request of this call.
func (p *Phone) sessionRequest(reason string) live.Request {
	c := &p.call
	caller := c.number
	if name := p.nameOf(c); name != "" {
		caller = name + " (" + c.number + ")"
	}
	person := c.person
	if c.stranger {
		person = ""
	}
	req := live.Request{Reason: reason, Person: person, Phone: true, Line: c.line, Caller: caller,
		CallerName: p.nameOf(c), Stranger: c.stranger}
	if c.callback {
		req.Callback = c.reason + c.history
	}
	return req
}

// nameOf: how the assistant greets: an incoming call's name from the register or address book; for a
// call the assistant makes to a person of the household, theirs.
func (p *Phone) nameOf(c *call) string {
	if c.outgoing {
		return p.people.Name(c.person)
	}
	return c.name
}

// pickUp: an incoming call gets its session (after the PIN, if one is set).
func (p *Phone) pickUp() {
	c := &p.call
	if p.pin != "" && !c.stranger {
		p.answer()           // the keys come as RTP: only after answering
		if p.call.answered { // else the RTP port failed and the call is gone
			c.stage = stagePin
		}
		return
	}
	c.stage = stageConnecting
	p.session.Start(p.sessionRequest(""))
}

func (p *Phone) onInvite(req *Message, from *net.UDPAddr) {
	callID := req.Header("call-id")
	c := &p.call
	if c.stage != stageNone && callID == c.callID {
		if c.outgoing {
			return
		}
		if req.Header("cseq") == c.inviteHeader { // the INVITE again: our answer got lost
			if c.ok != "" {
				p.send(c.ok, from)
			}
			return
		}
		// re-INVITE (hold, codec change): new media, same answer
		before := p.invite
		p.invite = req
		media, ok := ParseMedia(req.Body)
		if !ok {
			p.reply(488, "Not Acceptable Here", "")
			p.invite = before
			return
		}
		c.media = media
		p.reply(200, "OK", p.sdpAnswer())
		if c.line != nil && c.line.Active() {
			p.startLine()
		}
		return
	}
	number := uriUser(req.Header("from"))
	to := withTag(req.Header("to"))
	if c.stage != stageNone || p.session.OnPhone() {
		log.Printf("[Phone] call from %s: busy", number)
		p.send(buildResponse(req, 486, "Busy Here", to, p.contact(), ""), from)
		return
	}
	// Numbers not in PHONE_CALLERS are answered as guests: no memory, no tools but hanging up,
	// and the user gets a report of who called and what for.
	guest := !p.allowed(number)
	p.call = newCall()
	c = &p.call
	p.invite = req
	media, ok := ParseMedia(req.Body)
	if !ok {
		p.send(buildResponse(req, 488, "Not Acceptable Here", to, p.contact(), ""), from)
		log.Print("[Phone] call without G.711 in the offer refused")
		p.call, p.invite = newCall(), nil
		return
	}
	c.media = media
	c.callID = callID
	c.remote = req.Header("from")
	c.local = to
	c.inviteHeader = req.Header("cseq")
	c.target = uriOf(req.Header("contact"))
	c.route = strings.Join(req.All("record-route"), ", ")
	c.signal = from
	c.number = textutil.PhoneNormalized(number)
	c.person = p.people.ByPhone(c.number)
	c.stranger = guest
	c.since = time.Now()
	c.line = NewLine(rtpPort, p.lineHeard, p.bridge)
	p.send(buildResponse(req, 100, "Trying", req.Header("to"), p.contact(), ""), from)
	// The assistant greets it by name.
	if c.person != "" {
		c.name = p.people.Name(c.person)
	} else if p.deps.CallerName != nil {
		c.name = p.deps.CallerName(c.number)
	}
	p.reply(180, "Ringing", "")
	extra := ""
	if guest {
		extra = " (guest: not in PHONE_CALLERS)"
		// A number the assistant called for the user calls back: it knows why and goes on with the task.
		if f := p.callbackLocked(c.number, time.Now()); f != nil {
			c.callback, c.reason, c.history = true, f.Task, historyText(f)
			extra = " (call back about a call for the user)"
		}
	}
	comma := ""
	if c.name != "" {
		comma = ", "
	}
	log.Printf("[Phone] call from %s%s%s%s", c.number, comma, c.name, extra)
	if p.secondary && p.reg.others > 0 {
		c.stage = stageRinging
		log.Print("[Phone] secondary device: it rings here too, the primary answers (button: take it here)")
		return
	}
	if p.session.Open() {
		// A conversation at home: it rings on while the assistant tells that one (call waiting).
		c.stage = stageWaiting
		log.Print("[Phone] conversation at home running: the call waits")
	} else {
		p.pickUp()
	}
}

// --- Outgoing calls ---------------------------------------------------------------------------

func (p *Phone) sendInvite(withAuth bool) {
	c := &p.call
	c.cseq = c.inviteCseq + 1
	c.inviteCseq = c.cseq
	c.branch = "z9hG4bK" + randomHex(8)
	body := BuildOffer(p.localIP, rtpPort, c.since.Unix())
	text := "INVITE " + c.target + " SIP/2.0\r\nVia: SIP/2.0/UDP " + p.localIP + ":" + strconv.Itoa(sipPort) +
		";branch=" + c.branch + ";rport\r\nMax-Forwards: 70\r\nFrom: " + c.local + "\r\nTo: " + c.remote +
		"\r\nCall-ID: " + c.callID + "\r\nCSeq: " + strconv.Itoa(c.inviteCseq) + " INVITE\r\nContact: " +
		p.contact() + "\r\nUser-Agent: smart-speaker\r\nAllow: INVITE, ACK, BYE, CANCEL, OPTIONS\r\n"
	if withAuth {
		text += c.challenge.Authorization(p.user, p.password, "INVITE", c.target)
	}
	text += "Content-Type: application/sdp\r\nContent-Length: " + strconv.Itoa(len(body)) + "\r\n\r\n" + body
	p.send(text, c.signal)
	c.authSent = withAuth
}

// sendForInvite: CANCEL and the ACK of a failed INVITE go with the INVITE's branch and CSeq number.
func (p *Phone) sendForInvite(method, to string) {
	c := &p.call
	p.send(method+" "+c.target+" SIP/2.0\r\nVia: SIP/2.0/UDP "+p.localIP+":"+strconv.Itoa(sipPort)+";branch="+
		c.branch+";rport\r\nMax-Forwards: 70\r\nFrom: "+c.local+"\r\nTo: "+to+"\r\nCall-ID: "+c.callID+
		"\r\nCSeq: "+strconv.Itoa(c.inviteCseq)+" "+method+"\r\nUser-Agent: smart-speaker\r\nContent-Length: 0\r\n\r\n", c.signal)
}

// dial: asked for by phone_call, started by the loop once no conversation runs.
func (p *Phone) dial(r *callRequest) {
	p.call = newCall()
	c := &p.call
	c.outgoing = true
	c.stranger = r.stranger
	if !r.stranger {
		c.person = r.person
	}
	c.requester = r.by
	c.number = r.number
	c.reason = r.reason
	c.direct = r.direct
	c.probe = r.probe
	c.listen = r.listen
	c.reportCall = r.reportCall
	c.name = r.name
	// A call for the user outside the household: kept per number, with the earlier calls as context.
	if r.stranger && !r.direct && r.probe == nil {
		c.history = historyText(p.followupLocked(r.number))
		p.startFollowupLocked(r, time.Now())
	}
	c.callID = randomHex(12) + "@assistant"
	c.local = p.from() + ";tag=" + randomHex(4)
	c.remote = "<sip:" + r.number + "@" + p.registrar + ">"
	c.target = "sip:" + r.number + "@" + p.registrar
	c.signal = p.regAddr
	c.since = time.Now()
	c.stage = stageDialing
	c.line = NewLine(rtpPort, p.lineHeard, p.bridge)
	if r.probe != nil { // the echo test listens itself, and the room is the phone's call
		c.line = NewLine(rtpPort, r.probe.record, nil)
	}
	p.sendInvite(false)
	// dial_for_me: the user talks themselves, so no GPT Live session is started at all. It rings
	// at the other end, and once they pick up the call goes to the speaker and the microphone at
	// home (onInviteResponse).
	if r.direct {
		log.Printf("[Phone] calling %s for the user themselves", r.number)
		return
	}
	log.Printf("[Phone] calling %s: %s", r.number, r.reason)
	reason := r.reason
	if reason == "" {
		reason = "Der Nutzer wollte angerufen werden."
	}
	p.session.Start(p.sessionRequest(reason + c.history))
}

// minutesText: how long a call took, in German, for the activity log.
func minutesText(d time.Duration) string {
	switch minutes := int(d.Minutes()); {
	case minutes < 1:
		return "keine Minute lang"
	case minutes == 1:
		return "eine Minute lang"
	default:
		return strconv.Itoa(minutes) + " Minuten lang"
	}
}

// say: a notice at home for a call the user makes themselves - there is no conversation that
// could tell them. Falls back to a message if this device cannot speak on its own.
func (p *Phone) say(text string) {
	if p.deps.Say == nil {
		p.tell(text)
		return
	}
	go p.deps.Say(text) // text to speech takes a moment: never in the loop
}

func failureText(status int) string {
	switch status {
	case 486, 600:
		return "besetzt"
	case 480, 408:
		return "nicht erreichbar"
	case 487:
		return "nicht abgenommen"
	case 603:
		return "abgelehnt"
	}
	return "fehlgeschlagen"
}

// failureOutcome: a failed INVITE as the outcome of a call for the user (followup.go).
func failureOutcome(status int) string {
	switch status {
	case 486, 600:
		return outcomeBusy
	case 480, 408, 487, 603: // a declined call is often someone busy right now
		return outcomeNoAnswer
	}
	return outcomeFailed
}

// tell: a failed call Jarvis made belongs in the activity log (asked "Hat Max den Weckanruf
// angenommen?") and goes to whoever asked (Telegram or WhatsApp), without blocking the loop.
func (p *Phone) tell(text string) {
	c := &p.call
	plain := strings.NewReplacer("<code>", "", "</code>", "").Replace(text)
	channel, title := memory.PhoneOut, "Anruf von "+prompts.Name()+" gescheitert"
	if c.stranger {
		channel, title = memory.CallFor, "Anruf im Auftrag gescheitert"
	}
	summary := plain
	if c.reason != "" {
		summary += " Anlass: " + c.reason
	}
	person, requester := c.person, c.requester
	go func() {
		if p.deps.LogEvent != nil {
			p.deps.LogEvent(person, channel, title, summary)
		}
		if p.deps.Message != nil {
			if r := p.deps.Message(requester, plain); strings.HasPrefix(r, "Error") {
				log.Printf("[Phone] notice about the failed call: %s", r)
			}
		}
	}()
}

func (p *Phone) onInviteResponse(m *Message) {
	c := &p.call
	if m.Status < 200 {
		if (m.Status == 180 || m.Status == 183) && !c.ringing {
			c.ringing = true
			log.Print("[Phone] ringing")
		}
		return
	}
	if m.Status >= 300 {
		p.sendForInvite("ACK", m.Header("to")) // every final response to an INVITE is acknowledged
		if (m.Status == 401 || m.Status == 407) && !c.authSent {
			c.challenge = parseChallenge(m)
			p.sendInvite(true)
			return
		}
		log.Printf("[Phone] call %s: %d %s", failureText(m.Status), m.Status, m.Reason)
		if c.direct {
			p.say("Der Anruf bei " + c.calledText() + " war " + failureText(m.Status) + ".")
		} else if c.stranger {
			p.callFailedLocked(failureOutcome(m.Status), "Mein Anruf bei <code>"+c.number+"</code> war "+failureText(m.Status)+".")
		} else {
			p.tell("Ich habe dich angerufen, aber der Anruf war " + failureText(m.Status) + ".")
		}
		if p.session.Open() {
			p.session.Close()
		}
		p.clearCall()
		return
	}
	// 200 OK (again, if our ACK got lost)
	if c.answered {
		p.send(c.ack, c.signal)
		return
	}
	media, ok := ParseMedia(m.Body)
	if !ok {
		log.Print("[Phone] answer without G.711")
		return
	}
	c.media = media
	c.remote = m.Header("to")
	if target := uriOf(m.Header("contact")); target != "" {
		c.target = target
	}
	// A caller's route set is the Record-Route list in reverse.
	routes := m.All("record-route")
	for i := len(routes) - 1; i >= 0; i-- {
		if c.route != "" {
			c.route += ", "
		}
		c.route += routes[i]
	}
	c.ack = p.inDialog("ACK", c.inviteCseq) + "Content-Length: 0\r\n\r\n"
	p.send(c.ack, c.signal)
	c.answered, c.acked = true, true
	c.greet = true // once the session is up, the assistant says why it calls
	c.stage = stageTalking
	if err := p.startLine(); err != nil {
		log.Printf("[Phone] RTP port %d not available: %v", rtpPort, err)
		p.hangUp("no RTP port")
		return
	}
	// dial_for_me: they picked up, so the call belongs to the room at home from now on. The two
	// tones tell whoever asked for it that they can talk.
	if c.probe != nil {
		c.stage, c.bridgeSince, c.greet = stageBridge, time.Now(), false
		log.Printf("[Echotest] %s picked up (%s)", c.number, codecName(c.media.Payload))
		return
	}
	if c.direct {
		c.stage = stageBridge
		c.bridgeSince = time.Now()
		c.greet = false
		// The ringback may still be in the speaker's pipe: it has done its job, the two tones
		// take over. Nothing else plays at home, the room belongs to this call now.
		if !c.ringbackAt.IsZero() && p.deps.Speaker != nil {
			if p.deps.HomeBusy == nil || !p.deps.HomeBusy() {
				p.deps.Speaker.Flush()
			}
		}
		if p.bridge.setMode(modeTalk) {
			p.bridge.announce(true)
		}
		log.Printf("[Phone] %s picked up (%s), the call is at the speaker at home", c.number, codecName(c.media.Payload))
		return
	}
	// "Ich möchte mithören": the call is on the speaker from the pick-up (the conversation that
	// asked for it was over before the call started, so nobody at home could switch it on).
	if c.listen && p.deps.Speaker != nil && p.bridge.setMode(modeListen) {
		p.bridge.announce(true)
		log.Printf("[Phone] picked up (%s), audible at home from the start", codecName(c.media.Payload))
		return
	}
	log.Printf("[Phone] picked up (%s)", codecName(c.media.Payload))
}

// --- Both directions ---------------------------------------------------------------------------

// inDialog: a request within the call (BYE, ACK of a 200 OK): new branch, to the other side's contact.
func (p *Phone) inDialog(method string, cseq int) string {
	c := &p.call
	text := method + " " + c.target + " SIP/2.0\r\nVia: " + p.via() + "\r\nMax-Forwards: 70\r\n"
	if c.route != "" {
		text += "Route: " + c.route + "\r\n"
	}
	return text + "From: " + c.local + "\r\nTo: " + c.remote + "\r\nCall-ID: " + c.callID + "\r\nCSeq: " +
		strconv.Itoa(cseq) + " " + method + "\r\nUser-Agent: smart-speaker\r\n"
}

func (p *Phone) sendBye(withAuth bool) {
	c := &p.call
	c.cseq++
	text := p.inDialog("BYE", c.cseq)
	if withAuth {
		text += c.challenge.Authorization(p.user, p.password, "BYE", c.target)
	}
	p.send(text+"Content-Length: 0\r\n\r\n", c.signal)
}

// clearCall ends the call's media. Calls for someone else (a stranger, or a household call another
// person asked for) and calls of guests leave a report: the one who asked hears how it went,
// without asking (when the conversation's transcript is complete: CallEnded).
func (p *Phone) clearCall() {
	c := &p.call
	asker := c.requester
	if asker == "" {
		asker = p.people.Admin()
	}
	// A call the user had themselves (dial_for_me) leaves no transcript and no task: there is
	// nothing to report, and a report would be an empty message about their own conversation.
	forSomeoneElse := !c.direct && (c.stranger || asker != c.person)
	report := &pendingReport{line: c.line, number: c.number, since: time.Now()}
	switch {
	case c.outgoing && c.answered && forSomeoneElse:
		report.task, report.asker = c.reason, c.requester
		if !c.stranger {
			report.person = c.person
		}
		report.followup, report.byCall = c.stranger && c.probe == nil, c.reportCall
		p.reports = append(p.reports, report)
	case c.stranger && c.answered && !c.direct: // a guest called
		if c.callback { // ... back, about a call for the user: reported like that call
			f := p.followupLocked(c.number)
			report.task, report.followup, report.callback = c.reason, true, true
			if f != nil {
				report.asker, report.byCall = f.Asker, f.ReportCall
			}
		}
		p.reports = append(p.reports, report)
	}
	// A call the user made themselves has no transcript; the activity log gets at least that it
	// happened ("mit wem habe ich heute telefoniert?").
	if c.direct && c.answered && c.probe == nil && p.deps.LogEvent != nil {
		go p.deps.LogEvent(c.requester, memory.PhoneSelf, "Eigenes Telefonat mit "+c.calledText(),
			"Du hast selbst mit "+c.calledText()+" telefoniert, "+minutesText(time.Since(c.since))+
				". "+prompts.Name()+" war nicht im Gespräch, der Inhalt ist ihm unbekannt.")
	}
	if c.line != nil {
		c.line.Stop()
	}
	// The assistant in this call (a landline call on the speaker) leaves with it; one in the mobile
	// phone's call stays. The mobile call has the room from its dialling on, before its audio
	// stands: 22.10.2026 the owner called the landline from the paired phone, a secondary stopped
	// ringing along when the primary answered, and switched off the bridge of the phone's call.
	mobileCall := p.mob.call != "" || p.mob.audio != nil
	if !mobileCall {
		p.endConferenceLocked("the call is over")
	}
	// The room at home belongs to itself again: speaker and microphone back to normal, and a held
	// conversation is not held any more (it is over with this call).
	if p.bridge.Mode().on() && !mobileCall {
		p.bridge.setMode(modeOff)
		p.bridge.announce(false)
	}
	if p.session.Paused() {
		p.session.SetPaused(false)
	}
	p.call, p.invite = newCall(), nil
}

// hangUp: The assistant ends the call (the session is over, wrong PIN, nobody answers, ...).
func (p *Phone) hangUp(why string) {
	c := &p.call
	log.Printf("[Phone] hanging up: %s", why)
	if why != "conversation over" { // the normal end is no error
		dir, guest := "call from", ""
		if c.outgoing {
			dir = "call to"
		}
		if c.stranger {
			guest = " (on behalf / guest)"
		}
		log.Printf("[Error] Phone: hanging up (%s), %s %s%s, after %d s", why, dir, c.number, guest,
			int(time.Since(c.since).Seconds()))
	}
	switch {
	case c.answered:
		p.sendBye(false)
	case c.outgoing:
		p.sendForInvite("CANCEL", c.remote)
	default:
		p.reply(480, "Temporarily Unavailable", "")
	}
	if p.session.Open() {
		p.session.Close()
	}
	if c.outgoing && !c.answered {
		// The 487 to the INVITE still comes and needs its ACK: keep the call data a moment.
		c.stage = stageNone
		if c.line != nil {
			c.line.Stop()
		}
		return
	}
	p.clearCall()
}

func (p *Phone) onRequest(req *Message, from *net.UDPAddr) {
	c := &p.call
	ours := c.callID != "" && req.Header("call-id") == c.callID
	ok := func(to string) {
		p.send(buildResponse(req, 200, "OK", to, p.contact(), ""), from)
	}
	switch req.Method {
	case "INVITE":
		p.onInvite(req, from)
	case "ACK":
		if ours {
			c.acked = true
		}
	case "BYE":
		ok(req.Header("to"))
		if !ours {
			return
		}
		log.Print("[Phone] the other side hung up")
		if p.session.Open() {
			p.session.Close()
		}
		p.clearCall()
	case "CANCEL":
		ok(req.Header("to"))
		if !ours || c.answered || c.outgoing {
			return
		}
		p.reply(487, "Request Terminated", "")
		if c.stage == stageRinging { // the primary (or another Echo) took it; it reports the call
			log.Print("[Phone] the call was taken elsewhere or the caller hung up")
			p.clearCall()
			return
		}
		log.Print("[Phone] the caller hung up before we answered")
		if c.stage == stageWaiting { // the conversation at home goes on
			if c.announced {
				p.session.Commentary(strings.ReplaceAll(prompts.Text("LIVE_CALL_GAVE_UP"), "{name}", c.callerText()))
			}
			p.logMissed(*c, "rief an, während "+prompts.Name()+" zu Hause im Gespräch war, und legte wieder auf.")
		} else if p.session.Open() {
			p.session.Close()
		}
		p.clearCall()
	case "OPTIONS", "NOTIFY", "INFO":
		ok(withTag(req.Header("to")))
	default:
		p.send(buildResponse(req, 501, "Not Implemented", withTag(req.Header("to")), p.contact(), ""), from)
	}
}

func (p *Phone) logMissed(c call, what string) {
	if p.deps.LogEvent == nil {
		return
	}
	channel := memory.PhoneIn
	if c.stranger {
		channel = memory.Guest
	}
	person, text := c.person, c.callerText()+" "+what
	go p.deps.LogEvent(person, channel, "Verpasster Anruf", text)
}

func (p *Phone) onResponse(m *Message) {
	c := &p.call
	method := m.CSeqMethod()
	callID := m.Header("call-id")
	switch {
	case method == "REGISTER" && callID == p.reg.callID:
		p.onRegisterResponse(m)
	case callID != c.callID || c.callID == "":
	case method == "INVITE" && c.outgoing:
		// Responses to an earlier INVITE (the FritzBox repeats its 401): the INVITE with the
		// password is already out, ignore them.
		if m.CSeqNumber() != c.inviteCseq {
			return
		}
		if c.stage != stageNone {
			p.onInviteResponse(m)
			return
		}
		// After our CANCEL
		if m.Status >= 300 {
			p.sendForInvite("ACK", m.Header("to"))
			p.clearCall()
		} else if m.Status == 200 {
			// Picked up just as we cancelled: the line would stay open and silent.
			c.remote = m.Header("to")
			if target := uriOf(m.Header("contact")); target != "" {
				c.target = target
			}
			p.send(p.inDialog("ACK", c.inviteCseq)+"Content-Length: 0\r\n\r\n", c.signal)
			p.sendBye(false)
			log.Print("[Phone] picked up just after cancelling: hung up")
			p.clearCall()
		}
	case method == "BYE" && (m.Status == 401 || m.Status == 407):
		c.challenge = parseChallenge(m)
		p.sendBye(true)
	}
}

// --- The loop's work -------------------------------------------------------------------------

func (p *Phone) dialIfRequested(now time.Time) {
	r := p.wanted
	if r == nil {
		return
	}
	// A call the user wants to have themselves is wanted now, not in half an hour: it waits only
	// briefly, and they hear why it did not happen.
	if r.direct && now.Sub(r.at) > bridgeWait {
		p.wanted = nil
		log.Print("[Phone] call for the user dropped: another call is running")
		p.say("Ich konnte " + r.nameOrNumber() + " nicht anrufen, es läuft noch ein anderes Telefonat.")
		return
	}
	if now.Sub(r.at) > callWait { // a conversation all this time: too late now
		p.wanted = nil
		log.Print("[Phone] call dropped: busy for too long")
		return
	}
	if p.call.callID != "" || p.session.Open() || !p.reg.registered {
		return // later
	}
	p.wanted = nil
	p.dial(r)
}

func (p *Phone) updateCall(now time.Time) {
	c := &p.call
	// UDP loses packets: our 200 OK again until the ACK comes (RFC 3261 timer G).
	if !c.outgoing && c.answered && !c.acked && now.Sub(c.okSentAt) >= c.okInterval && c.okInterval <= 4*time.Second {
		p.send(c.ok, c.signal)
		c.okSentAt = now
		c.okInterval *= 2
	}
	switch c.stage {
	case stageNone:
		if c.callID != "" && now.Sub(c.since) > ringTimeout+10*time.Second {
			p.clearCall() // no 487 came
		}
	case stageRinging:
		p.ringAlongLocked(now)
	case stageWaiting:
		switch {
		case !p.session.Open(): // the conversation at home is over: answer now
			log.Print("[Phone] conversation at home over: answering the waiting call")
			p.pickUp()
		case !c.announced && p.session.State() == live.Live:
			c.announced = true
			p.session.Commentary(strings.ReplaceAll(prompts.Text("LIVE_CALL_WAITING"), "{name}", c.callerText()))
		case now.Sub(c.since) > waitingFor:
			p.reply(486, "Busy Here", "")
			log.Print("[Phone] waiting call: busy (the conversation at home goes on)")
			p.logMissed(*c, "rief an, während "+prompts.Name()+" zu Hause im Gespräch war; nach einer Minute bekam der Anruf besetzt.")
			p.clearCall()
		}
	case stagePin:
		for {
			digit, ok := c.line.TakeDigit()
			if !ok {
				break
			}
			if digit != '#' {
				c.pin += string(digit)
				continue
			}
			if c.pin != p.pin {
				log.Printf("[Phone] wrong PIN from %s", c.number)
				p.hangUp("wrong PIN")
				return
			}
			log.Print("[Phone] PIN correct")
			c.stage = stageConnecting
			p.session.Start(p.sessionRequest(""))
			return
		}
		if now.Sub(c.since) > pinTimeout {
			p.hangUp("no PIN")
		}
	case stageConnecting:
		limit := connectTimeout
		if p.pin != "" {
			limit += pinTimeout
		}
		switch {
		case p.session.State() == live.Live:
			if !c.answered {
				p.answer()
				if p.call.stage == stageNone {
					return
				}
			}
			c.stage = stageTalking
		case !p.session.Open():
			p.hangUp("the session did not start")
		case now.Sub(c.since) > limit:
			p.hangUp("the session takes too long")
		}
	case stageDialing:
		// dial_for_me: no session is waiting for the pick-up, only the ringing has a limit.
		if c.direct {
			if now.Sub(c.since) > ringTimeout {
				p.say(c.calledText() + " nimmt nicht ab.")
				p.hangUp("nobody answers")
				return
			}
			if c.probe == nil {
				p.ringback(now)
			}
			return
		}
		switch {
		case !p.session.Open():
			if c.stranger {
				p.callFailedLocked(outcomeFailed, "Der Anruf bei <code>"+c.number+"</code> ließ sich nicht aufbauen.")
			} else {
				p.tell("Ich wollte dich anrufen, aber die Sitzung ließ sich nicht aufbauen.")
			}
			p.hangUp("the session did not start")
		case now.Sub(c.since) > ringTimeout:
			if c.stranger {
				p.callFailedLocked(outcomeNoAnswer, "Bei <code>"+c.number+"</code> hat niemand abgenommen.")
			} else {
				p.tell("Ich habe dich angerufen, aber du hast nicht abgenommen.")
			}
			p.hangUp("nobody answers")
		}
	case stageBridge:
		// The call is nothing but the room at home now (the user dialled in): it does not end
		// with the conversation - that is the point - but with a BYE, when it is switched off at
		// home, or at the latest after PHONE_BRIDGE_MAX_MS, so a forgotten call does not stay
		// open with an open microphone.
		switch {
		case !p.bridge.Mode().on() && c.probe == nil: // the echo test's room is the phone's call
			p.hangUp("the bridge is off")
		case now.Sub(c.bridgeSince) > bridgeMaxCall:
			p.hangUp("the bridge ran for too long")
		}
	case stageTalking:
		// Closing: hang up now, not after the server confirmed the end (up to 5 s of silence).
		if !p.session.Open() || p.session.State() == live.Closing {
			p.hangUp("conversation over")
		} else if c.greet && p.session.State() == live.Live {
			c.greet = false
			if c.stranger {
				p.session.Commentary(prompts.Text("LIVE_CALL_ANSWERED"))
			} else {
				p.session.Commentary(prompts.Text("LIVE_PHONE_ANSWERED"))
			}
			// Listened to at home from the start: the other side is told at once (§ 201 StGB).
			if c.listen && p.bridge.Mode().ToRoom {
				p.session.Commentary(prompts.TextOr("LIVE_BRIDGE_LISTENING", bridgeListening))
			}
		}
	}
}
