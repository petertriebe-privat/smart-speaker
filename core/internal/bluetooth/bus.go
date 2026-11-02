package bluetooth

import (
	"errors"
	"strings"
	"sync"

	"github.com/godbus/dbus/v5"
)

// blueZ: what the pairing needs of bluetoothd; *bus satisfies it, the tests fake it.
type blueZ interface {
	Adapter() (string, error) // the default adapter's path (it changes with every bluetoothd)
	SetProperty(adapter, name string, value any) error
	RegisterAgent(adapter, capability string) error
	Export(p *Pairing) error // the agent object on the bus
	Devices(adapter string) ([]device, error)
	Device(path string) (device, error)
	SetTrusted(device string) error
	RemoveDevice(adapter, device string) error
}

type device struct {
	Path, Name, Address string
	Paired, Trusted     bool
}

const agentPath = "/jarvis/agent"

var errRejected = errors.New("org.bluez.Error.Rejected")

// bus: BlueZ 4 on the system bus. The connection is made on first use and again after it broke.
type bus struct {
	mu       sync.Mutex
	conn     *dbus.Conn
	exported bool
}

func (b *bus) get() (*dbus.Conn, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.conn != nil && b.conn.Connected() {
		return b.conn, nil
	}
	conn, err := dbus.ConnectSystemBus()
	if err != nil {
		return nil, err
	}
	b.conn, b.exported = conn, false
	return conn, nil
}

func (b *bus) call(path, method string, out []any, args ...any) error {
	conn, err := b.get()
	if err != nil {
		return err
	}
	c := conn.Object("org.bluez", dbus.ObjectPath(path)).Call(method, 0, args...)
	if c.Err != nil {
		return c.Err
	}
	if len(out) > 0 {
		return c.Store(out...)
	}
	return nil
}

func (b *bus) Adapter() (string, error) {
	var path dbus.ObjectPath
	err := b.call("/", "org.bluez.Manager.DefaultAdapter", []any{&path})
	return string(path), err
}

func (b *bus) SetProperty(adapter, name string, value any) error {
	return b.call(adapter, "org.bluez.Adapter.SetProperty", nil, name, dbus.MakeVariant(value))
}

func (b *bus) RegisterAgent(adapter, capability string) error {
	return b.call(adapter, "org.bluez.Adapter.RegisterAgent", nil, dbus.ObjectPath(agentPath), capability)
}

func (b *bus) Export(p *Pairing) error {
	conn, err := b.get()
	if err != nil {
		return err
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.exported {
		return nil
	}
	if err := conn.Export(&agent{p}, agentPath, "org.bluez.Agent"); err != nil {
		return err
	}
	b.exported = true
	return nil
}

func (b *bus) Devices(adapter string) ([]device, error) {
	var props map[string]dbus.Variant
	if err := b.call(adapter, "org.bluez.Adapter.GetProperties", []any{&props}); err != nil {
		return nil, err
	}
	paths, _ := props["Devices"].Value().([]dbus.ObjectPath)
	list := make([]device, 0, len(paths))
	for _, path := range paths {
		if d, err := b.Device(string(path)); err == nil {
			list = append(list, d)
		}
	}
	return list, nil
}

func (b *bus) Device(path string) (device, error) {
	var props map[string]dbus.Variant
	if err := b.call(path, "org.bluez.Device.GetProperties", []any{&props}); err != nil {
		return device{}, err
	}
	str := func(k string) string { s, _ := props[k].Value().(string); return s }
	flag := func(k string) bool { v, _ := props[k].Value().(bool); return v }
	d := device{Path: path, Name: str("Name"), Address: str("Address"), Paired: flag("Paired"), Trusted: flag("Trusted")}
	if alias := str("Alias"); alias != "" {
		d.Name = alias
	}
	// A phone whose name BlueZ never asked for has its address as alias ("44-C6-3C-..."): not
	// something to read out (17.10.2026). The last four digits tell two such phones apart.
	if d.Name == "" || strings.Count(d.Name, "-") == 5 || strings.Count(d.Name, ":") == 5 {
		tail := strings.NewReplacer(":", "", "-", "").Replace(d.Address)
		if len(tail) > 4 {
			tail = tail[len(tail)-4:]
		}
		d.Name = "Handy " + tail
	}
	return d, nil
}

func (b *bus) SetTrusted(path string) error {
	return b.call(path, "org.bluez.Device.SetProperty", nil, "Trusted", dbus.MakeVariant(true))
}

func (b *bus) RemoveDevice(adapter, path string) error {
	return b.call(adapter, "org.bluez.Adapter.RemoveDevice", nil, dbus.ObjectPath(path))
}

// agent: org.bluez.Agent as BlueZ 4 calls it. godbus runs each call in its own goroutine, so
// RequestConfirmation may wait for the user's answer.
type agent struct{ p *Pairing }

func rejected(err error) *dbus.Error {
	if err == nil {
		return nil
	}
	return dbus.NewError("org.bluez.Error.Rejected", []any{"refused"})
}

func (a *agent) Release() *dbus.Error { return nil }

func (a *agent) RequestConfirmation(device dbus.ObjectPath, passkey uint32) *dbus.Error {
	return rejected(a.p.confirm(string(device), passkey))
}

func (a *agent) RequestPinCode(device dbus.ObjectPath) (string, *dbus.Error) {
	pin, err := a.p.code(string(device), 4)
	return pin, rejected(err)
}

func (a *agent) RequestPasskey(device dbus.ObjectPath) (uint32, *dbus.Error) {
	pin, err := a.p.code(string(device), 6)
	if err != nil {
		return 0, rejected(err)
	}
	var n uint32
	for _, c := range pin {
		n = n*10 + uint32(c-'0')
	}
	return n, nil
}

func (a *agent) DisplayPasskey(device dbus.ObjectPath, passkey uint32) *dbus.Error { return nil }

func (a *agent) Authorize(device dbus.ObjectPath, uuid string) *dbus.Error {
	return rejected(a.p.authorize(string(device), uuid))
}

func (a *agent) ConfirmModeChange(mode string) *dbus.Error {
	if a.p.Open() {
		return nil
	}
	return rejected(errRejected)
}

func (a *agent) Cancel() *dbus.Error {
	a.p.mu.Lock()
	a.p.question = nil
	a.p.mu.Unlock()
	return nil
}
