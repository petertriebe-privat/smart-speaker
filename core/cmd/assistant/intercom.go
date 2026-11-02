package main

import (
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

// The intercom between two Echos ("Verbinde mich mit der Küche"): both microphones open, each
// room hears the other, without GPT Live. Two tones up on both sides at the start, one down at
// the end. It ends by voice (intercom stop), with the action button on either Echo, when one
// side is gone, or after INTERCOM_MAX_MS (10 minutes), so no open microphone is forgotten. While
// a conversation with the assistant runs on one side, that side sends silence and plays nothing (one
// voice at a time on its speaker); a conversation that asked for the intercom ends first.
// Audio: the peer audio link (internal/peer/audio.go), 16 kHz from the microphone with the
// speakerphone's gain control and gate (internal/phone/speakerphone.go), 24 kHz into the speaker.

type intercom struct {
	link   *peer.AudioLink
	with   peer.Peer
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
}

var (
	icMu      sync.Mutex
	icNow     *intercom
	silence20 = make([]int16, 320) // 20 ms at 16 kHz
)

var intercomMax = time.Duration(prompts.Int("INTERCOM_MAX_MS", 10*60*1000)) * time.Millisecond

func (d *device) setupIntercom() {
	if d.peers == nil {
		return
	}
	d.peers.Handle("/intercom/open", d.serveIntercomOpen)
	d.peers.Handle("/intercom/close", d.serveIntercomClose)
	d.registry.Add(tools.Tool{
		Name: "intercom",
		Description: "The intercom to another of the assistant' speakers ('Verbinde mich mit der Küche', 'Gegensprechen mit " + prompts.Name() + " 2'): " +
			"both rooms hear each other until someone ends it ('Verbindung beenden', or the action button). Not for " +
			"announcements (speak with room) or phone calls. action start with room (room or name of the other speaker), " +
			"stop to end it. The conversation ends right after a start: the speaker belongs to the intercom.",
		Parameters: `{"type": "object", "additionalProperties": false,
            "properties": {"action": {"type": "string", "enum": ["start", "stop"]},
                           "room": {"type": "string", "description": "start: room or name of the other assistant speaker"}},
            "required": ["action"]}`,
		Channels: tools.Voice | tools.Agent | tools.Console,
		Run: func(args map[string]any, ctx tools.Context) string {
			if tools.String(args, "action") == "stop" {
				return d.endIntercom(true, "ended by voice")
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

// startIntercom: this Echo calls the one in room.
func (d *device) startIntercom(room string) string {
	if intercomActive() != nil {
		return "Error: an intercom is already running; stop it first."
	}
	if room == "" {
		return "Error: name the room or the speaker to connect with."
	}
	here, others, errText := d.speakersIn(room)
	switch {
	case errText != "":
		return errText
	case here || len(others) != 1:
		return "Error: name exactly one other assistant speaker."
	case others[0].State != "idle" && others[0].State != "music":
		return "Error: " + others[0].Name + " is busy right now (" + others[0].State + ")."
	}
	with := others[0]
	key := make([]byte, 16)
	rand.Read(key)
	var sb [4]byte
	rand.Read(sb[:])
	session := binary.BigEndian.Uint32(sb[:])
	if err := d.openIntercom(session, key, with); err != nil {
		return "Error: " + err.Error()
	}
	answer := d.roomRequest(with, "/intercom/open", intercomOpen{Session: session, Key: hex.EncodeToString(key), From: d.peers.Name})
	if strings.HasPrefix(answer, "Error") {
		d.endIntercom(false, "the other Echo refused")
		return answer
	}
	return "Connected with " + with.Name + ": both rooms hear each other now. Say nothing more yourself."
}

// openIntercom: the link to with, the tones, the watch.
func (d *device) openIntercom(session uint32, key []byte, with peer.Peer) error {
	link, err := d.peers.NewAudioLink(session, key, with.Addr, with.Port+1)
	if err != nil {
		return err
	}
	ic := &intercom{link: link, with: with, since: time.Now(), sp: phone.NewSpeakerphone(), up: &phone.MicResampler{},
		lastIn: time.Now()}
	link.OnPCM = func(pcm []int16) { d.intercomIn(ic, pcm) }
	link.OnEnd = func() { d.endIntercom(false, with.Name+" ended it") }
	icMu.Lock()
	icNow = ic
	icMu.Unlock()
	log.Printf("[Intercom] with %s", with.Name)
	d.speaker.Tone(660, 120, 0.15)
	d.speaker.Tone(880, 160, 0.15)
	go d.watchIntercom(ic)
	return nil
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
			d.endIntercom(false, ic.with.Name+" is silent")
			return
		}
	}
}

// endIntercom: the intercom is over here; tell: the other Echo too.
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
	if tell {
		go d.roomRequest(ic.with, "/intercom/close", intercomOpen{Session: ic.link.Session})
	}
	log.Printf("[Intercom] over (%s) after %s, %d frames in, %d out", why, time.Since(ic.since).Round(time.Second),
		ic.in, ic.out)
	d.speaker.Tone(440, 180, 0.15)
	return "The intercom with " + ic.with.Name + " is over."
}

// --- The Echo that is called ------------------------------------------------------------------

func (d *device) serveIntercomOpen(body []byte) ([]byte, error) {
	var q intercomOpen
	if err := json.Unmarshal(body, &q); err != nil {
		return nil, err
	}
	busy := intercomActive() != nil || d.session.Open() ||
		d.svc.phone != nil && (d.svc.phone.InCall() || d.svc.phone.BridgeActive())
	var from *peer.Peer
	for _, p := range d.peers.Alive() {
		if p.Name == q.From {
			from = &p
		}
	}
	key, err := hex.DecodeString(q.Key)
	switch {
	case busy:
		return json.Marshal(sharedText{Text: "Error: " + d.peers.Name + " is busy right now."})
	case from == nil || err != nil || len(key) < 16:
		return json.Marshal(sharedText{Text: "Error: unknown Echo or key."})
	}
	if err := d.openIntercom(q.Session, key, *from); err != nil {
		return nil, err
	}
	return json.Marshal(sharedText{Text: "ok"})
}

func (d *device) serveIntercomClose(body []byte) ([]byte, error) {
	var q intercomOpen
	json.Unmarshal(body, &q)
	if ic := intercomActive(); ic != nil && ic.link.Session == q.Session {
		d.endIntercom(false, ic.with.Name+" ended it")
	}
	return json.Marshal(sharedText{Text: "ok"})
}
