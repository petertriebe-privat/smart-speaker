package main

import (
	"context"
	"crypto/rand"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"log"
	"strings"
	"sync"
	"time"

	"echo-assistant/internal/peer"
	"echo-assistant/internal/phone"
	"echo-assistant/internal/tools"
)

// A call on another Echo, heard and joined here (phase 7 of devices/echo/docs/18-several-echos.md, #63).
// This Echo (where the user is) asks the one with the call for its room: that one's speaker
// direction plays here, this microphone goes onto its line (internal/phone/remote.go), over a
// peer audio link. The button walks off -> listen -> talk -> off as for a call here; a long press
// ends it (a call that is only the bridge hangs up, as at home). By voice: join_call.

// roomLink: the call of another Echo whose room is here (nil = none).
type roomLink struct {
	link *peer.AudioLink
	at   peer.Peer
	mode string
}

type roomJoin struct {
	Session uint32 `json:"session"`
	Key     string `json:"key"`
	From    string `json:"from"`
	Mode    string `json:"mode"`
}

type roomModeReq struct {
	Session uint32 `json:"session"`
	Mode    string `json:"mode"`
}

var (
	roomMu    sync.Mutex
	hereRoom  *roomLink                      // on this Echo: the room of another's call
	roomLinks = map[uint32]*peer.AudioLink{} // on the Echo with the call: the rooms elsewhere
)

func (d *device) setupRoomLink() {
	if d.peers == nil {
		return
	}
	d.peers.Handle("/room/join", d.serveRoomJoin)
	d.peers.Handle("/room/mode", d.serveRoomMode)
	d.registry.Add(tools.Tool{
		Name: "join_call",
		Description: "A phone call that runs at another of the assistant's speakers (the assistant's own call, the user's call on the landline " +
			"or the mobile phone) comes here: listen (hear it on this speaker), talk (take part with this microphone), " +
			"off (end it here; a call that is only the user's own then hangs up). For a call at this speaker: speaker_call.",
		Parameters: `{"type": "object", "additionalProperties": false,
            "properties": {"mode": {"type": "string", "enum": ["listen", "talk", "off"]}}, "required": ["mode"]}`,
		Channels: tools.Voice | tools.Agent | tools.Console,
		Run: func(args map[string]any, ctx tools.Context) string {
			if ctx.Conversation != nil && ctx.Conversation.Label() == "speaker" {
				defer ctx.Conversation.EndSoon() // the speaker belongs to the call now
			}
			return d.joinCall(tools.String(args, "mode"))
		},
	})
}

// callElsewhere: another Echo with a call running.
func (d *device) callElsewhere() *peer.Peer {
	for _, p := range d.peers.Alive() {
		if p.State == "call" {
			return &p
		}
	}
	return nil
}

func roomHere() *roomLink {
	roomMu.Lock()
	defer roomMu.Unlock()
	return hereRoom
}

// joinCall: listen, talk or off for another Echo's call, here.
func (d *device) joinCall(mode string) string {
	if d.peers == nil {
		return "Error: this Echo is not linked with others."
	}
	if r := roomHere(); r != nil {
		answer := d.roomRequest(r.at, "/room/mode", roomModeReq{Session: r.link.Session, Mode: mode})
		if mode == "off" || strings.HasPrefix(answer, "Error") && strings.Contains(answer, "not at another") {
			d.leaveRoom()
		} else if !strings.HasPrefix(answer, "Error") {
			roomMu.Lock()
			r.mode = mode
			roomMu.Unlock()
		}
		return answer
	}
	if mode == "off" {
		return "No call of another Echo is here."
	}
	if d.svc.phone != nil && d.svc.phone.CallForRoom() {
		return "Error: the call runs at this speaker; use speaker_call."
	}
	at := d.callElsewhere()
	if at == nil {
		return "Error: no call is running at another assistant speaker."
	}
	key := make([]byte, 16)
	rand.Read(key)
	var sb [4]byte
	rand.Read(sb[:])
	session := binary.BigEndian.Uint32(sb[:])
	link, err := d.peers.NewAudioLink(session, key, at.Addr, at.Port+1)
	if err != nil {
		return "Error: " + err.Error()
	}
	link.OnPCM = func(pcm []int16) {
		if d.speaker.Queued() < 300*time.Millisecond { // late audio is worth nothing
			d.speaker.Queue(pcm)
		}
	}
	link.OnTone = func(freq float64, ms int, amp float64) { d.speaker.Tone(freq, ms, amp) }
	link.OnFlush = d.speaker.Flush
	link.OnEnd = func() {
		log.Print("[Room] the call at the other Echo is over")
		d.leaveRoom()
	}
	roomMu.Lock()
	hereRoom = &roomLink{link: link, at: *at, mode: mode}
	roomMu.Unlock()
	answer := d.roomRequest(*at, "/room/join", roomJoin{Session: session, Key: hex.EncodeToString(key), From: d.peers.Name, Mode: mode})
	if strings.HasPrefix(answer, "Error") {
		d.leaveRoom()
		return answer
	}
	log.Printf("[Room] the call at %s is here (%s)", at.Name, mode)
	return answer
}

func (d *device) leaveRoom() {
	roomMu.Lock()
	r := hereRoom
	hereRoom = nil
	roomMu.Unlock()
	if r != nil {
		r.link.Close()
	}
}

// roomButton: the action button while another Echo's call can come here; "" = not ours.
func (d *device) roomButton(held time.Duration) string {
	r := roomHere()
	switch {
	case r != nil && held >= buttonLongPress:
		return d.joinCall("off")
	case r != nil && r.mode == "listen":
		return d.joinCall("talk")
	case r != nil:
		return d.joinCall("off")
	}
	return d.joinCall("listen")
}

const buttonLongPress = time.Second

// roomOwnsButton: the button belongs to another Echo's call (here, or one to fetch).
func (d *device) roomOwnsButton() bool {
	if d.peers == nil || d.session.Open() {
		return false
	}
	if roomHere() != nil {
		return true
	}
	return (d.svc.phone == nil || !d.svc.phone.ButtonOwned()) && d.callElsewhere() != nil
}

// hearRoom: this microphone onto the other Echo's line while its room is here.
func (d *device) hearRoom(pcm []int16) {
	if r := roomHere(); r != nil {
		r.link.SendPCM(pcm)
	}
}

func (d *device) roomRequest(p peer.Peer, path string, q any) string {
	body, _ := json.Marshal(q)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	out, err := d.peers.CallPeer(ctx, p, path, body)
	if err != nil {
		return "Error: " + err.Error()
	}
	var a sharedText
	if err := json.Unmarshal(out, &a); err != nil {
		return "Error: " + err.Error()
	}
	return a.Text
}

// --- The Echo with the call --------------------------------------------------------------------

func (d *device) serveRoomJoin(body []byte) ([]byte, error) {
	var q roomJoin
	if err := json.Unmarshal(body, &q); err != nil {
		return nil, err
	}
	if d.svc.phone == nil {
		return nil, errors.New("no phone here")
	}
	var from *peer.Peer
	for _, p := range d.peers.Alive() {
		if p.Name == q.From {
			from = &p
		}
	}
	key, err := hex.DecodeString(q.Key)
	if from == nil || err != nil || len(key) < 16 {
		return json.Marshal(sharedText{Text: "Error: unknown Echo or key."})
	}
	link, err := d.peers.NewAudioLink(q.Session, key, from.Addr, from.Port+1)
	if err != nil {
		return nil, err
	}
	link.OnPCM = d.svc.phone.HearRemote
	sp := phone.RemoteSpeaker(link.SendPCM, link.SendTone, func() { link.Send(peer.AudioFlush, nil) })
	gone := func() {
		log.Printf("[Room] the room is back from %s", q.From)
		link.Send(peer.AudioEnd, nil)
		link.Close()
		roomMu.Lock()
		delete(roomLinks, q.Session)
		roomMu.Unlock()
	}
	answer := d.svc.phone.MoveRoom(sp, q.Mode, gone)
	if strings.HasPrefix(answer, "Error") {
		link.Close()
	} else {
		roomMu.Lock()
		roomLinks[q.Session] = link
		roomMu.Unlock()
		log.Printf("[Room] the call's room is at %s (%s)", q.From, q.Mode)
	}
	return json.Marshal(sharedText{Text: answer})
}

func (d *device) serveRoomMode(body []byte) ([]byte, error) {
	var q roomModeReq
	if err := json.Unmarshal(body, &q); err != nil {
		return nil, err
	}
	roomMu.Lock()
	_, ok := roomLinks[q.Session]
	roomMu.Unlock()
	if !ok || d.svc.phone == nil {
		return json.Marshal(sharedText{Text: "Error: the room of the call is not at another Echo."})
	}
	return json.Marshal(sharedText{Text: d.svc.phone.RoomMode(q.Mode)})
}
