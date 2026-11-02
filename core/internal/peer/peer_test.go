package peer

import (
	"context"
	"net"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

const testKey = "0123456789abcdef0123456789abcdef" // gitleaks:allow (a test key)

func TestSealAndOpen(t *testing.T) {
	a := New(testKey, 1, []string{"127.0.0.1"})
	a.Name = "a"
	b := New(testKey, 1, []string{"127.0.0.1"})
	now := time.Now()
	m := Message{Kind: "hb", From: "a", Run: "r1", Seq: 1, At: now.UnixMilli()}
	data := a.seal(m)
	if got, ok := b.open(data, now); !ok || got.From != "a" {
		t.Fatalf("own key refused: %v %+v", ok, got)
	}
	if _, ok := b.open(data, now); ok {
		t.Error("replay accepted")
	}
	m.Run, m.Seq = "r2", 1 // the sender restarted
	if _, ok := b.open(a.seal(m), now); !ok {
		t.Error("a restarted sender refused")
	}
	tampered := strings.Replace(string(a.seal(Message{Kind: "hb", From: "a", Run: "r2", Seq: 5, At: now.UnixMilli()})), `"seq":5`, `"seq":6`, 1)
	if _, ok := b.open([]byte(tampered), now); ok {
		t.Error("tampered datagram accepted")
	}
	other := New("another key, long enough", 1, nil)
	if _, ok := b.open(other.seal(Message{Kind: "hb", From: "x", Run: "r", Seq: 1, At: now.UnixMilli()}), now); ok {
		t.Error("foreign key accepted")
	}
	old := a.seal(Message{Kind: "hb", From: "a", Run: "r2", Seq: 9, At: now.Add(-time.Minute).UnixMilli()})
	if _, ok := b.open(old, now); ok {
		t.Error("stale heartbeat accepted")
	}
	if New("short", 1, nil) != nil {
		t.Error("a short key must switch the link off")
	}
}

func TestDirectToKnownPeers(t *testing.T) {
	s := New(testKey, 47800, nil) // the broadcast address
	now := time.Now()
	s.peers["a"] = &Peer{Name: "a", Addr: "192.0.2.10", Seen: now.Add(-time.Minute)}
	s.peers["b"] = &Peer{Name: "b", Addr: "192.0.2.11", Seen: now.Add(-time.Hour)} // gone long
	s.peers["c"] = &Peer{Name: "c"}                                                // no address
	got := s.directLocked()
	if len(got) != 1 || got[0].String() != "192.0.2.10:47800" {
		t.Fatalf("direct: %v", got)
	}
	// a configured target is not sent twice
	s = New(testKey, 47800, []string{"192.0.2.10"})
	s.peers["a"] = &Peer{Name: "a", Addr: "192.0.2.10", Seen: now}
	if got := s.directLocked(); len(got) != 0 {
		t.Fatalf("target twice: %v", got)
	}
}

func TestDecide(t *testing.T) {
	at := time.Now().UnixMilli()
	own := Message{From: "kitchen", At: at, Level: 300, Prio: 1}
	if ok, _ := decide(own, nil); !ok {
		t.Error("alone must win")
	}
	if ok, why := decide(own, []Message{{From: "office", At: at + 200, Level: 500}}); ok || !strings.Contains(why, "office louder") {
		t.Errorf("a louder claim must win: %v %s", ok, why)
	}
	if ok, _ := decide(own, []Message{{From: "office", At: at - 100, Level: 120}}); !ok {
		t.Error("the louder one must win")
	}
	own.ID = "c1"
	if ok, why := decide(own, []Message{{From: "office", At: at, ID: "c1", Level: 400, Busy: true}}); ok || !strings.Contains(why, "conversation") {
		t.Errorf("a conversation where the user is louder keeps them: %v %s", ok, why)
	}
	if ok, _ := decide(own, []Message{{From: "office", At: at, ID: "c0", Level: 400, Busy: true}}); !ok {
		t.Error("a conversation's answer to another claim counted")
	}
	if ok, _ := decide(own, []Message{{From: "office", At: at + 5000, Level: 900}}); !ok {
		t.Error("another wake word five seconds later is not this one")
	}
	if ok, _ := decide(own, []Message{{From: "office", At: at, Level: 300, Prio: 2}}); ok {
		t.Error("a tie goes to the higher priority")
	}
	if ok, _ := decide(own, []Message{{From: "attic", At: at, Level: 300, Prio: 1}}); ok {
		t.Error("then to the name")
	}
}

func freePort(t *testing.T) int {
	c, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	return c.LocalAddr().(*net.UDPAddr).Port
}

// pair: two devices on loopback that see each other.
func pair(t *testing.T) (*Service, *Service) {
	t.Helper()
	pa, pb := freePort(t), freePort(t)
	a := New(testKey, pa, []string{"127.0.0.1:" + strconv.Itoa(pb)})
	b := New(testKey, pb, []string{"127.0.0.1:" + strconv.Itoa(pa)})
	a.Name, b.Name = "a", "b"
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	go a.Run(ctx)
	go b.Run(ctx)
	deadline := time.Now().Add(3 * time.Second)
	for len(a.Alive()) == 0 || len(b.Alive()) == 0 {
		if time.Now().After(deadline) {
			t.Fatal("the devices do not see each other")
		}
		// the first heartbeat may go out before the other listens: send again
		a.send(Message{Kind: "hb"})
		b.send(Message{Kind: "hb"})
		time.Sleep(50 * time.Millisecond)
	}
	return a, b
}

func TestTwoDevicesAgree(t *testing.T) {
	a, b := pair(t)
	a.Level = func(from, to time.Time) float64 { return 400 }
	b.Level = func(from, to time.Time) float64 { return 150 }
	at := time.Now()
	var wg sync.WaitGroup
	var wonA, wonB bool
	wg.Add(2)
	go func() { defer wg.Done(); wonA, _ = a.Claim(0.9, at) }()
	go func() { defer wg.Done(); wonB, _ = b.Claim(0.95, at.Add(80*time.Millisecond)) }()
	wg.Wait()
	if !wonA || wonB {
		t.Errorf("a (louder) %v, b %v: exactly the louder one answers", wonA, wonB)
	}
}

// A device whose model detects later (the M5, ~1 s) and heard the words louder still takes them.
func TestLateLouderClaimTakesIt(t *testing.T) {
	a, b := pair(t)
	a.Level = func(from, to time.Time) float64 { return 300 }
	b.Level = func(from, to time.Time) float64 { return 900 }
	outvoiced := make(chan string, 1)
	a.Outvoiced = func(why string) { outvoiced <- why }
	at := time.Now()
	if won, why := a.Claim(0.9, at); !won {
		t.Fatalf("a alone in the window must answer: %s", why)
	}
	if won, _ := b.Claim(0.8, at.Add(900*time.Millisecond)); !won {
		t.Error("b, louder and late, must answer")
	}
	select {
	case why := <-outvoiced:
		if !strings.Contains(why, "late") {
			t.Errorf("reason: %s", why)
		}
	case <-time.After(2 * time.Second):
		t.Error("a kept the words against a louder late claim")
	}
	b.Level = func(from, to time.Time) float64 { return 100 } // quieter and late: nothing changes
	time.Sleep(2 * sameWake)                                  // other words (b's claim was dated 0.9 s ahead)
	if won, _ := a.Claim(0.9, time.Now()); !won {
		t.Fatal("a must answer")
	}
	b.Claim(0.8, time.Now())
	select {
	case why := <-outvoiced:
		t.Errorf("a gave up against a quieter claim: %s", why)
	case <-time.After(500 * time.Millisecond):
	}
}

func TestConversationKeepsTheUser(t *testing.T) {
	a, b := pair(t)
	a.Level = func(from, to time.Time) float64 { return 200 }
	b.Level = func(from, to time.Time) float64 { return 600 } // the user stands at b
	b.Busy = func() bool { return true }
	if won, why := a.Claim(0.9, time.Now()); won {
		t.Errorf("a answered while the user talks to b: %s", why)
	}
	b.Level = func(from, to time.Time) float64 { return 50 } // b's conversation is elsewhere
	if won, why := a.Claim(0.9, time.Now()); !won {
		t.Errorf("a must answer when the user stands at a: %s", why)
	}
}
