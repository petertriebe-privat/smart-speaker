package peer

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"slices"
	"testing"
	"time"
)

// The phone away from home: an audio link over the call port (TCP), as through the home tunnel.
func TestAudioStream(t *testing.T) {
	pa := freePort(t)
	a := New(testKey, pa, nil)
	phone := New(testKey, freePort(t), nil)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go a.Run(ctx)
	serverEnded := make(chan bool, 1)
	a.HandleAudio("/talk", func(body []byte, link *AudioLink) ([]byte, bool) {
		var q struct {
			Room string `json:"room"`
		}
		json.Unmarshal(body, &q)
		if q.Room != "kitchen" {
			return []byte(`{"text":"Error: no such room"}`), false
		}
		link.SendPCM([]int16{9, 9}) // before the answer: dropped, it would break the answer
		link.Handle(AudioHandlers{
			OnPCM: func(pcm []int16) { link.SendPCM(append(pcm, 1)) }, // back, with a mark
			OnEnd: func() { serverEnded <- true },
		})
		return []byte(`{"text":"ok"}`), true
	})
	key := []byte("session key 0123")
	body := func(room string) []byte {
		b, _ := json.Marshal(map[string]any{"session": 5, "key": hex.EncodeToString(key), "room": room})
		return b
	}
	at := Peer{Addr: "127.0.0.1", Port: pa + 1}
	var link *AudioLink
	var answer []byte
	var err error
	for i := 0; i < 40; i++ { // the HTTP side may still be starting
		if link, answer, err = phone.DialAudio(ctx, at, "/talk", body("kitchen"), 5, key); err == nil {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if err != nil || string(answer) != `{"text":"ok"}` || !link.Streamed() {
		t.Fatalf("dial: %q %v", answer, err)
	}
	got := make(chan []int16, 4)
	link.Handle(AudioHandlers{OnPCM: func(pcm []int16) { got <- pcm }})
	link.SendPCM([]int16{1, -2, 32767})
	select {
	case pcm := <-got:
		if !slices.Equal(pcm, []int16{1, -2, 32767, 1}) {
			t.Errorf("came back as %v", pcm)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("no audio back")
	}
	link.Send(AudioEnd, nil)
	link.Close()
	select {
	case <-serverEnded:
	case <-time.After(3 * time.Second):
		t.Fatal("the other side did not hear the end")
	}
	// A refused call: its answer, and the connection is closed by the other side.
	refused, answer, err := phone.DialAudio(ctx, at, "/talk", body("cellar"), 6, key)
	if err != nil || string(answer) != `{"text":"Error: no such room"}` {
		t.Fatalf("refused: %q %v", answer, err)
	}
	for deadline := time.Now().Add(3 * time.Second); !refused.stream.closed.Load(); time.Sleep(20 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatal("a refused link stayed open")
		}
	}
	// Unsigned: refused before any stream.
	other := New("another key, long enough", freePort(t), nil)
	if _, _, err := other.DialAudio(ctx, at, "/talk", body("kitchen"), 7, key); err == nil {
		t.Error("a call under another key became a link")
	}
}
