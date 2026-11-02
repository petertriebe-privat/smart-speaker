package phone

import (
	"net"
	"strings"
	"testing"
	"time"

	"echo-assistant/internal/config"
	"echo-assistant/internal/live"
	"echo-assistant/internal/people"
	"echo-assistant/internal/store"
	"echo-assistant/internal/tools"
)

func TestSpeakerAccessList(t *testing.T) {
	h := newHarness(t, nil)
	// Nobody but the admin to begin with.
	if got := h.p.listSpeaker(); !strings.Contains(got, "nobody else") {
		t.Errorf("list at the start: %q", got)
	}
	if got := h.p.allowSpeaker("0171 1234567"); !strings.Contains(got, "may now ask") {
		t.Fatalf("allow: %q", got)
	}
	if got := h.p.allowSpeaker("01711234567"); !strings.Contains(got, "already released") {
		t.Errorf("allow twice: %q", got)
	}
	if got := h.p.listSpeaker(); !strings.Contains(got, "01711234567") {
		t.Errorf("list: %q", got)
	}
	// It survives a restart: the list is on the data partition, not in the secrets.
	again := New(config.Secrets{"SIP_USER": "u", "SIP_PASSWORD": "p"}, people.New(config.Secrets{}), h.sess, Deps{})
	if len(again.speakers) != 1 || again.speakers[0].Number != "01711234567" {
		t.Fatalf("after a restart: %+v", again.speakers)
	}
	if got := h.p.denySpeaker("0171 1234567"); !strings.Contains(got, "no longer released") {
		t.Fatalf("deny: %q", got)
	}
	if got := h.p.denySpeaker("0171 1234567"); !strings.Contains(got, "was not released") {
		t.Errorf("deny twice: %q", got)
	}
	third := New(config.Secrets{"SIP_USER": "u", "SIP_PASSWORD": "p"}, people.New(config.Secrets{}), h.sess, Deps{})
	if len(third.speakers) != 0 {
		t.Errorf("after a restart: %+v", third.speakers)
	}
	// What is not a number and not a person of the household is refused.
	for _, bad := range []string{"", "123"} {
		if got := h.p.allowSpeaker(bad); !strings.Contains(got, "Error") {
			t.Errorf("allow %q: %q", bad, got)
		}
	}
}

func TestOnlyTheAdminNeedsNoRelease(t *testing.T) {
	h := newHarness(t, config.Secrets{"PHONE_OWNER": "0171 7654321"})
	admin := &call{number: "01717654321"}
	if !h.p.maySpeaker(admin) {
		t.Error("the admin's own number needs a release")
	}
	other := &call{number: "03012345678"}
	if h.p.maySpeaker(other) {
		t.Error("a number nobody released may have the room")
	}
	h.p.allowSpeaker("030 12345678")
	if !h.p.maySpeaker(other) {
		t.Error("a released number may not have the room")
	}
	if h.p.maySpeaker(&call{number: ""}) {
		t.Error("a call without a number may have the room")
	}
}

// Taking the release away ends a connection that is running on exactly that number.
func TestDenyEndsTheRunningConnection(t *testing.T) {
	h := newHarness(t, config.Secrets{"PHONE_CALLERS": "0171 1234567"})
	from := h.reg.LocalAddr().(*net.UDPAddr)
	h.p.onRequest(invite("a1", "01711234567"), from)
	h.read(t)
	h.read(t)
	h.sess.set(live.Live)
	h.p.updateCall(time.Now())
	h.read(t) // 200 OK
	h.p.allowSpeaker("0171 1234567")
	if got := h.p.ConnectSpeaker("monitor"); !strings.Contains(got, "Connected") {
		t.Fatalf("connect: %q", got)
	}
	h.sess.end()
	if got := h.p.denySpeaker("0171 1234567"); !strings.Contains(got, "ended") {
		t.Fatalf("deny while connected: %q", got)
	}
	if m := h.read(t); m.Method != "BYE" {
		t.Fatalf("BYE %+v", m)
	}
	if h.p.InCall() || h.p.BridgeActive() {
		t.Error("the connection outlived the release")
	}
}

func TestSpeakerAccessTool(t *testing.T) {
	h := newHarness(t, nil)
	var r tools.Registry
	h.p.AddTools(&r)
	ctx := tools.Context{Channel: tools.Voice}
	if got := r.Call("speaker_access", `{"action":"list"}`, ctx); !strings.Contains(got, "nobody else") {
		t.Errorf("list: %q", got)
	}
	if got := r.Call("speaker_access", `{"action":"allow","who":"0171 1234567"}`, ctx); !strings.Contains(got, "may now ask") {
		t.Errorf("allow: %q", got)
	}
	if got := r.Call("speaker_access", `{"action":"deny","who":"0171 1234567"}`, ctx); !strings.Contains(got, "no longer") {
		t.Errorf("deny: %q", got)
	}
	if got := r.Call("speaker_access", `{"action":"sideways"}`, ctx); !strings.Contains(got, "allow, deny or list") {
		t.Errorf("unknown action: %q", got)
	}
	// Not for a guest on the phone.
	if got := r.Call("speaker_access", `{"action":"list"}`, tools.Context{Channel: tools.Stranger}); !strings.Contains(got, "unknown") {
		t.Errorf("a guest called speaker_access: %q", got)
	}
	// Only the admin: the tool is private, the register refuses everyone else.
	tools.Denied = func(person, tool string, private bool) string {
		if private && person != "admin" {
			return "Error: only the admin may do that."
		}
		return ""
	}
	defer func() { tools.Denied = nil }()
	if got := r.Call("speaker_access", `{"action":"list"}`, tools.Context{Channel: tools.Voice, Person: "kind"}); !strings.Contains(got, "only the admin") {
		t.Errorf("a child called speaker_access: %q", got)
	}
	if store.Dir == "" {
		t.Error("the list needs the data partition")
	}
}
