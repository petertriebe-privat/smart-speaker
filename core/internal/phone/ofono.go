package phone

import (
	"fmt"
	"strings"
	"sync"

	"github.com/godbus/dbus/v5"
)

// The user's mobile phone over Bluetooth (issue #51, the Echo only): oFono 1.5 is the hands-free
// unit of a paired phone and is driven over the system bus. It makes one modem per paired phone
// ("/hfp/<adapter>_<phone>", type "hfp"); while the modem is powered, oFono is BlueZ' Handsfree
// Agent and the phone's "calls" profile connects; unpowered, BlueZ turns it down and every call
// stays at the phone. devices/echo/docs/17-bluetooth.md has what was measured.

// handsfree: what the mobile call needs of oFono; *ofono satisfies it, the tests fake it.
type handsfree interface {
	// Modems: the paired phones oFono knows, with whether they are powered.
	Modems() ([]hfModem, error)
	SetPowered(modem string, on bool) error
	// Calls of one modem; State is oFono's: incoming, waiting, dialing, alerting, active, held,
	// disconnected.
	Calls(modem string) ([]hfCall, error)
	Answer(call string) error
	Hangup(call string) error
	Dial(modem, number string) error
}

type hfModem struct {
	Path    string
	Powered bool
	Online  bool // the service level connection is up
}

type hfCall struct {
	Path   string
	State  string
	Number string // LineIdentification: the other side's number, "" if withheld
}

// ofono talks to ofonod on the system bus. The connection is made on first use and again after
// it broke (ofonod or the bus restarted).
type ofono struct {
	mu   sync.Mutex
	conn *dbus.Conn
}

func (o *ofono) bus() (*dbus.Conn, error) {
	o.mu.Lock()
	defer o.mu.Unlock()
	if o.conn != nil && o.conn.Connected() {
		return o.conn, nil
	}
	conn, err := dbus.ConnectSystemBus()
	if err != nil {
		return nil, err
	}
	o.conn = conn
	return conn, nil
}

func (o *ofono) call(path, method string, out []any, args ...any) error {
	conn, err := o.bus()
	if err != nil {
		return err
	}
	call := conn.Object("org.ofono", dbus.ObjectPath(path)).Call(method, 0, args...)
	if call.Err != nil {
		return call.Err
	}
	if len(out) > 0 {
		return call.Store(out...)
	}
	return nil
}

// objects: the a(oa{sv}) answer of GetModems and GetCalls.
type objects []struct {
	Path  dbus.ObjectPath
	Props map[string]dbus.Variant
}

func (o *ofono) Modems() ([]hfModem, error) {
	var list objects
	if err := o.call("/", "org.ofono.Manager.GetModems", []any{&list}); err != nil {
		return nil, err
	}
	var modems []hfModem
	for _, m := range list {
		if text(m.Props["Type"]) != "hfp" && !strings.HasPrefix(string(m.Path), "/hfp/") {
			continue
		}
		modems = append(modems, hfModem{Path: string(m.Path), Powered: flag(m.Props["Powered"]), Online: flag(m.Props["Online"])})
	}
	return modems, nil
}

func (o *ofono) SetPowered(modem string, on bool) error {
	return o.call(modem, "org.ofono.Modem.SetProperty", nil, "Powered", dbus.MakeVariant(on))
}

func (o *ofono) Calls(modem string) ([]hfCall, error) {
	var list objects
	if err := o.call(modem, "org.ofono.VoiceCallManager.GetCalls", []any{&list}); err != nil {
		return nil, err
	}
	calls := make([]hfCall, 0, len(list))
	for _, c := range list {
		calls = append(calls, hfCall{Path: string(c.Path), State: text(c.Props["State"]), Number: text(c.Props["LineIdentification"])})
	}
	return calls, nil
}

func (o *ofono) Answer(call string) error { return o.call(call, "org.ofono.VoiceCall.Answer", nil) }
func (o *ofono) Hangup(call string) error { return o.call(call, "org.ofono.VoiceCall.Hangup", nil) }

func (o *ofono) Dial(modem, number string) error {
	var path dbus.ObjectPath
	if err := o.call(modem, "org.ofono.VoiceCallManager.Dial", []any{&path}, number, "default"); err != nil {
		return fmt.Errorf("dial: %w", err)
	}
	return nil
}

func text(v dbus.Variant) string {
	s, _ := v.Value().(string)
	return s
}

func flag(v dbus.Variant) bool {
	b, _ := v.Value().(bool)
	return b
}
