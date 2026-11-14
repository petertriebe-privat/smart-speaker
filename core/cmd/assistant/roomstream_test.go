package main

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"slices"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"echo-assistant/internal/config"
	"echo-assistant/internal/peer"
)

// "Ruf die Praxis an, ich will mithören" from the phone: the stream waits at the Echo the gateway
// picks (the living room) until the call at the study's Echo is picked up, then the call's audio
// and tones come to the phone and the phone's microphone goes onto the line; when the phone
// leaves, the call's room goes back at the study.
func TestRoomStreamWaitsAndPassesOn(t *testing.T) {
	px, py := testPort(t), testPort(t)
	x := peer.New(testPeerKey, px, []string{"127.0.0.1:" + strconv.Itoa(py)})
	y := peer.New(testPeerKey, py, []string{"127.0.0.1:" + strconv.Itoa(px)})
	x.Name, x.Room, y.Name, y.Room = "Jarvis Wohnzimmer", "Wohnzimmer", "Jarvis Arbeitszimmer", "Arbeitszimmer"
	var yState atomic.Value
	yState.Store("idle")
	x.State = func() string { return "idle" }
	y.State = func() string { return yState.Load().(string) }
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go x.Run(ctx)
	go y.Run(ctx)

	// The study's Echo with the call: plays a tone and a frame into the room, echoes the room's
	// microphone with a mark, and reports the modes it is asked for.
	modes := make(chan string, 4)
	y.Handle("/room/join", func(body []byte) ([]byte, error) {
		var q roomJoin
		json.Unmarshal(body, &q)
		key, _ := hex.DecodeString(q.Key)
		var from peer.Peer
		for _, p := range y.Alive() {
			if p.Name == q.From {
				from = p
			}
		}
		link, err := y.NewAudioLink(q.Session, key, from.Addr, from.Port+1)
		if err != nil {
			return nil, err
		}
		link.Handle(peer.AudioHandlers{OnPCM: func(pcm []int16) { link.SendPCM(append(pcm, 5)) }})
		modes <- q.Mode
		go func() {
			time.Sleep(50 * time.Millisecond)
			link.SendTone(660, 120, 0.15)
			link.SendPCM([]int16{4, 4})
		}()
		return json.Marshal(sharedText{Text: "The call is " + q.Mode + " at the other Echo now."})
	})
	y.Handle("/room/mode", func(body []byte) ([]byte, error) {
		var q roomModeReq
		json.Unmarshal(body, &q)
		modes <- q.Mode
		return json.Marshal(sharedText{Text: "ok"})
	})

	d := &device{cfg: &config.Config{Settings: config.Settings{Room: "Wohnzimmer"}}, peers: x}
	x.HandleAudio("/room/stream", d.serveRoomStream)
	for deadline := time.Now().Add(5 * time.Second); len(x.Alive()) == 0 || len(y.Alive()) == 0; time.Sleep(50 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatal("the two Echos do not see each other")
		}
	}
	phone := peer.New(testPeerKey, testPort(t), nil)
	at := peer.Peer{Addr: "127.0.0.1", Port: px + 1}
	dial := func(mode string, wait bool) (*peer.AudioLink, string) {
		session, key := newSession()
		body, _ := json.Marshal(roomStreamReq{Session: session, Key: hex.EncodeToString(key), From: "Jarvis Phone", Mode: mode, Wait: wait})
		var link *peer.AudioLink
		var out []byte
		var err error
		for i := 0; i < 40; i++ { // the call port may still be starting
			if link, out, err = phone.DialAudio(ctx, at, "/room/stream", body, session, key); err == nil {
				break
			}
			time.Sleep(50 * time.Millisecond)
		}
		if err != nil {
			t.Fatal(err)
		}
		var a sharedText
		json.Unmarshal(out, &a)
		return link, a.Text
	}

	// No call and no waiting: refused.
	link, answer := dial("listen", false)
	link.Close()
	if !strings.HasPrefix(answer, "Error: no call") {
		t.Fatalf("without a call: %q", answer)
	}
	// Waiting for the pick-up.
	link, answer = dial("talk", true)
	if !strings.HasPrefix(answer, "Waiting") {
		t.Fatalf("waiting: %q", answer)
	}
	pcm := make(chan []int16, 8)
	tones := make(chan float64, 2)
	link.Handle(peer.AudioHandlers{
		OnPCM:  func(p []int16) { pcm <- p },
		OnTone: func(freq float64, ms int, amp float64) { tones <- freq },
	})
	yState.Store("call") // picked up at the study; X hears it with the next heartbeat (5 s)
	select {
	case m := <-modes:
		if m != "talk" {
			t.Errorf("the study was asked for %q", m)
		}
	case <-time.After(2 * heartbeatTest):
		t.Fatal("the waiting stream did not join the call")
	}
	select {
	case f := <-tones:
		if f != 660 {
			t.Errorf("tone %v", f)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("no tone on the phone")
	}
	if got := <-pcm; !slices.Equal(got, []int16{4, 4}) {
		t.Errorf("the call came as %v", got)
	}
	link.SendPCM([]int16{1, 2})
	select {
	case got := <-pcm:
		if !slices.Equal(got, []int16{1, 2, 5}) {
			t.Errorf("the phone's microphone came back as %v", got)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("the phone's microphone does not reach the line")
	}
	// The phone leaves: the room goes back at the study.
	link.Send(peer.AudioEnd, nil)
	link.Close()
	select {
	case m := <-modes:
		if m != "off" {
			t.Errorf("after the phone left: %q", m)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("the study was not told that the phone left")
	}
}
