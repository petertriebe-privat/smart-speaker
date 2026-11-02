package peer

import (
	"slices"
	"testing"
	"time"
)

func TestAudioLink(t *testing.T) {
	a := New(testKey, freePort(t), nil)
	b := New(testKey, freePort(t), nil)
	key := []byte("session key 0123")
	la, err := a.NewAudioLink(7, key, "127.0.0.1", b.AudioPort())
	if err != nil {
		t.Fatal(err)
	}
	lb, err := b.NewAudioLink(7, key, "127.0.0.1", a.AudioPort())
	if err != nil {
		t.Fatal(err)
	}
	got := make(chan []int16, 4)
	tones := make(chan float64, 1)
	ended := make(chan bool, 1)
	lb.OnPCM = func(pcm []int16) { got <- pcm }
	lb.OnTone = func(freq float64, ms int, amp float64) { tones <- freq }
	lb.OnEnd = func() { ended <- true }
	frame := []int16{1, -2, 32767, -32768}
	la.SendPCM(frame)
	select {
	case pcm := <-got:
		if !slices.Equal(pcm, frame) {
			t.Errorf("pcm %v", pcm)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("no audio")
	}
	la.SendTone(660, 140, 0.05)
	if f := <-tones; f != 660 {
		t.Errorf("tone %v", f)
	}
	// a frame under another key is dropped
	wrong, _ := a.NewAudioLink(8, []byte("other key"), "127.0.0.1", b.AudioPort())
	wrong.Session = 7
	wrong.SendPCM(frame)
	la.Send(AudioEnd, nil)
	select {
	case <-ended:
	case <-time.After(2 * time.Second):
		t.Fatal("no end")
	}
	select {
	case pcm := <-got:
		t.Errorf("a frame under a wrong key arrived: %v", pcm)
	default:
	}
}
