package main

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"net"
	"slices"
	"strconv"
	"testing"
	"time"

	"echo-assistant/internal/config"
	"echo-assistant/internal/peer"
)

const testPeerKey = "a peer key for the tests, long enough"

// heartbeatTest: a state change reaches the other Echo with the next heartbeat at the latest.
const heartbeatTest = 6 * time.Second

func testPort(t *testing.T) int {
	c, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	return c.LocalAddr().(*net.UDPAddr).Port
}

// The phone away from home asks for the study: the Echo the home gateway hands the stream to (the
// living room) passes the audio on to the study's Echo over UDP and back, and an end on the phone
// ends it there too.
func TestIntercomStreamPassedOn(t *testing.T) {
	px, py := testPort(t), testPort(t)
	x := peer.New(testPeerKey, px, []string{"127.0.0.1:" + strconv.Itoa(py)})
	y := peer.New(testPeerKey, py, []string{"127.0.0.1:" + strconv.Itoa(px)})
	x.Name, x.Room, y.Name, y.Room = "Jarvis Wohnzimmer", "Wohnzimmer", "Jarvis Arbeitszimmer", "Arbeitszimmer"
	idle := func() string { return "idle" }
	x.State, y.State = idle, idle
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go x.Run(ctx)
	go y.Run(ctx)

	// The study's Echo: echoes what comes in, with a mark, and reports the end.
	opened := make(chan intercomOpen, 1)
	ended := make(chan string, 4)
	y.Handle("/intercom/open", func(body []byte) ([]byte, error) {
		var q intercomOpen
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
		link.Handle(peer.AudioHandlers{
			OnPCM: func(pcm []int16) { link.SendPCM(append(pcm, 7)) },
			OnEnd: func() { ended <- "audio" },
		})
		opened <- q
		return json.Marshal(sharedText{Text: "ok"})
	})
	y.Handle("/intercom/close", func(body []byte) ([]byte, error) {
		ended <- "call"
		return json.Marshal(sharedText{Text: "ok"})
	})

	d := &device{cfg: &config.Config{Settings: config.Settings{Room: "Wohnzimmer"}}, peers: x}
	x.HandleAudio("/intercom/stream", d.serveIntercomStream)
	for deadline := time.Now().Add(5 * time.Second); len(x.Alive()) == 0 || len(y.Alive()) == 0; time.Sleep(50 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatal("the two Echos do not see each other")
		}
	}

	phone := peer.New(testPeerKey, testPort(t), nil)
	session, key := newSession()
	body, _ := json.Marshal(intercomOpen{Session: session, Key: hex.EncodeToString(key), From: "Jarvis Phone",
		Room: "Arbeitszimmer"})
	var link *peer.AudioLink
	var out []byte
	var err error
	for i := 0; i < 40; i++ { // the call port may still be starting
		if link, out, err = phone.DialAudio(ctx, peer.Peer{Addr: "127.0.0.1", Port: px + 1}, "/intercom/stream", body,
			session, key); err == nil {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if err != nil {
		t.Fatal(err)
	}
	var a intercomAnswer
	json.Unmarshal(out, &a)
	if a.With != "Jarvis Arbeitszimmer" {
		t.Fatalf("answer %+v", a)
	}
	if q := <-opened; q.Who != "Jarvis Phone" || q.From != "Jarvis Wohnzimmer" {
		t.Errorf("the study was opened as %+v", q)
	}
	got := make(chan []int16, 8)
	link.Handle(peer.AudioHandlers{OnPCM: func(pcm []int16) { got <- pcm }})
	link.SendPCM([]int16{1, 2, 3})
	select {
	case pcm := <-got:
		if !slices.Equal(pcm, []int16{1, 2, 3, 7}) {
			t.Errorf("came back as %v", pcm)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("nothing came back from the study")
	}
	// The phone ends it: the study hears the end frame and the call.
	link.Send(peer.AudioEnd, nil)
	link.Close()
	seen := map[string]bool{}
	for len(seen) < 2 {
		select {
		case how := <-ended:
			seen[how] = true
		case <-time.After(3 * time.Second):
			t.Fatalf("the study was not told the end (%v)", seen)
		}
	}
	icMu.Lock()
	left := len(icRelays)
	icMu.Unlock()
	if left != 0 {
		t.Errorf("%d relays left", left)
	}

	// A room nobody has: refused, nothing opened.
	session, key = newSession()
	body, _ = json.Marshal(intercomOpen{Session: session, Key: hex.EncodeToString(key), From: "Jarvis Phone", Room: "Keller"})
	refused, out, err := phone.DialAudio(ctx, peer.Peer{Addr: "127.0.0.1", Port: px + 1}, "/intercom/stream", body, session, key)
	if err != nil {
		t.Fatal(err)
	}
	refused.Close()
	a = intercomAnswer{}
	json.Unmarshal(out, &a)
	if a.With != "" || a.Text == "" || a.Text[:5] != "Error" {
		t.Errorf("an unknown room: %+v", a)
	}
}
