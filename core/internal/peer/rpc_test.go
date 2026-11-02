package peer

import (
	"bytes"
	"context"
	"net/http"
	"strconv"
	"testing"
	"time"
)

func TestCallPrimary(t *testing.T) {
	pa, pb := freePort(t), freePort(t)
	a := New(testKey, pa, []string{"127.0.0.1:" + strconv.Itoa(pb)})
	b := New(testKey, pb, []string{"127.0.0.1:" + strconv.Itoa(pa)})
	a.Name, b.Name, a.Primary = "primary", "secondary", true
	a.Handle("/echo", func(body []byte) ([]byte, error) { return append([]byte("got "), body...), nil })
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go a.Run(ctx)
	go b.Run(ctx)
	deadline := time.Now().Add(3 * time.Second)
	for b.Leader() == nil {
		if time.Now().After(deadline) {
			t.Fatal("the secondary does not see the primary")
		}
		a.send(Message{Kind: "hb", Primary: true, Port: pa + 1})
		time.Sleep(50 * time.Millisecond)
	}
	if a.Leader() != nil {
		t.Error("the primary must not see a leader but itself")
	}
	var out []byte
	var err error
	for i := 0; i < 20; i++ { // the HTTP side may still be starting
		if out, err = b.CallPrimary(ctx, "/echo", []byte("hello")); err == nil {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if err != nil || string(out) != "got hello" {
		t.Fatalf("call: %q %v", out, err)
	}
	url := "http://127.0.0.1:" + strconv.Itoa(pa+1) + "/echo"
	resp, err := http.Post(url, "application/json", bytes.NewReader([]byte("x")))
	if err != nil || resp.StatusCode != http.StatusForbidden {
		t.Errorf("unsigned call: %v %v", resp, err)
	}
	// a replayed request
	req, _ := http.NewRequest(http.MethodPost, url, bytes.NewReader([]byte("y")))
	req.Header.Set(authHeader, b.sign(time.Now().UnixMilli(), "n1", "/echo", []byte("y")))
	if resp, err := http.DefaultClient.Do(req); err != nil || resp.StatusCode != http.StatusOK {
		t.Fatalf("first: %v %v", resp, err)
	}
	req, _ = http.NewRequest(http.MethodPost, url, bytes.NewReader([]byte("y")))
	req.Header.Set(authHeader, b.sign(time.Now().UnixMilli(), "n1", "/echo", []byte("y")))
	if resp, err := http.DefaultClient.Do(req); err != nil || resp.StatusCode != http.StatusForbidden {
		t.Errorf("replay: %v %v", resp, err)
	}
}
