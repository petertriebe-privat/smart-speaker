package main

import (
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"echo-assistant/internal/phone"
	"echo-assistant/internal/tools"
)

// The phone's own tools (#75, level 1 and 2 of devices/atoms3r/docs/mobile.md): the app runs them
// on the phone (phone: PhoneTools.java) and answers with text. Only on the phone (board.Phone),
// only in its own conversations. Over the button connection (phoneboard.go):
//
//	The assistant -> app  "tool <id> {"name": …, "args": {…}}"
//	app -> the assistant  "result <id> "<text as a JSON string>""

const phoneToolTimeout = 20 * time.Second

var (
	phoneToolSeq     atomic.Uint64
	phoneToolMu      sync.Mutex
	phoneToolWaiting = map[string]chan string{}
)

// phoneToolCall runs a tool in the app and waits for its answer.
func phoneToolCall(name string, args map[string]any) string {
	p := phoneLink.Load()
	if p == nil {
		return "Error: the phone's app is not connected"
	}
	id := strconv.FormatUint(phoneToolSeq.Add(1), 10)
	ch := make(chan string, 1)
	phoneToolMu.Lock()
	phoneToolWaiting[id] = ch
	phoneToolMu.Unlock()
	defer func() {
		phoneToolMu.Lock()
		delete(phoneToolWaiting, id)
		phoneToolMu.Unlock()
	}()
	data, _ := json.Marshal(map[string]any{"name": name, "args": args})
	if err := p.write([]byte("tool " + id + " " + string(data) + "\n")); err != nil {
		return "Error: the phone's app: " + err.Error()
	}
	select {
	case out := <-ch:
		return out
	case <-time.After(phoneToolTimeout):
		return "Error: the phone's app did not answer"
	}
}

// phoneToolResult: a "result <id> <JSON string>" line from the app.
func phoneToolResult(line string) {
	id, rest, _ := strings.Cut(strings.TrimPrefix(line, "result "), " ")
	var text string
	if err := json.Unmarshal([]byte(rest), &text); err != nil {
		text = rest
	}
	phoneToolMu.Lock()
	ch := phoneToolWaiting[id]
	phoneToolMu.Unlock()
	if ch != nil {
		ch <- text
	}
}

// addPhoneTools: the phone's tools, on the phone only.
func (d *device) addPhoneTools() {
	add := func(name, description, parameters string) {
		d.registry.Add(tools.Tool{Name: name, Description: description, Parameters: parameters,
			Channels: tools.Voice | tools.Console,
			Run: func(args map[string]any, ctx tools.Context) string {
				return phoneToolCall(name, args)
			}})
	}
	str := func(desc string) string { return fmt.Sprintf(`{"type": "string", "description": %q}`, desc) }

	add("phone_navigate", "Starts navigation on the user's phone to a place (an address, a place's "+
		"name, a city): in the car on the Android Auto screen, else in Google Maps. For the route's "+
		"time or distance without starting it use the maps tools.",
		`{"type": "object", "additionalProperties": false, "properties": {"destination": `+str("where to, as the user said it or an address")+
			`, "mode": {"type": "string", "enum": ["driving", "walking", "bicycling", "transit"]}}, "required": ["destination"]}`)
	// The user talks themselves: the same tool and the same words as at the speakers at home
	// (dial_for_me), here over the phone's own line; the app calls it phone_call.
	d.registry.Add(tools.Tool{
		Name: "dial_for_me",
		Description: "Calls someone from the user's phone, and the user talks with them themselves (in the car over " +
			"its hands-free): 'Rufe Anna an', 'wähl die Praxis'. That is the right tool whenever they name no task " +
			"for you - with a task for you ('reservier einen Tisch', 'sag ihr, dass ...') it is phone_call. By name " +
			"from the phone's contacts, or a number. Say whom you call before: a call is not undone.",
		Parameters: `{"type": "object", "additionalProperties": false, "properties": {"to": ` +
			str("a contact's name or a phone number") + `}, "required": ["to"]}`,
		Channels: tools.Voice | tools.Console,
		Run: func(args map[string]any, ctx tools.Context) string {
			return phoneToolCall("phone_call", map[string]any{"who": tools.String(args, "to")})
		},
	})
	// The assistant calls with a task (a table, a practice, "ruf Anna an und sag ihr ..."): over the
	// landline at home, on the primary - the phone has no landline (install_phone.sh leaves SIP out).
	d.registry.Add(phone.CallTool(false, func(args map[string]any, ctx tools.Context) string {
		delete(args, "listen")
		delete(args, "talk")
		arguments, _ := json.Marshal(args)
		out, err := d.remoteTool("phone_call", string(arguments), ctx)
		if err != nil {
			return "Error: the speakers at home are not reachable for the call: " + err.Error()
		}
		return out
	}))
	add("phone_messages", "Reads the messages waiting on the user's phone: the notifications of "+
		"WhatsApp, Signal, Telegram, SMS and other messengers (sender, text, app, key). Read them out "+
		"or summarize them; the key answers one with phone_reply.",
		`{"type": "object", "additionalProperties": false, "properties": {"app": `+str("only this app, e.g. WhatsApp; empty = all")+`}}`)
	add("phone_reply", "Answers a message waiting on the phone in its own app (WhatsApp, Signal, …), "+
		"by the key from phone_messages, or by the sender's name. Read the answer back and send only "+
		"after the user agreed.",
		`{"type": "object", "additionalProperties": false, "properties": {"key": `+str("the message's key from phone_messages, or empty")+
			`, "sender": `+str("the sender's name when there is no key")+`, "text": `+str("the answer")+`}, "required": ["text"]}`)
	add("phone_sms", "Sends an SMS from the user's phone to a contact (by name from the phone's "+
		"contacts) or a number. Read it back and send only after the user agreed.",
		`{"type": "object", "additionalProperties": false, "properties": {"to": `+str("a contact's name or a phone number")+
			`, "text": `+str("the message")+`}, "required": ["to", "text"]}`)
	add("phone_media", "Controls what plays on the user's phone (Spotify, YouTube Music, a podcast, "+
		"the car's media): pause, play, next, previous.",
		`{"type": "object", "additionalProperties": false, "properties": {"action": {"type": "string", "enum": ["pause", "play", "next", "previous"]}}, "required": ["action"]}`)
	add("phone_open_app", "Opens an app on the user's phone by its name.",
		`{"type": "object", "additionalProperties": false, "properties": {"app": `+str("the app's name, e.g. Spotify")+`}, "required": ["app"]}`)
	add("phone_alarm", "Sets an alarm (time) or a timer (seconds) in the phone's clock app.",
		`{"type": "object", "additionalProperties": false, "properties": {"time": `+str("HH:MM for an alarm, or empty")+
			`, "seconds": {"type": "integer", "description": "a timer's length, or 0"}, "label": `+str("what for, or empty")+`}}`)
}
