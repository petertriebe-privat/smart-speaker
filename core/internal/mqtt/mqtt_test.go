package mqtt

import (
	"bufio"
	"bytes"
	"net"
	"strings"
	"testing"
	"time"
)

func TestLength(t *testing.T) {
	for n, want := range map[int][]byte{0: {0}, 127: {0x7F}, 128: {0x80, 1}, 16383: {0xFF, 0x7F}, 321: {0xC1, 2}} {
		if got := appendLength(nil, n); !bytes.Equal(got, want) {
			t.Errorf("%d: %x, want %x", n, got, want)
		}
		r := bufio.NewReader(bytes.NewReader(append(append([]byte{0x30}, want...), make([]byte, n)...)))
		if _, body, err := readPacket(r); err != nil || len(body) != n {
			t.Errorf("%d: read %d %v", n, len(body), err)
		}
	}
}

func TestTopicID(t *testing.T) {
	for in, want := range map[string]string{"Jarvis Küche": "jarvis_kueche", "echo-2": "echo_2", " Bad  OG ": "bad_og"} {
		if got := TopicID(in); got != want {
			t.Errorf("%q: %q, want %q", in, got, want)
		}
	}
}

// A fake broker: checks the login and the will, answers CONNACK, records SUBSCRIBE and PUBLISH,
// and sends one message to the client.
func TestSession(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	got := make(chan string, 8)
	go func() {
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		defer conn.Close()
		r := bufio.NewReader(conn)
		kind, body, _ := readPacket(r)
		if kind != 1 || !bytes.Contains(body, []byte("assistant/x/status")) || !bytes.Contains(body, []byte("secret")) ||
			body[7]&0xC6 != 0xC6 {
			got <- "bad connect"
			return
		}
		conn.Write([]byte{0x20, 2, 0, 0})
		for {
			kind, body, err := readPacket(r)
			if err != nil {
				return
			}
			switch kind {
			case 8:
				got <- "subscribe " + string(body[4:len(body)-1])
				conn.Write(publishPacket("assistant/x/volume/set", []byte("55"), false))
			case 3:
				m, _ := parsePublish(body)
				got <- "publish " + m.Topic + " " + string(m.Payload)
			}
		}
	}()
	received := make(chan Message, 1)
	c, err := New(Options{URL: "mqtt://" + ln.Addr().String(), User: "jarvis", Password: "secret", ClientID: "x",
		WillTopic: "assistant/x/status", WillPayload: "offline", KeepAlive: time.Minute,
		OnConnect: func(c *Client) {
			c.Publish("assistant/x/status", []byte("online"), true)
			c.Subscribe("assistant/x/+/set")
		},
		OnMessage: func(m Message) { received <- m }})
	if err != nil {
		t.Fatal(err)
	}
	go c.session()
	want := []string{"publish assistant/x/status online", "subscribe assistant/x/+/set"}
	for _, w := range want {
		select {
		case g := <-got:
			if g != w {
				t.Fatalf("broker saw %q, want %q", g, w)
			}
		case <-time.After(3 * time.Second):
			t.Fatalf("broker saw nothing, want %q", w)
		}
	}
	select {
	case m := <-received:
		if m.Topic != "assistant/x/volume/set" || string(m.Payload) != "55" {
			t.Fatalf("received %+v", m)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("no message")
	}
	if !c.Connected() {
		t.Fatal("not connected")
	}
}

func TestBadURL(t *testing.T) {
	for _, u := range []string{"", "http://ha:1883", "mqtt://"} {
		if _, err := New(Options{URL: u}); err == nil || !strings.Contains(err.Error(), "mqtt") {
			t.Errorf("%q: %v", u, err)
		}
	}
}
