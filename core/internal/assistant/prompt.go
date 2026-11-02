// Package assistant: how the assistant is told what it knows, as devices/atoms3r/src/assistant/prompt.cpp
// and the instruction assembly of voice/live_session.cpp.
package assistant

import (
	"fmt"
	"time"

	"echo-assistant/internal/board"
	"echo-assistant/internal/live"
	"echo-assistant/internal/netx"
	"echo-assistant/internal/people"
	"echo-assistant/internal/prompts"
)

var weekdays = [...]string{"Sonntag", "Montag", "Dienstag", "Mittwoch", "Donnerstag", "Freitag", "Samstag"}

// Wired by main: the modules that exist on this device (nil = not there).
var (
	People *people.Register
	// Profile reads a field of a person's memory profile ("" = the admin's).
	Profile func(field, person string) string
	// Notes: the memory for a prompt (withIDs: for the backend).
	Notes func(withIDs bool, person string) string
	// Rules: the household's behaviour rules for person.
	Rules func(person string, withIDs bool) string
	// MCPSummary: one line per connected MCP server.
	MCPSummary func() string
	// Planned: scheduled tasks and the agenda for the prompt; voice: GPT Live's instructions
	// (the agenda remembers what this conversation was given), false for its backend.
	Planned func(req live.Request, voice bool) string
	// Room and Area: where this device stands (several Echos, #58); "" = not set.
	Room, Area string
	// Speakers: The assistant's other speakers with their rooms, one line ("" = none).
	Speakers func() string
	// RoomContext: the card on the TV in this room and the room's last conversation, while
	// recent (screen.RoomContext); "" = none.
	RoomContext func() string
)

func call(f func() string) string {
	if f == nil {
		return ""
	}
	return f()
}

// Context: the current time (and place), as prompt::context() on the M5.
func Context() string {
	if !netx.TimeSynced {
		return ""
	}
	now := time.Now()
	text := fmt.Sprintf("Current time: %s, %02d.%02d.%04d, %02d:%02d.", weekdays[now.Weekday()],
		now.Day(), int(now.Month()), now.Year(), now.Hour(), now.Minute())
	home := prompts.Text("LOCATION")
	if home == "" && Profile != nil {
		home = Profile("home", "")
	}
	if home != "" {
		text += " Location: " + home + "."
	}
	if Room != "" {
		area := ""
		if Area != "" && Area != Room {
			area = " (Home Assistant area " + Area + ")"
		}
		text += " This speaker stands in the room " + Room + area + ": 'hier', 'das Licht' or no room named " +
			"means this room."
	}
	if Speakers != nil {
		if s := Speakers(); s != "" {
			text += " the assistant's speakers (speak, start_conversation and intercom take one as room): " + s + "."
		}
	}
	return text
}

func tone(p people.Person) string {
	if p.Child {
		return prompts.Text("PERSONA_CHILD")
	}
	levels := prompts.List("PERSONA_LEVELS")
	level := min(People.Humor(p), len(levels)-1)
	text := levels[level]
	if level > 0 {
		text += prompts.Text("PERSONA_LIMITS")
	}
	return text
}

// Persona: The assistant's character for person; nobody named: the default, and for whom something
// else holds once it is clear who talks.
func Persona(person string) string {
	text := prompts.Text("PERSONA_CORE")
	if People != nil {
		if p, ok := People.Find(person); ok {
			return text + tone(p)
		}
	}
	levels := prompts.List("PERSONA_LEVELS")
	level := max(0, min(prompts.Int("PERSONA_LEVEL", 2), prompts.Int("PERSONA_MAX_LEVEL", 3), len(levels)-1))
	text += levels[level]
	if level > 0 {
		text += prompts.Text("PERSONA_LIMITS")
	}
	if People != nil {
		for _, p := range People.All() {
			if p.Child || People.Humor(p) != level {
				text += "# Once it is clear that you are talking with " + p.Name + ", this applies instead\n" + tone(p)
			}
		}
	}
	return text
}

// LiveInstructions for GPT Live at the speaker, in the M5's order: persona, instructions, MCP
// servers, binding rules, the occasion if the assistant speaks up itself, time, memory notes, plans.
func LiveInstructions(req live.Request) string {
	if req.Stranger {
		// Someone else is on the line: only the task, none of the user's notes, no tools but
		// hanging up.
		owner := "your owner"
		if Profile != nil {
			if name := Profile("name", ""); name != "" {
				owner = name
			}
		}
		if req.Callback != "" { // a number the assistant called for the user calls back
			return prompts.Text("LIVE_CALLBACK_INSTRUCTIONS") + "\nYour owner: " + owner + ". Caller: " + req.Caller +
				".\nTask of your earlier call: " + req.Callback + "\n" + Context()
		}
		if req.Reason == "" { // an unknown caller
			return prompts.Text("LIVE_GUEST_INSTRUCTIONS") + "\nYour owner: " + owner + ". Caller: " + req.Caller +
				".\n" + Context()
		}
		return prompts.Text("LIVE_CALL_INSTRUCTIONS") + "\nYou are calling on behalf of " + owner +
			". Number called: " + req.Caller + ".\nTask: " + req.Reason + "\n" + Context()
	}
	text := Persona(req.Person) + prompts.Text("LIVE_INSTRUCTIONS") + deviceAbilities() + "\n" + call(MCPSummary)
	if Rules != nil {
		text += Rules(req.Person, false)
	}
	switch {
	case req.Conference != "": // brought into a call on the speaker at home (the Echo, #54)
		text += prompts.Text("LIVE_CONFERENCE") + "- On the phone: " + req.Conference + ".\n"
	case req.Phone && req.Reason != "":
		text += prompts.Text("LIVE_PHONE_HINT") + prompts.Text("LIVE_PHONE_CALLING") + " " + req.Reason + "\n"
	case req.Phone:
		text += prompts.Text("LIVE_PHONE_HINT") + "- Caller: " + req.Caller + ".\n"
	case req.Reason != "":
		text += prompts.Text("LIVE_SPEAKER_CALLING")
		if req.Insist {
			text += prompts.Text("LIVE_SPEAKER_INSISTING")
		}
		text += "- Occasion: " + req.Reason + "\n"
	}
	text += Context() + "\n"
	if Notes != nil {
		text += Notes(false, req.Person)
	}
	if Planned != nil {
		text += Planned(req, true)
	}
	if !req.Phone {
		text += call(RoomContext)
	}
	return text
}

// BackendInstructions for the delegation model. The time last: it changes every minute, and
// OpenAI's prompt cache covers only what stays the same from the start.
func BackendInstructions(req live.Request) string {
	if req.Stranger {
		return prompts.Text("DELEGATION_CALL_INSTRUCTIONS")
	}
	text := prompts.Text("DELEGATION_INSTRUCTIONS") + "\n" + Persona(req.Person)
	switch {
	case board.FireTV:
		text += prompts.Text("DELEGATION_TV_CONTEXT") // the TV is this device (devices/android)
	case board.Phone:
		text += prompts.Text("DELEGATION_PHONE_CONTEXT") // the user's phone, in no room (#75)
	}
	if Rules != nil {
		text += Rules(req.Person, true)
	}
	if Notes != nil {
		text += Notes(true, req.Person)
	}
	if Planned != nil {
		text += Planned(req, false)
	}
	if !req.Phone {
		text += call(RoomContext)
	}
	return text + Context()
}

// deviceAbilities: what only this device can do. The Fire TV is a screen held by the remote's key
// (LIVE_TV_ABILITIES), the phone's app a button and a screen (LIVE_PHONE_ABILITIES); the Echos
// have the phone at the speaker, and Bluetooth except the Dot.
func deviceAbilities() string {
	switch {
	case board.FireTV:
		return prompts.Text("LIVE_TV_ABILITIES")
	case board.Phone:
		return prompts.Text("LIVE_PHONE_ABILITIES")
	}
	return prompts.Text("LIVE_ECHO_ABILITIES") + bluetoothAbilities()
}

// bluetoothAbilities: the mobile phone over Bluetooth, only where there is Bluetooth (not on the Dot).
func bluetoothAbilities() string {
	if board.Dot2 {
		return ""
	}
	return prompts.Text("LIVE_ECHO_BLUETOOTH")
}
