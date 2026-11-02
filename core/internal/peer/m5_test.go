package peer

import (
	"testing"
	"time"
)

// What the M5 (devices/atoms3r/src/system/peer.cpp, #66) sends, signed independently of this
// package (Python's hmac): the C++ and the Go side must agree byte for byte.
const (
	m5Heartbeat = `J1 7990c41a9f042c31bedb858ac9220443fdc8bd1a1bc7d15cf9e0f805ddb03081 ` +
		`{"kind":"hb","state":"idle","room":"Test","version":"m5","no_lead":true,"from":"M5","at":1760000000000,"run":"0a1b2c3d4e5f","seq":1,"prio":0}`
	m5CallBody = `{"name":"remember","arguments":"{}"}`
	m5CallAuth = "1760000000000 a1b2c3d4e5f6 36d61d7ec383f1558e81c78a6ca3db642d7e8b271c3cbd937bab9af47b49999b"
)

func TestM5Datagram(t *testing.T) {
	s := New(testKey, 1, nil)
	s.Name = "echo"
	at := time.UnixMilli(1760000000000)
	m, ok := s.open([]byte(m5Heartbeat), at)
	if !ok {
		t.Fatal("the M5's heartbeat was refused")
	}
	if m.From != "M5" || m.Room != "Test" || !m.NoLead || m.Primary {
		t.Errorf("heartbeat read wrong: %+v", m)
	}
}

func TestM5Call(t *testing.T) {
	s := New(testKey, 1, nil)
	if got := s.sign(1760000000000, "a1b2c3d4e5f6", "/tool", []byte(m5CallBody)); got != m5CallAuth {
		t.Errorf("call signature differs from the M5's:\n got %s\nwant %s", got, m5CallAuth)
	}
	if !s.verify(m5CallAuth, "/tool", []byte(m5CallBody), time.UnixMilli(1760000000000)) {
		t.Error("the M5's call was refused")
	}
}
