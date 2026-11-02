package main

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"echo-assistant/internal/audio"
	"echo-assistant/internal/config"
	"echo-assistant/internal/errorlog"
	"echo-assistant/internal/hometunnel"
	"echo-assistant/internal/input"
)

// The phone (board.Phone, devices/android/phone, #75): the app is microphone and speaker as on the
// Fire TV (internal/audio/tv.go). Its assistant button talks over one more connection, line by line:
//
//	app -> the assistant  "tap"     the Echo's action button: a conversation starts (GPT Live hears when
//	                         the user is done), or ends
//	               "hold 1"  held: push to talk, as the remote's key on the TV
//	               "hold 0"  let go
//	               "free"    after "hold 0": the press that started the conversation was a tap,
//	                         it goes on hands free (GPT Live) instead of push to talk
//	               "barge"   the user speaks over the assistant (the app's echo gate): it stops
//	The assistant -> app  "mic 1"   a conversation is open: the app streams its microphone
//	               "mic 0"   none: the app closes the microphone and streams silence
//	               "card {…}" the assistant's state and words for the console (screenSend), here instead of
//	                         over the broker, whose way through the data centre made them lag
//
// So the phone's microphone is open only while the button is held, a conversation runs, or the
// console is open in front (the wake word, the app's choice).

const phoneMicPoll = 150 * time.Millisecond

// phoneQuietEvery: what the speakers refresh every few minutes, the phone does this seldom (its
// battery, #75): the agenda and the mirror of the primary's state. A conversation asks the primary
// for its context anyway.
const phoneQuietEvery = 30 * time.Minute

// The home tunnel's local ports (internal/hometunnel): the primary's calls, the broker, Home
// Assistant.
const (
	tunnelRPC  = 47811
	tunnelMQTT = 41883
	tunnelHA   = 48123
)

// homeRemote: the primary's calls behind the tunnel, for the peer link (peers.go); "" without one.
var homeRemote string

// startHomeTunnel: with HOME_ENDPOINTS in the secrets ("gateway-in-the-LAN:port,vps:port") and the
// phone's certificates in state_dir/home, the house's services go over the home gateway, at home
// and away alike: the broker and Home Assistant through local ports, the primary's calls when no
// heartbeat shows it (away). Without them the phone uses the LAN as the speakers do.
func startHomeTunnel(cfg *config.Config) {
	t, err := hometunnel.New(cfg.Secrets.String("HOME_ENDPOINTS"), filepath.Join(cfg.Settings.StateDir, "home"))
	if err != nil {
		log.Printf("[Home tunnel] off: %v", err)
		errorlog.Add("Home tunnel: %v", err)
		return
	}
	if t == nil {
		return
	}
	ctx := context.Background()
	for _, l := range []struct {
		port int
		name string
	}{{tunnelRPC, hometunnel.RPC}, {tunnelMQTT, hometunnel.MQTT}, {tunnelHA, hometunnel.HA}} {
		if err := t.Listen(ctx, l.port, l.name); err != nil {
			log.Printf("[Home tunnel] off: %v", err)
			errorlog.Add("Home tunnel: %v", err)
			return
		}
	}
	cfg.Secrets["MQTT_URL"] = fmt.Sprintf("mqtt://127.0.0.1:%d", tunnelMQTT)
	cfg.Secrets["HA_URL"] = fmt.Sprintf("http://127.0.0.1:%d", tunnelHA)
	homeRemote = fmt.Sprintf("127.0.0.1:%d", tunnelRPC)
	log.Print("[Home tunnel] the broker, Home Assistant and the primary over the home gateway")
}

// phoneLink: the open button connection, nil without one. Its lines are written by phoneMic and
// phoneCard, one at a time.
var phoneLink atomic.Pointer[phoneConn]

type phoneConn struct {
	mu    sync.Mutex
	conn  net.Conn
	cards chan []byte
}

func (p *phoneConn) write(line []byte) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.conn.SetWriteDeadline(time.Now().Add(2 * time.Second))
	_, err := p.conn.Write(line)
	return err
}

// phoneCard hands a status card to the app; false without a connection (then it goes over MQTT).
// Never blocks: the screen's lock is held. The oldest waiting card gives way.
func (d *device) phoneCard(card map[string]any) bool {
	p := phoneLink.Load()
	if p == nil {
		return false
	}
	data, err := json.Marshal(card)
	if err != nil {
		return false
	}
	line := append(append([]byte("card "), data...), '\n')
	for {
		select {
		case p.cards <- line:
			return true
		default:
			select {
			case <-p.cards:
			default:
			}
		}
	}
}

// phoneHoldModel: a conversation started by holding the button is push to talk as on the Fire TV:
// the Realtime API without turn detection, the turn committed on release, billed by tokens. A tap
// starts GPT Live, which hears by itself when the user is done (01.11.2026: GPT Live with the
// button held answered slowly, the turn ended only by its own silence detection).
const phoneHoldModel = "gpt-realtime-mini"

func (d *device) phoneKeys() {
	for {
		err := d.phoneKeysOnce()
		log.Printf("[Phone app] button: %v, again in 1 s", err)
		time.Sleep(time.Second)
	}
}

func (d *device) phoneKeysOnce() error {
	conn, err := audio.AppKeys()
	if err != nil {
		return err
	}
	defer conn.Close()
	p := &phoneConn{conn: conn, cards: make(chan []byte, 16)}
	done := make(chan struct{})
	defer close(done)
	phoneLink.Store(p)
	defer phoneLink.CompareAndSwap(p, nil)
	go d.phoneMic(p, done)
	go func() {
		for {
			select {
			case <-done:
				return
			case line := <-p.cards:
				if p.write(line) != nil {
					conn.Close()
					return
				}
			}
		}
	}()
	button := false // the hold started a conversation: let go ends the button's press
	sc := bufio.NewScanner(conn)
	sc.Buffer(make([]byte, 64<<10), 1<<20) // a tool's answer (the messages) may be long
	for sc.Scan() {
		d.phoneKey(strings.TrimSpace(sc.Text()), &button)
	}
	if err := sc.Err(); err != nil {
		return err
	}
	return io.EOF
}

// phoneMic tells the app when a conversation needs its microphone. When one ends, the next is
// GPT Live again unless a hold chooses push to talk (a wake word's conversation needs GPT Live's
// turn detection).
func (d *device) phoneMic(p *phoneConn, done <-chan struct{}) {
	t := time.NewTicker(phoneMicPoll)
	defer t.Stop()
	sent := ""
	for {
		want := "mic 0\n"
		if d.listening() || d.session.Open() {
			want = "mic 1\n"
		}
		if want != sent {
			if p.write([]byte(want)) != nil {
				return
			}
			if want == "mic 0\n" && sent != "" {
				d.session.SetRealtime("")
			}
			sent = want
		}
		select {
		case <-done:
			return
		case <-t.C:
		}
	}
}

func (d *device) phoneKey(key string, button *bool) {
	if strings.HasPrefix(key, "result ") {
		phoneToolResult(key) // phonetools.go
		return
	}
	switch key {
	case "tap":
		log.Print("[Phone app] tap")
		if !(d.listening() || d.session.Open()) {
			d.session.SetRealtime("") // the conversation it starts: GPT Live
		}
		d.onInput(input.Event{Kind: input.ActionDown})
		d.onInput(input.Event{Kind: input.ActionUp})
	case "hold 1":
		log.Print("[Phone app] held")
		// A running conversation goes on; otherwise the press is the action button (a
		// conversation starts, a ringing timer stops), as the TV's key (tvboard.go).
		ringing := d.timers != nil && d.timers.Ringing()
		*button = ringing || !(d.listening() || d.session.Open())
		if *button && !ringing {
			d.session.SetRealtime(phoneHoldModel) // the conversation it starts: push to talk
		}
		d.session.PushToTalk(true) // Realtime: the user's turn; stops the assistant if it speaks
		d.screenKey(true)
		if *button {
			d.onInput(input.Event{Kind: input.ActionDown})
		}
	case "free":
		// The press that started the conversation was a tap: the app said "hold 1" at once (the
		// first word must not be lost), so it would have been push to talk on the Realtime API,
		// which never ends a turn by itself: The assistant heard and did not answer (01.11.2026). The
		// session connects only after the local listening (~2 s), so the choice still counts.
		log.Print("[Phone app] tapped: a hands-free conversation")
		d.session.SetRealtime("")
	case "barge":
		// The app heard the user over the assistant's voice (it gates its microphone while it plays):
		// he stops at once (live/bargein.go).
		d.session.BargeIn()
	case "hold 0":
		d.session.PushToTalk(false)
		d.screenKey(false)
		if *button {
			*button = false
			d.onInput(input.Event{Kind: input.ActionUp})
		}
	}
}
