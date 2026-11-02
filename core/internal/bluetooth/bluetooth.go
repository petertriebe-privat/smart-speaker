// Package bluetooth: pairing a phone with the Echo by voice (issue #51, step 3), over BlueZ 4 on
// the system bus. "Koppel mein Handy" opens a window of PAIR_WINDOW_S (two minutes): the Echo is
// visible as "Jarvis", the ring is blue, and a phone that pairs gets Secure Simple Pairing's
// numeric comparison - The assistant reads the six digits out and asks; a yes pairs it and makes it
// trusted, so it reconnects on its own from then on. A no, or no answer in time, refuses.
//
// Outside the window the Echo is not visible but connectable: known phones reconnect, new ones
// do not find it, and a pairing request from outside is refused without asking (it only goes to
// the log). Asking on every request was turned down in #51: it trains the yes and gives everyone
// in radio range a doorbell. The agent stays registered all the time for exactly that: without
// one, BlueZ would fall back to "just works".
//
// Contract for main: New, AddTools, then Run as a goroutine. Without bluetoothd it idles.
package bluetooth

import (
	"context"
	"crypto/rand"
	"fmt"
	"log"
	"math/big"
	"strings"
	"sync"
	"time"

	"echo-assistant/internal/errorlog"
	"echo-assistant/internal/prompts"
	"echo-assistant/internal/tools"
)

var (
	pairWindow  = time.Duration(prompts.Int("PAIR_WINDOW_S", 120)) * time.Second
	confirmWait = 20 * time.Second // BlueZ waits 25 s for the agent's answer
	adapterName = prompts.TextOr("BT_NAME", prompts.Name())
)

// The system notes (English, as all of them). {digits} is the number to compare, {pin} one to
// type in.
const (
	noteConfirm = "System note, not from the user: a phone is pairing with this speaker over Bluetooth. Read this " +
		"number out slowly, in two groups of three digits: {digits}. Ask whether the phone shows the same number. " +
		"With yes, call bluetooth_pair (confirm); with no, bluetooth_pair (reject). It must be answered within 20 seconds."
	notePin = "System note, not from the user: the phone asks for a code to pair with this speaker. Tell the user " +
		"to type {pin} on the phone, digit by digit."
	notePaired = "System note, not from the user: the phone {name} is paired now and connects on its own from now on. " +
		"Tell the user in one short sentence; calls stay at the phone until they say otherwise (mobile_handsfree)."
)

// Deps: what lives elsewhere.
type Deps struct {
	// Ask: a system note for the assistant at the speaker - into the open conversation, else the assistant
	// starts one with it (the user must hear the number while the phone waits).
	Ask func(text string)
	// Name: what the phone lists, the speaker's own ("Jarvis Arbeitszimmer"); "" = BT_NAME.
	// 22.10.2026 every Echo was "Jarvis", so a phone could not tell the rooms apart.
	Name string
}

// name: the adapter's name.
func (p *Pairing) name() string {
	if p.deps.Name != "" {
		return p.deps.Name
	}
	return adapterName
}

// Pairing: the agent, the window and the tools.
type Pairing struct {
	bz   blueZ
	deps Deps

	mu       sync.Mutex
	adapter  string    // BlueZ' default adapter ("" = bluetoothd not there)
	until    time.Time // the window is open until then
	question *question // a phone waits for the user's yes
}

type question struct {
	device string
	answer chan bool
}

// New: the pairing over the real system bus.
func New(deps Deps) *Pairing { return &Pairing{bz: &bus{}, deps: deps} }

// Run keeps the agent registered: every few seconds it looks at BlueZ' default adapter, and a
// new one (bluetoothd started or restarted) gets the name, "not visible, not pairable" and the
// agent. It closes the window when it is over.
func (p *Pairing) Run(ctx context.Context) {
	ticker := time.NewTicker(3 * time.Second)
	defer ticker.Stop()
	for {
		p.check(time.Now())
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

func (p *Pairing) check(now time.Time) {
	adapter, err := p.bz.Adapter()
	p.mu.Lock()
	known, until := p.adapter, p.until
	p.mu.Unlock()
	if err != nil {
		if known != "" {
			log.Printf("[Bluetooth] bluetoothd is gone: %v", err)
			p.mu.Lock()
			p.adapter, p.until = "", time.Time{}
			p.mu.Unlock()
		}
		return
	}
	if adapter != known {
		if err := p.setup(adapter); err != nil {
			log.Printf("[Bluetooth] adapter %s: %v", adapter, err)
			return
		}
		p.mu.Lock()
		p.adapter = adapter
		p.mu.Unlock()
		log.Printf("[Bluetooth] adapter %s ready, agent registered", adapter)
	}
	if !until.IsZero() && now.After(until) {
		p.close("the window is over")
	}
}

// setup: the name, closed, and the agent.
func (p *Pairing) setup(adapter string) error {
	if err := p.bz.Export(p); err != nil {
		return err
	}
	if err := p.bz.SetProperty(adapter, "Name", p.name()); err != nil {
		return err
	}
	p.bz.SetProperty(adapter, "Discoverable", false)
	p.bz.SetProperty(adapter, "Pairable", false)
	return p.bz.RegisterAgent(adapter, "DisplayYesNo")
}

// Open: the pairing window is open (the ring is blue).
func (p *Pairing) Open() bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	return !p.until.IsZero() && time.Now().Before(p.until)
}

func (p *Pairing) open() string {
	p.mu.Lock()
	adapter := p.adapter
	p.mu.Unlock()
	if adapter == "" {
		return NotRunning()
	}
	secs := uint32(pairWindow / time.Second)
	if err := p.bz.SetProperty(adapter, "DiscoverableTimeout", secs); err != nil {
		return "Error: " + err.Error()
	}
	p.bz.SetProperty(adapter, "Pairable", true)
	if err := p.bz.SetProperty(adapter, "Discoverable", true); err != nil {
		return "Error: " + err.Error()
	}
	p.mu.Lock()
	p.until = time.Now().Add(pairWindow)
	p.mu.Unlock()
	log.Printf("[Bluetooth] pairing window open for %s", pairWindow)
	return fmt.Sprintf("The speaker is visible as %q for %d minutes. Tell the user to choose %q in the phone's "+
		"Bluetooth settings. When the phone asks, you get a system note with a number to compare; stay in the "+
		"conversation until then.", p.name(), int(pairWindow/time.Minute), p.name())
}

func (p *Pairing) close(why string) {
	p.mu.Lock()
	adapter, wasOpen := p.adapter, !p.until.IsZero()
	p.until = time.Time{}
	p.mu.Unlock()
	if adapter != "" {
		p.bz.SetProperty(adapter, "Discoverable", false)
		p.bz.SetProperty(adapter, "Pairable", false)
	}
	if wasOpen {
		log.Printf("[Bluetooth] pairing window closed: %s", why)
	}
}

// --- The agent (org.bluez.Agent, called by bluetoothd) ---------------------------------------

// confirm: RequestConfirmation, numeric comparison. Inside the window the user is asked and the
// answer awaited; outside it the request is refused at once.
func (p *Pairing) confirm(device string, passkey uint32) error {
	if !p.Open() {
		refused(device, "numeric comparison")
		return errRejected
	}
	q := &question{device: device, answer: make(chan bool, 1)}
	p.mu.Lock()
	p.question = q
	p.mu.Unlock()
	defer func() {
		p.mu.Lock()
		if p.question == q {
			p.question = nil
		}
		p.mu.Unlock()
	}()
	digits := fmt.Sprintf("%06d", passkey)
	log.Printf("[Bluetooth] %s asks for numeric comparison", device)
	if p.deps.Ask != nil {
		go p.deps.Ask(strings.ReplaceAll(noteConfirm, "{digits}", digits[:3]+" "+digits[3:]))
	}
	select {
	case yes := <-q.answer:
		if !yes {
			log.Printf("[Bluetooth] %s refused by the user", device)
			return errRejected
		}
	case <-time.After(confirmWait):
		log.Printf("[Bluetooth] %s: no answer in time, refused", device)
		return errRejected
	}
	go p.paired(device)
	return nil
}

// code: RequestPinCode / RequestPasskey of an old phone without numeric comparison: inside the
// window the assistant names a code to type in, outside it is refused.
func (p *Pairing) code(device string, digits int) (string, error) {
	if !p.Open() {
		refused(device, "code")
		return "", errRejected
	}
	max := big.NewInt(1)
	for i := 0; i < digits; i++ {
		max.Mul(max, big.NewInt(10))
	}
	n, _ := rand.Int(rand.Reader, max)
	pin := fmt.Sprintf("%0*d", digits, n)
	if p.deps.Ask != nil {
		go p.deps.Ask(strings.ReplaceAll(notePin, "{pin}", pin))
	}
	go p.paired(device)
	return pin, nil
}

// authorize: a paired phone wants a service (calls, music). Known means paired; anything else
// is refused.
func (p *Pairing) authorize(device, uuid string) error {
	if d, err := p.bz.Device(device); err == nil && d.Paired {
		return nil
	}
	refused(device, "service "+uuid)
	return errRejected
}

// paired: after the yes BlueZ finishes the pairing; then the phone is made trusted (it reconnects
// on its own) and the window closes.
func (p *Pairing) paired(device string) {
	for i := 0; i < 20; i++ {
		time.Sleep(500 * time.Millisecond)
		d, err := p.bz.Device(device)
		if err != nil || !d.Paired {
			continue
		}
		if err := p.bz.SetTrusted(device); err != nil {
			log.Printf("[Bluetooth] %s not trusted: %v", device, err)
		}
		log.Printf("[Bluetooth] paired with %s", d.Name)
		p.close("paired")
		if p.deps.Ask != nil {
			p.deps.Ask(strings.ReplaceAll(notePaired, "{name}", d.Name))
		}
		return
	}
	log.Printf("[Bluetooth] %s did not finish pairing", device)
}

func refused(device, what string) {
	log.Printf("[Bluetooth] pairing request (%s) outside the window refused: %s", what, device)
	errorlog.Add("[Bluetooth] pairing request (%s) outside the window refused: %s", what, device)
}

// answer: the user's yes or no to the number (the tool).
func (p *Pairing) answer(yes bool) string {
	p.mu.Lock()
	q := p.question
	p.mu.Unlock()
	if q == nil {
		return "Error: no phone is waiting for an answer."
	}
	select {
	case q.answer <- yes:
	default:
	}
	if yes {
		return "Confirmed: the phone is being paired."
	}
	return "Refused: the phone is not paired."
}

// --- Tools ------------------------------------------------------------------------------------

func (p *Pairing) devices() string {
	p.mu.Lock()
	adapter := p.adapter
	p.mu.Unlock()
	if adapter == "" {
		return NotRunning()
	}
	list, err := p.bz.Devices(adapter)
	if err != nil {
		return "Error: " + err.Error()
	}
	var names []string
	for _, d := range list {
		if d.Paired {
			names = append(names, d.Name)
		}
	}
	if len(names) == 0 {
		return "No phone is paired."
	}
	return "Paired: " + strings.Join(names, ", ") + "."
}

func (p *Pairing) forget(name string) string {
	p.mu.Lock()
	adapter := p.adapter
	p.mu.Unlock()
	if adapter == "" {
		return NotRunning()
	}
	list, err := p.bz.Devices(adapter)
	if err != nil {
		return "Error: " + err.Error()
	}
	var match []device
	for _, d := range list {
		if d.Paired && (name == "" || strings.Contains(strings.ToLower(d.Name), strings.ToLower(strings.TrimSpace(name)))) {
			match = append(match, d)
		}
	}
	switch {
	case len(match) == 0:
		return "Error: no paired phone by that name. " + p.devices()
	case len(match) > 1:
		return "Error: more than one paired phone matches; name it exactly. " + p.devices()
	}
	if err := p.bz.RemoveDevice(adapter, match[0].Path); err != nil {
		return "Error: " + err.Error()
	}
	log.Printf("[Bluetooth] forgot %s", match[0].Name)
	return "Forgotten: " + match[0].Name + " is no longer paired; to use it again, pair it anew."
}

// AddTools: bluetooth_pair. Private and at the speaker only: pairing means standing in the room.
func (p *Pairing) AddTools(r *tools.Registry) {
	r.Add(tools.Tool{
		Name: "bluetooth_pair",
		Description: "Pairs a phone with this speaker over Bluetooth, and forgets one. start ('koppel mein Handy'): the " +
			"speaker is visible for two minutes; when the phone asks, you get the number to compare and ask the " +
			"user - stay in the conversation. confirm / reject: their yes or no to that number. list: the paired " +
			"phones. forget ('vergiss mein Handy'): unpair the one named (name may be part of it; empty if only one " +
			"is paired). stop: close the window early. Only at the speaker at home.",
		Parameters: `{"type": "object", "additionalProperties": false,
            "properties": {"action": {"type": "string", "enum": ["start", "confirm", "reject", "list", "forget", "stop"]},
                           "name": {"type": "string", "description": "forget: the phone's name or a part of it"}},
            "required": ["action"]}`,
		Private:  true,
		Channels: tools.Voice | tools.Console,
		Run: func(args map[string]any, ctx tools.Context) string {
			if ctx.Conversation != nil && ctx.Conversation.Label() == "phone" {
				return "Error: pairing is done at the speaker at home, not from a phone call."
			}
			switch tools.String(args, "action") {
			case "start":
				return p.open()
			case "confirm":
				return p.answer(true)
			case "reject":
				return p.answer(false)
			case "list":
				return p.devices()
			case "forget":
				return p.forget(tools.String(args, "name"))
			case "stop":
				p.close("stopped by the user")
				return "The speaker is not visible any more."
			}
			return "Error: action must be start, confirm, reject, list, forget or stop."
		},
	})
}
