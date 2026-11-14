package main

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"log"
	"strings"
	"sync"
	"time"

	"echo-assistant/internal/peer"
	"echo-assistant/internal/tools"
)

// A call at home, heard and joined on the phone (issue #20): the phone's app is the call's room
// as another Echo is (roomlink.go), over the audio stream through the home tunnel
// (internal/peer/stream.go, as the intercom from the phone). The Echo the gateway hands the stream
// to takes it itself when the call runs there, else it passes the audio on to the Echo with the
// call over the usual UDP link (roomRelay). For "ruf … an, ich will mithören" from the phone the
// call does not run yet: the stream waits (wait) until it is picked up, for up to roomWaitMax.
//
// On the phone: speaker_call (listen, talk, off) and phone_call listen/talk; a change of mode is a
// new stream (the Echo with the call takes the new room and drops the old one, roomNow).

type roomStreamReq struct {
	Session uint32 `json:"session"`
	Key     string `json:"key"`
	From    string `json:"from"`
	Mode    string `json:"mode"`
	Wait    bool   `json:"wait,omitempty"` // no call yet: wait for the pick-up
}

// roomWaitMax: how long a stream waits for a call to be picked up (dialling, ringing).
const roomWaitMax = 3 * time.Minute

// callHome: a call at this Echo or another one that a room can join; at is nil for this one.
func (d *device) callHome() (here bool, at *peer.Peer) {
	if d.svc.phone != nil && d.svc.phone.CallForRoom() {
		return true, nil
	}
	if d.peers != nil {
		at = d.callElsewhere()
	}
	return false, at
}

// serveRoomStream: the phone's app wants the room of the call at home.
func (d *device) serveRoomStream(body []byte, link *peer.AudioLink) ([]byte, bool) {
	reply := func(text string, keep bool) ([]byte, bool) {
		out, _ := json.Marshal(sharedText{Text: text})
		return out, keep
	}
	var q roomStreamReq
	if json.Unmarshal(body, &q) != nil || q.From == "" || q.Mode != "listen" && q.Mode != "talk" {
		return reply("Error: bad request; mode must be listen or talk.", false)
	}
	here, at := d.callHome()
	switch {
	case here || at != nil:
		answer := d.attachRoomStream(link, q, here, at)
		return reply(answer, !strings.HasPrefix(answer, "Error"))
	case !q.Wait:
		return reply("Error: no call is running at home.", false)
	}
	go d.waitForCall(link, q)
	return reply("Waiting: the call comes onto the phone ("+q.Mode+") once it is picked up.", true)
}

func (d *device) attachRoomStream(link *peer.AudioLink, q roomStreamReq, here bool, at *peer.Peer) string {
	if here {
		return d.takeRoom(link, q.From, q.Mode, false)
	}
	return d.relayRoom(link, q.From, *at, q.Mode)
}

// waitForCall: the stream of a call that is not picked up yet; it joins once it is.
func (d *device) waitForCall(link *peer.AudioLink, q roomStreamReq) {
	gone := make(chan struct{})
	var once sync.Once
	link.Handle(peer.AudioHandlers{OnEnd: func() { once.Do(func() { close(gone) }) }})
	deadline := time.Now().Add(roomWaitMax)
	t := time.NewTicker(300 * time.Millisecond)
	defer t.Stop()
	for {
		select {
		case <-gone:
			return
		case <-t.C:
		}
		if link.Closed() {
			return
		}
		if here, at := d.callHome(); here || at != nil {
			answer := d.attachRoomStream(link, q, here, at)
			log.Printf("[Room] the waiting phone: %s", answer)
			if strings.HasPrefix(answer, "Error") {
				link.Send(peer.AudioEnd, nil)
				link.Close()
			}
			return
		}
		if time.Now().After(deadline) {
			log.Printf("[Room] %s waited for a call in vain", q.From)
			link.Send(peer.AudioEnd, nil)
			link.Close()
			return
		}
	}
}

// relayRoom: the phone's stream as the room of the call at `at`, passed on over UDP.
func (d *device) relayRoom(stream *peer.AudioLink, from string, at peer.Peer, mode string) string {
	session, key := newSession()
	udp, err := d.peers.NewAudioLink(session, key, at.Addr, at.Port+1)
	if err != nil {
		return "Error: " + err.Error()
	}
	var once sync.Once
	end := func(fromPhone bool, why string) {
		once.Do(func() {
			if fromPhone { // the phone left: the room goes back at the Echo with the call
				go d.roomRequest(at, "/room/mode", roomModeReq{Session: session, Mode: "off"})
			}
			stream.Send(peer.AudioEnd, nil)
			stream.Close()
			udp.Close()
			log.Printf("[Room] passing %s on to the call at %s is over (%s)", from, at.Name, why)
		})
	}
	stream.Handle(peer.AudioHandlers{
		OnPCM: udp.SendPCM,
		OnEnd: func() { end(true, from+" left") },
	})
	udp.Handle(peer.AudioHandlers{
		OnPCM:   stream.SendPCM,
		OnTone:  stream.SendTone,
		OnFlush: func() { stream.Send(peer.AudioFlush, nil) },
		OnEnd:   func() { end(false, "the call's room went back") },
	})
	answer := d.roomRequest(at, "/room/join", roomJoin{Session: session, Key: hex.EncodeToString(key), From: d.peers.Name,
		Mode: mode})
	if strings.HasPrefix(answer, "Error") {
		udp.Close()
		return answer
	}
	log.Printf("[Room] passing %s on to the call at %s (%s)", from, at.Name, mode)
	return answer
}

// --- On the phone --------------------------------------------------------------------------------

// joinHome: the call at home onto the phone in mode (listen, talk); wait: it may not be picked up
// yet. A room already here is replaced (the Echo with the call drops the old one).
func (d *device) joinHome(mode string, wait bool) string {
	home := d.peers.Leader()
	if home == nil {
		return "Error: the speakers at home are not reachable right now."
	}
	session, key := newSession()
	body, _ := json.Marshal(roomStreamReq{Session: session, Key: hex.EncodeToString(key), From: d.peers.Name, Mode: mode,
		Wait: wait})
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	link, out, err := d.peers.DialAudio(ctx, *home, "/room/stream", body, session, key)
	if err != nil {
		return "Error: the speakers at home are not reachable: " + err.Error()
	}
	var a sharedText
	json.Unmarshal(out, &a)
	if a.Text == "" || strings.HasPrefix(a.Text, "Error") {
		link.Close()
		if a.Text == "" {
			return "Error: no answer from home."
		}
		return a.Text
	}
	r := &roomLink{link: link, at: *home, mode: mode}
	link.Handle(peer.AudioHandlers{
		OnPCM: func(pcm []int16) {
			if d.speaker.Queued() < 300*time.Millisecond { // late audio is worth nothing
				d.speaker.Queue(pcm)
			}
		},
		OnTone:  func(freq float64, ms int, amp float64) { d.speaker.Tone(freq, ms, amp) },
		OnFlush: d.speaker.Flush,
		OnEnd: func() {
			roomMu.Lock()
			mine := hereRoom == r
			if mine {
				hereRoom = nil
			}
			roomMu.Unlock()
			link.Close()
			if mine {
				log.Print("[Room] the call at home is over for the phone")
			}
		},
	})
	roomMu.Lock()
	old := hereRoom
	hereRoom = r
	roomMu.Unlock()
	if old != nil {
		old.link.Close() // the Echo with the call has the new room already
	}
	log.Printf("[Room] the call at home on the phone (%s): %s", mode, a.Text)
	return a.Text
}

// talkingHome: the phone talks along in a call at home (its microphone goes onto the line).
func talkingHome() bool {
	r := roomHere()
	return r != nil && r.link.Streamed() && r.mode == "talk"
}

// joinHomeCall: speaker_call on the phone.
func (d *device) joinHomeCall(mode string) string {
	if mode == "off" {
		r := roomHere()
		if r == nil {
			return "No call from home is on the phone."
		}
		r.link.Send(peer.AudioEnd, nil)
		d.leaveRoom()
		return "The call is off the phone; it goes on at home without you."
	}
	if r := roomHere(); r != nil && r.mode == mode {
		return "The call is already " + mode + " on the phone."
	}
	return d.joinHome(mode, false)
}

// addPhoneRoomTools: speaker_call on the phone (the call at home onto it).
func (d *device) addPhoneRoomTools() {
	d.registry.Add(tools.Tool{
		Name: "speaker_call",
		Description: "Puts the phone call that runs at home (the assistant's own call, outgoing or incoming, or the " +
			"user's on the landline) onto this phone, so the user follows it or takes part in it ('ich möchte " +
			"mithören', 'ich möchte mitsprechen', 'Verbindung beenden'). listen: the whole call is audible here, the " +
			"microphone stays off - say at once that someone is listening in. talk: this microphone is on the line " +
			"too, a three-way call; the assistant in the call stays in it. off: the call goes on at home without the " +
			"phone (a call that is only the user's own ends with it). A tap on the app's button walks listen, talk, " +
			"off. Only while a call runs at home.",
		Parameters: `{"type": "object", "additionalProperties": false,
            "properties": {"mode": {"type": "string", "enum": ["listen", "talk", "off"], "description": "listen = mithören, talk = mitsprechen, off = beenden"}},
            "required": ["mode"]}`,
		Channels: tools.Voice | tools.Console,
		Run: func(args map[string]any, ctx tools.Context) string {
			answer := d.joinHomeCall(tools.String(args, "mode"))
			// The phone's speaker belongs to the call now: end the conversation that asked for it.
			if ctx.Conversation != nil && !strings.HasPrefix(answer, "Error") && tools.String(args, "mode") != "off" {
				ctx.Conversation.EndSoon()
				answer += " Say one short sentence that it is on the phone, nothing more: this conversation ends now."
			}
			return answer
		},
	})
}
