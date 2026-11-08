package main

import (
	"context"
	"crypto/rand"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"log"
	"strings"
	"sync"
	"time"

	"echo-assistant/internal/peer"
	"echo-assistant/internal/phone"
	"echo-assistant/internal/prompts"
	"echo-assistant/internal/tools"
)

// The intercom between two of the assistant's speakers ("Verbinde mich mit der Küche"): both
// microphones open, each room hears the other, without GPT Live. Two tones up on both sides at
// the start, one down at the end. It ends by voice (intercom stop), with the action button on
// either side (the phone app: a tap), when one side is gone, or after INTERCOM_MAX_MS (10
// minutes), so no open microphone is forgotten. While a conversation with the assistant runs on
// one side, that side sends silence and plays nothing (one voice at a time on its speaker); a
// conversation that asked for the intercom ends first.
// Audio: the peer audio link (internal/peer/audio.go), 16 kHz from the microphone with the
// speakerphone's gain control and gate (internal/phone/speakerphone.go), 24 kHz into the speaker.
//
// The phone away from home sees no other speaker (no heartbeats outside the LAN) and has no UDP
// way home: its intercom is a stream over the home tunnel (internal/peer/stream.go) to the Echo
// the home gateway hands it to. That Echo takes it itself when the room asked for is its own,
// else it passes the audio on to the Echo of that room over the usual UDP link (icRelay), and
// for that one the intercom is with the relaying Echo, under the phone's name.

type intercom struct {
	link   *peer.AudioLink
	with   peer.Peer // where the audio goes (the relaying Echo for the phone)
	who    string    // who is at the other end, for the log and the answers
	since  time.Time
	sp     *phone.Speakerphone
	up     *phone.MicResampler
	lastIn time.Time
	in     int // frames received, for the log
	out    int // frames sent
}

type intercomOpen struct {
	Session uint32 `json:"session"`
	Key     string `json:"key"`
	From    string `json:"from"`
	Room    string `json:"room,omitempty"` // the stream from the phone: the room asked for
	Who     string `json:"who,omitempty"`  // passed on: who is at the other end (the phone)
}

// intercomAnswer: the answer to the phone's stream; With is the speaker it is connected with.
type intercomAnswer struct {
	Text string `json:"text"`
	With string `json:"with,omitempty"`
}

var (
	icMu      sync.Mutex
	icNow     *intercom
	icRelays  = map[uint32]*icRelay{} // on the Echo that passes the phone's stream on: by the UDP session
	silence20 = make([]int16, 320)    // 20 ms at 16 kHz
)

var intercomMax = time.Duration(prompts.Int("INTERCOM_MAX_MS", 10*60*1000)) * time.Millisecond

func (d *device) setupIntercom() {
	if d.peers == nil {
		return
	}
	d.peers.Handle("/intercom/open", d.serveIntercomOpen)
	d.peers.Handle("/intercom/close", d.serveIntercomClose)
	d.peers.HandleAudio("/intercom/stream", d.serveIntercomStream)
	d.registry.Add(tools.Tool{
		Name: "intercom",
		Description: "The intercom to another of the assistant's speakers ('Verbinde mich mit der Küche', 'Gegensprechen mit " + prompts.Name() + " 2'): " +
			"both rooms hear each other until someone ends it ('Verbindung beenden', or the action button). Not for " +
			"announcements (speak with room) and not in a phone call (there connect_speaker with room). action start " +
			"with room (room or name of the other speaker), stop to end it. The conversation ends right after a start: " +
			"the speaker belongs to the intercom.",
		Parameters: `{"type": "object", "additionalProperties": false,
            "properties": {"action": {"type": "string", "enum": ["start", "stop"]},
                           "room": {"type": "string", "description": "start: room or name of the other assistant speaker"}},
            "required": ["action"]}`,
		Channels: tools.Voice | tools.Agent | tools.Console,
		Run: func(args map[string]any, ctx tools.Context) string {
			if tools.String(args, "action") == "stop" {
				return d.endIntercom(true, "ended by voice")
			}
			if ctx.Conversation != nil && ctx.Conversation.Label() == "phone" {
				return "Error: this is a phone call: connect_speaker with mode talk and room connects the caller with that room."
			}
			answer := d.startIntercom(tools.String(args, "room"))
			if !strings.HasPrefix(answer, "Error") && ctx.Conversation != nil && ctx.Conversation.Label() == "speaker" {
				ctx.Conversation.EndSoon()
			}
			return answer
		},
	})
}

func intercomActive() *intercom {
	icMu.Lock()
	defer icMu.Unlock()
	return icNow
}

func newSession() (uint32, []byte) {
	key := make([]byte, 16)
	rand.Read(key)
	var sb [4]byte
	rand.Read(sb[:])
	return binary.BigEndian.Uint32(sb[:]), key
}

// startIntercom: this speaker calls the one in room.
func (d *device) startIntercom(room string) string {
	if intercomActive() != nil {
		return "Error: an intercom is already running; stop it first."
	}
	if room == "" {
		return "Error: name the room or the speaker to connect with."
	}
	here, others, errText := d.speakersIn(room)
	if !here && len(others) != 1 && homeRemote != "" {
		return d.startIntercomHome(room) // the phone away from home
	}
	switch {
	case errText != "":
		return errText
	case here || len(others) != 1:
		return "Error: name exactly one other assistant speaker."
	case others[0].State != "idle" && others[0].State != "music":
		return "Error: " + others[0].Name + " is busy right now (" + others[0].State + ")."
	}
	with := others[0]
	session, key := newSession()
	link, err := d.peers.NewAudioLink(session, key, with.Addr, with.Port+1)
	if err != nil {
		return "Error: " + err.Error()
	}
	d.runIntercom(link, with, with.Name)
	answer := d.roomRequest(with, "/intercom/open", intercomOpen{Session: session, Key: hex.EncodeToString(key), From: d.peers.Name})
	if strings.HasPrefix(answer, "Error") {
		d.endIntercom(false, "the other Echo refused")
		return answer
	}
	return "Connected with " + with.Name + ": both rooms hear each other now. Say nothing more yourself."
}

// startIntercomHome: the phone's intercom over the home tunnel; the Echo that takes the stream
// finds the room.
func (d *device) startIntercomHome(room string) string {
	home := d.peers.Leader()
	if home == nil {
		return "Error: the speakers at home are not reachable right now."
	}
	session, key := newSession()
	body, _ := json.Marshal(intercomOpen{Session: session, Key: hex.EncodeToString(key), From: d.peers.Name, Room: room})
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	link, out, err := d.peers.DialAudio(ctx, *home, "/intercom/stream", body, session, key)
	if err != nil {
		return "Error: the speakers at home are not reachable: " + err.Error()
	}
	var a intercomAnswer
	if json.Unmarshal(out, &a) != nil || a.With == "" || strings.HasPrefix(a.Text, "Error") {
		link.Close()
		if strings.HasPrefix(a.Text, "Error") {
			return a.Text
		}
		return "Error: the speaker at home did not connect."
	}
	d.runIntercom(link, peer.Peer{Name: a.With}, a.With)
	return "Connected with " + a.With + " over the way home: both sides hear each other now. Say nothing more yourself."
}

// runIntercom: the intercom over link with, the tones, the watch.
func (d *device) runIntercom(link *peer.AudioLink, with peer.Peer, who string) {
	ic := &intercom{link: link, with: with, who: who, since: time.Now(), sp: phone.NewSpeakerphone(),
		up: &phone.MicResampler{}, lastIn: time.Now()}
	link.Handle(peer.AudioHandlers{
		OnPCM: func(pcm []int16) { d.intercomIn(ic, pcm) },
		OnEnd: func() { d.endIntercom(false, who+" ended it") },
	})
	icMu.Lock()
	icNow = ic
	icMu.Unlock()
	via := ""
	if link.Streamed() {
		via = " (stream)"
	}
	log.Printf("[Intercom] with %s%s", who, via)
	d.speaker.Tone(660, 120, 0.15)
	d.speaker.Tone(880, 160, 0.15)
	go d.watchIntercom(ic)
}

// intercomIn: a frame of the other room (16 kHz) into this speaker.
func (d *device) intercomIn(ic *intercom, pcm []int16) {
	icMu.Lock()
	ic.lastIn = time.Now()
	ic.in++
	icMu.Unlock()
	if d.session.Open() {
		return // The assistant talks here: one voice at a time
	}
	later := d.speaker.Queued()
	if later > 300*time.Millisecond {
		return // late audio is worth nothing
	}
	out := ic.up.Process(pcm)
	ic.sp.Played(out, later)
	d.speaker.Queue(out)
}

// hearIntercom: this microphone to the other room; silence while the assistant talks here or the
// microphone is muted (the frames keep the link alive).
func (d *device) hearIntercom(pcm []int16) {
	ic := intercomActive()
	if ic == nil {
		return
	}
	icMu.Lock()
	ic.out++
	icMu.Unlock()
	if d.session.Open() || d.muted.Load() {
		ic.link.SendPCM(silence20[:min(len(pcm), len(silence20))])
		return
	}
	ic.link.SendPCM(ic.sp.Out(pcm))
}

func (d *device) watchIntercom(ic *intercom) {
	for range time.Tick(time.Second) {
		if intercomActive() != ic {
			return
		}
		icMu.Lock()
		quiet := time.Since(ic.lastIn)
		icMu.Unlock()
		switch {
		case time.Since(ic.since) > intercomMax:
			d.endIntercom(true, "time is up")
			return
		case quiet > 10*time.Second:
			d.endIntercom(false, ic.who+" is silent")
			return
		}
	}
}

// endIntercom: the intercom is over here; tell: the other side too (a stream hears it from
// the end of the connection).
func (d *device) endIntercom(tell bool, why string) string {
	icMu.Lock()
	ic := icNow
	icNow = nil
	icMu.Unlock()
	if ic == nil {
		return "No intercom is running."
	}
	ic.link.Send(peer.AudioEnd, nil)
	ic.link.Close()
	if tell && !ic.link.Streamed() {
		go d.roomRequest(ic.with, "/intercom/close", intercomOpen{Session: ic.link.Session})
	}
	log.Printf("[Intercom] over (%s) after %s, %d frames in, %d out", why, time.Since(ic.since).Round(time.Second),
		ic.in, ic.out)
	if r := ic.sp.Report(); r != "" {
		log.Printf("[Intercom] microphone: %s", r)
	}
	d.speaker.Tone(440, 180, 0.15)
	return "The intercom with " + ic.who + " is over."
}

// --- The speaker that is called --------------------------------------------------------------

// intercomBusy: this speaker cannot take an intercom now.
func (d *device) intercomBusy() bool {
	return intercomActive() != nil || d.session.Open() || roomHere() != nil ||
		d.svc.phone != nil && (d.svc.phone.InCall() || d.svc.phone.BridgeActive())
}

func (d *device) serveIntercomOpen(body []byte) ([]byte, error) {
	var q intercomOpen
	if err := json.Unmarshal(body, &q); err != nil {
		return nil, err
	}
	var from *peer.Peer
	for _, p := range d.peers.Alive() {
		if p.Name == q.From {
			from = &p
		}
	}
	key, err := hex.DecodeString(q.Key)
	switch {
	case d.intercomBusy():
		return json.Marshal(sharedText{Text: "Error: " + d.peers.Name + " is busy right now."})
	case from == nil || err != nil || len(key) < 16:
		return json.Marshal(sharedText{Text: "Error: unknown Echo or key."})
	}
	link, err := d.peers.NewAudioLink(q.Session, key, from.Addr, from.Port+1)
	if err != nil {
		return nil, err
	}
	who := from.Name
	if q.Who != "" {
		who = q.Who
	}
	d.runIntercom(link, *from, who)
	return json.Marshal(sharedText{Text: "ok"})
}

func (d *device) serveIntercomClose(body []byte) ([]byte, error) {
	var q intercomOpen
	json.Unmarshal(body, &q)
	if ic := intercomActive(); ic != nil && ic.link.Session == q.Session {
		d.endIntercom(false, ic.who+" ended it")
	}
	icMu.Lock()
	r := icRelays[q.Session]
	icMu.Unlock()
	if r != nil {
		d.endRelay(r, false, r.to.Name+" ended it")
	}
	return json.Marshal(sharedText{Text: "ok"})
}

// serveIntercomStream: the phone's intercom over the home tunnel, for the room it names.
func (d *device) serveIntercomStream(body []byte, link *peer.AudioLink) ([]byte, bool) {
	reply := func(text, with string) ([]byte, bool) {
		out, _ := json.Marshal(intercomAnswer{Text: text, With: with})
		return out, with != ""
	}
	var q intercomOpen
	if json.Unmarshal(body, &q) != nil || q.From == "" {
		return reply("Error: bad request.", "")
	}
	here, others, errText := d.speakersIn(q.Room)
	switch {
	case errText != "":
		return reply(errText, "")
	case here:
		if d.intercomBusy() {
			return reply("Error: "+d.peers.Name+" is busy right now.", "")
		}
		d.runIntercom(link, peer.Peer{Name: q.From}, q.From)
		return reply("ok", d.peers.Name)
	case len(others) != 1:
		return reply("Error: name exactly one assistant speaker.", "")
	case others[0].State != "idle" && others[0].State != "music":
		return reply("Error: "+others[0].Name+" is busy right now ("+others[0].State+").", "")
	}
	if text := d.relayIntercom(link, q.From, others[0]); text != "" {
		return reply(text, "")
	}
	return reply("ok", others[0].Name)
}

// icRelay: the phone's stream passed on to the Echo of the room it asked for.
type icRelay struct {
	stream, udp *peer.AudioLink
	from        string // the phone
	to          peer.Peer
	since       time.Time
	once        sync.Once
	mu          sync.Mutex
	lastStream  time.Time // last frame from the phone
	lastUDP     time.Time // ... from the Echo
	frames      int
}

func (r *icRelay) seen(fromPhone bool) {
	r.mu.Lock()
	if fromPhone {
		r.lastStream = time.Now()
	} else {
		r.lastUDP = time.Now()
	}
	r.frames++
	r.mu.Unlock()
}

// relayIntercom: the intercom of the phone (stream) with the Echo to; "" when it runs, else the
// error for the phone.
func (d *device) relayIntercom(stream *peer.AudioLink, from string, to peer.Peer) string {
	session, key := newSession()
	udp, err := d.peers.NewAudioLink(session, key, to.Addr, to.Port+1)
	if err != nil {
		return "Error: " + err.Error()
	}
	r := &icRelay{stream: stream, udp: udp, from: from, to: to, since: time.Now(), lastStream: time.Now(),
		lastUDP: time.Now()}
	stream.Handle(peer.AudioHandlers{
		OnPCM: func(pcm []int16) { r.seen(true); udp.SendPCM(pcm) },
		OnEnd: func() { d.endRelay(r, true, from+" ended it") },
	})
	udp.Handle(peer.AudioHandlers{
		OnPCM: func(pcm []int16) { r.seen(false); stream.SendPCM(pcm) },
		OnEnd: func() { d.endRelay(r, false, to.Name+" ended it") },
	})
	icMu.Lock()
	icRelays[session] = r
	icMu.Unlock()
	answer := d.roomRequest(to, "/intercom/open", intercomOpen{Session: session, Key: hex.EncodeToString(key),
		From: d.peers.Name, Who: from})
	if strings.HasPrefix(answer, "Error") {
		icMu.Lock()
		delete(icRelays, session)
		icMu.Unlock()
		udp.Close()
		return answer
	}
	log.Printf("[Intercom] passing %s on to %s", from, to.Name)
	go d.watchRelay(r)
	return ""
}

func (d *device) watchRelay(r *icRelay) {
	for range time.Tick(time.Second) {
		icMu.Lock()
		live := icRelays[r.udp.Session] == r
		icMu.Unlock()
		if !live {
			return
		}
		r.mu.Lock()
		quiet := max(time.Since(r.lastStream), time.Since(r.lastUDP))
		r.mu.Unlock()
		switch {
		case time.Since(r.since) > intercomMax+time.Minute: // the ends watch the time themselves
			d.endRelay(r, true, "time is up")
			return
		case quiet > 12*time.Second:
			d.endRelay(r, true, "one side is silent")
			return
		}
	}
}

// endRelay: both ends are told; tellTo: also over a call (the UDP end frame may be lost).
func (d *device) endRelay(r *icRelay, tellTo bool, why string) {
	r.once.Do(func() {
		icMu.Lock()
		delete(icRelays, r.udp.Session)
		icMu.Unlock()
		r.stream.Send(peer.AudioEnd, nil)
		r.stream.Close()
		r.udp.Send(peer.AudioEnd, nil)
		r.udp.Close()
		if tellTo {
			go d.roomRequest(r.to, "/intercom/close", intercomOpen{Session: r.udp.Session})
		}
		r.mu.Lock()
		frames := r.frames
		r.mu.Unlock()
		log.Printf("[Intercom] %s with %s over (%s) after %s, %d frames passed on", r.from, r.to.Name, why,
			time.Since(r.since).Round(time.Second), frames)
	})
}
