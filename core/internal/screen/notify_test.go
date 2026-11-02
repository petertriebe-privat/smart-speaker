package screen

import "testing"

// Notify reaches the phones, their screen on or off, and no TV.
func TestNotifyPhones(t *testing.T) {
	s, f := setup(t)
	s.receive(prefix+"handy/status", []byte(`{"state":"online","display":"off","room":"Handy","kind":"phone"}`))
	s.receive(prefix+"handy_2/status", []byte(`{"state":"offline","display":"off","room":"Handy 2","kind":"phone"}`))
	if n := s.Notify(map[string]any{"kind": "info", "title": "T", "text": "x"}); n != 1 {
		t.Fatalf("%d phones notified, want 1", n)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.topics) != 1 || f.topics[0] != prefix+"handy/show" {
		t.Fatalf("published to %v", f.topics)
	}
}
