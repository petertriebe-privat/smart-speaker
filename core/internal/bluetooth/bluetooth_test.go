package bluetooth

import (
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"echo-assistant/internal/errorlog"
	"echo-assistant/internal/tools"
)

// fakeBlueZ: bluetoothd with one adapter.
type fakeBlueZ struct {
	mu      sync.Mutex
	adapter string
	props   map[string]any
	agent   string
	devices map[string]*device
	removed []string
}

func newFake() *fakeBlueZ {
	return &fakeBlueZ{adapter: "/org/bluez/1/hci0", props: map[string]any{}, devices: map[string]*device{}}
}

func (f *fakeBlueZ) Adapter() (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.adapter == "" {
		return "", errors.New("org.freedesktop.DBus.Error.ServiceUnknown")
	}
	return f.adapter, nil
}

func (f *fakeBlueZ) SetProperty(adapter, name string, value any) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.props[name] = value
	return nil
}

func (f *fakeBlueZ) RegisterAgent(adapter, capability string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.agent = capability
	return nil
}

func (f *fakeBlueZ) Export(p *Pairing) error { return nil }

func (f *fakeBlueZ) Devices(adapter string) ([]device, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var list []device
	for _, d := range f.devices {
		list = append(list, *d)
	}
	return list, nil
}

func (f *fakeBlueZ) Device(path string) (device, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if d, ok := f.devices[path]; ok {
		return *d, nil
	}
	return device{}, errors.New("org.bluez.Error.DoesNotExist")
}

func (f *fakeBlueZ) SetTrusted(path string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.devices[path].Trusted = true
	return nil
}

func (f *fakeBlueZ) RemoveDevice(adapter, path string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.devices, path)
	f.removed = append(f.removed, path)
	return nil
}

func (f *fakeBlueZ) prop(name string) any {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.props[name]
}

func setup(t *testing.T) (*Pairing, *fakeBlueZ, *[]string, *sync.Mutex) {
	t.Helper()
	errorlog.Open(t.TempDir())
	bz := newFake()
	var notes []string
	var mu sync.Mutex
	p := &Pairing{bz: bz, deps: Deps{Ask: func(text string) { mu.Lock(); notes = append(notes, text); mu.Unlock() }}}
	p.check(time.Now())
	return p, bz, &notes, &mu
}

func TestTheAgentIsRegisteredAndClosed(t *testing.T) {
	p, bz, _, _ := setup(t)
	if bz.agent != "DisplayYesNo" {
		t.Errorf("agent %q, want DisplayYesNo (numeric comparison)", bz.agent)
	}
	if bz.prop("Name") != adapterName || bz.prop("Discoverable") != false || bz.prop("Pairable") != false {
		t.Errorf("adapter %v", bz.props)
	}
	// Outside the window every request is refused without asking.
	if err := p.confirm("/dev/a", 123456); err == nil {
		t.Error("paired outside the window")
	}
	if _, err := p.code("/dev/a", 4); err == nil {
		t.Error("gave a PIN outside the window")
	}
}

func TestTheSpeakersNameIsListed(t *testing.T) {
	errorlog.Open(t.TempDir())
	bz := newFake()
	p := &Pairing{bz: bz, deps: Deps{Ask: func(string) {}, Name: "Jarvis Arbeitszimmer"}}
	p.check(time.Now())
	if bz.prop("Name") != "Jarvis Arbeitszimmer" {
		t.Errorf("name %v", bz.prop("Name"))
	}
	if out := p.open(); !strings.Contains(out, `visible as "Jarvis Arbeitszimmer"`) {
		t.Errorf("start %q", out)
	}
}

func TestPairingWithNumericComparison(t *testing.T) {
	p, bz, notes, mu := setup(t)
	r := &tools.Registry{}
	p.AddTools(r)
	ctx := tools.Context{Channel: tools.Console}
	if out := r.Call("bluetooth_pair", `{"action": "start"}`, ctx); !strings.Contains(out, "visible") {
		t.Fatalf("start: %q", out)
	}
	if !p.Open() || bz.prop("Discoverable") != true || bz.prop("Pairable") != true {
		t.Fatal("the window is not open")
	}
	// The phone asks: The assistant gets the number, the user says yes.
	done := make(chan error, 1)
	go func() { done <- p.confirm("/dev/phone", 4521) }()
	waitFor(t, "the number for Jarvis", func() bool { mu.Lock(); defer mu.Unlock(); return len(*notes) == 1 })
	mu.Lock()
	if !strings.Contains((*notes)[0], "004 521") {
		t.Errorf("note %q, want the digits in two groups", (*notes)[0])
	}
	mu.Unlock()
	bz.mu.Lock()
	bz.devices["/dev/phone"] = &device{Path: "/dev/phone", Name: "Pixel"}
	bz.mu.Unlock()
	if out := r.Call("bluetooth_pair", `{"action": "confirm"}`, ctx); !strings.HasPrefix(out, "Confirmed") {
		t.Fatalf("confirm: %q", out)
	}
	if err := <-done; err != nil {
		t.Fatalf("refused after yes: %v", err)
	}
	bz.mu.Lock()
	bz.devices["/dev/phone"].Paired = true // BlueZ finishes the pairing
	bz.mu.Unlock()
	waitFor(t, "trusted and closed", func() bool {
		d, _ := bz.Device("/dev/phone")
		return d.Trusted && !p.Open()
	})
	if bz.prop("Discoverable") != false {
		t.Error("still visible after pairing")
	}
	waitFor(t, "told the user", func() bool { mu.Lock(); defer mu.Unlock(); return len(*notes) == 2 })
	if out := r.Call("bluetooth_pair", `{"action": "list"}`, ctx); !strings.Contains(out, "Pixel") {
		t.Errorf("list: %q", out)
	}
	// A paired phone may use its services, an unknown one not.
	if err := p.authorize("/dev/phone", "0000111f"); err != nil {
		t.Errorf("paired phone refused: %v", err)
	}
	if err := p.authorize("/dev/stranger", "0000111f"); err == nil {
		t.Error("an unknown device was authorized")
	}
	if out := r.Call("bluetooth_pair", `{"action": "forget", "name": "pix"}`, ctx); !strings.HasPrefix(out, "Forgotten") {
		t.Errorf("forget: %q", out)
	}
	if len(bz.removed) != 1 {
		t.Errorf("removed %v", bz.removed)
	}
}

func TestPairingRefusedByNoOrSilence(t *testing.T) {
	p, _, notes, mu := setup(t)
	p.open()
	done := make(chan error, 1)
	go func() { done <- p.confirm("/dev/phone", 111111) }()
	waitFor(t, "the number", func() bool { mu.Lock(); defer mu.Unlock(); return len(*notes) == 1 })
	p.answer(false)
	if err := <-done; err == nil {
		t.Error("paired after no")
	}
	confirmWait = 50 * time.Millisecond
	if err := p.confirm("/dev/phone", 222222); err == nil {
		t.Error("paired without an answer")
	}
	if out := p.answer(true); !strings.HasPrefix(out, "Error") {
		t.Errorf("a yes with nobody waiting: %q", out)
	}
}

func TestTheWindowClosesByItself(t *testing.T) {
	p, bz, _, _ := setup(t)
	p.open()
	p.check(time.Now().Add(pairWindow + time.Second))
	if p.Open() || bz.prop("Discoverable") != false {
		t.Error("the window stayed open")
	}
	// bluetoothd restarts: the new adapter gets the agent again.
	bz.mu.Lock()
	bz.adapter, bz.agent = "/org/bluez/2/hci0", ""
	bz.mu.Unlock()
	p.check(time.Now())
	if bz.agent == "" {
		t.Error("no agent on the new adapter")
	}
}

func waitFor(t *testing.T, what string, ok func() bool) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if ok() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}
