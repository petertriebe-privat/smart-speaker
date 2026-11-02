package main

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"time"

	"echo-assistant/internal/assistant"
	"echo-assistant/internal/live"
	"echo-assistant/internal/peer"
)

// Rooms (phase 6 of devices/echo/docs/18-several-echos.md, #58, #65): each Echo knows its room
// (device.json: room, ha_area); the instructions name it, so "mach das Licht an" means this room,
// and list the other Echos. speak and start_conversation take a room and go to the Echo there
// over the peer link ("all": every Echo).

type roomSpeak struct {
	Text string `json:"text"`
}

type roomConversation struct {
	Reason   string `json:"reason"`
	Person   string `json:"person"`
	Insist   bool   `json:"insist"`
	RepeatMS int64  `json:"repeat_ms"`
}

func (d *device) setupRooms() {
	s := d.cfg.Settings
	assistant.Room, assistant.Area = s.Room, s.HAArea
	assistant.Speakers = d.speakersText
	d.svc.announcer.Route = d.speakIn
	if d.peers != nil {
		d.peers.Handle("/speak", d.serveSpeak)
		d.peers.Handle("/start_conversation", d.serveConversationStart)
	}
}

// speakersText: this Echo and the others for the instructions, with room and role. Without the
// role Jarvis made one up: "none of your Jarvis is the primary device" (19.10.2026). A device
// with a presence sensor (the M5) also says whether someone is in its room, so an announcement
// can go where someone is.
func (d *device) speakersText() string {
	describe := func(name, room string, primary bool, occupied *bool) string {
		var extra []string
		if room != "" {
			extra = append(extra, room)
		}
		if primary {
			extra = append(extra, "primary device: phone, chats, mail, memory")
		}
		if occupied != nil {
			extra = append(extra, map[bool]string{true: "someone in the room now", false: "nobody in the room now"}[*occupied])
		}
		if len(extra) == 0 {
			return name
		}
		return name + " (" + strings.Join(extra, ", ") + ")"
	}
	parts := []string{"this one is " + describe(d.peers.Name, d.cfg.Settings.Room, d.cfg.Settings.Primary, nil)}
	for _, p := range d.peers.Alive() {
		parts = append(parts, describe(p.Name, p.Room, p.Primary, p.Occupied))
	}
	if len(parts) == 1 {
		return ""
	}
	return strings.Join(parts, "; ")
}

// roomMatches: room names name, exactly or (loose) as a part ("Küche" in "in der Küche"); numbers
// must agree either way: "Jarvis" is not "Jarvis 2".
func roomMatches(room, name string, loose bool) bool {
	room, name = strings.ToLower(strings.TrimSpace(room)), strings.ToLower(strings.TrimSpace(name))
	if name == "" {
		return false
	}
	if room == name {
		return true
	}
	return loose && (strings.Contains(room, name) || strings.Contains(name, room)) &&
		slices.Equal(numbersOf(room), numbersOf(name))
}

func numbersOf(text string) []string {
	var out []string
	for _, w := range strings.Fields(text) {
		if strings.Trim(w, "0123456789") == "" {
			out = append(out, w)
		}
	}
	return out
}

// speakersIn: the Echos room means: this one (here), others; an error text when none.
func (d *device) speakersIn(room string) (here bool, others []peer.Peer, errText string) {
	all := d.peers.Alive()
	switch strings.ToLower(strings.TrimSpace(room)) {
	case "", "hier", "here":
		return true, nil, ""
	case "all", "alle", "überall", "everywhere":
		return true, all, ""
	}
	s := d.cfg.Settings
	for _, loose := range []bool{false, true} { // an exact name first
		if roomMatches(room, s.Room, loose) || roomMatches(room, d.peers.Name, loose) {
			return true, nil, ""
		}
		for _, p := range all {
			if roomMatches(room, p.Room, loose) || roomMatches(room, p.Name, loose) {
				others = append(others, p)
			}
		}
		if len(others) > 0 {
			return false, others, ""
		}
	}
	known := d.speakersText()
	if known == "" {
		known = "none"
	}
	return false, nil, "Error: no assistant speaker in " + room + ". Other assistant speakers: " + known +
		". For Amazon's Echos use alexa."
}

// speakIn: speak with a room; false = it means this Echo alone (the caller speaks here).
func (d *device) speakIn(room, text string) (string, bool) {
	if d.peers == nil {
		return "", false
	}
	here, others, errText := d.speakersIn(room)
	if errText != "" {
		return errText, true
	}
	if len(others) == 0 {
		return "", false
	}
	body, _ := json.Marshal(roomSpeak{Text: text})
	results := d.toPeers(others, "/speak", body)
	if here {
		results = append(results, "here: "+d.svc.announcer.Speak(text))
	}
	return strings.Join(results, " "), true
}

// conversationIn: start_conversation with a room.
func (d *device) conversationIn(room string, req live.Request) (string, bool) {
	if d.peers == nil {
		return "", false
	}
	here, others, errText := d.speakersIn(room)
	if errText != "" {
		return errText, true
	}
	if here || len(others) == 0 {
		return "", false // "all" makes no sense for a conversation: here
	}
	body, _ := json.Marshal(roomConversation{Reason: req.Reason, Person: req.Person, Insist: req.Insist,
		RepeatMS: req.Repeat.Milliseconds()})
	return strings.Join(d.toPeers(others[:1], "/start_conversation", body), " "), true
}

func (d *device) toPeers(peers []peer.Peer, path string, body []byte) []string {
	var out []string
	for _, p := range peers {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		answer, err := d.peers.CallPeer(ctx, p, path, body)
		cancel()
		var a sharedText
		if err == nil {
			err = json.Unmarshal(answer, &a)
		}
		if err != nil {
			out = append(out, fmt.Sprintf("%s: Error: %v.", p.Name, err))
			continue
		}
		out = append(out, p.Name+": "+a.Text)
	}
	return out
}

func (d *device) serveSpeak(body []byte) ([]byte, error) {
	var q roomSpeak
	if err := json.Unmarshal(body, &q); err != nil {
		return nil, err
	}
	return json.Marshal(sharedText{Text: d.svc.announcer.Speak(q.Text)})
}

func (d *device) serveConversationStart(body []byte) ([]byte, error) {
	var q roomConversation
	if err := json.Unmarshal(body, &q); err != nil {
		return nil, err
	}
	out := d.requestConversation(live.Request{Reason: q.Reason, Person: q.Person, Insist: q.Insist,
		Repeat: time.Duration(q.RepeatMS) * time.Millisecond})
	return json.Marshal(sharedText{Text: out})
}
