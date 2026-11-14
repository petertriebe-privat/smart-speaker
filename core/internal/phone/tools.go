package phone

import (
	"log"
	"strings"
	"time"

	"echo-assistant/internal/textutil"
	"echo-assistant/internal/tools"
)

// checkTarget: German numbers only, no emergency, short, premium or service numbers (a model must
// not be talked into calling them). "" or the error.
func checkTarget(number string) string {
	digits := textutil.PhoneNormalized(number)
	if strings.HasPrefix(digits, "00") {
		return "Error: only German numbers are called."
	}
	if !strings.HasPrefix(digits, "0") || len(digits) < 6 {
		return "Error: not a person of the household and not a complete phone number (with area code)."
	}
	for _, blocked := range []string{"0900", "0137", "0138", "0180", "0190", "0700", "01212"} {
		if strings.HasPrefix(digits, blocked) {
			return "Error: premium and service numbers (" + blocked + "...) are not called."
		}
	}
	return ""
}

// request: phone_call asked for a call; the loop dials it once no conversation runs.
func (p *Phone) request(number, reason string, stranger bool, person, by string) string {
	p.mu.Lock()
	p.wanted = &callRequest{number: number, reason: reason, person: person, by: by, stranger: stranger, at: time.Now()}
	p.mu.Unlock()
	if p.session.Open() {
		return "The call starts once the current conversation is over."
	}
	return "It rings in a moment."
}

// ownerNumber: the admin's number from PHONE_OWNER, else the first released one.
func (p *Phone) ownerNumber() string {
	if p.owner != "" {
		return p.owner
	}
	for _, entry := range p.callers {
		if !strings.HasPrefix(entry, "**") {
			return entry
		}
	}
	return ""
}

// numberOf: a person's number to dial: a mobile before the FritzBox's internal ones ("**6xx").
func (p *Phone) numberOf(phones []string, admin bool) string {
	number := ""
	if len(phones) > 0 {
		number = phones[0]
	}
	if strings.HasPrefix(number, "**") && len(phones) > 1 {
		number = phones[1]
	}
	if number == "" && admin {
		number = p.ownerNumber()
	}
	return number
}

// placeCall: one way to call for both kinds (the model does not choose): a person of the
// household (by name or by their number) gets the assistant itself, with memory and tools; anyone else
// gets a call on the user's behalf that knows only the task (no memory, no tools but hanging up),
// and the one who asked gets a report. Children may not have the assistant call outside the household.
func (p *Phone) placeCall(to, task string, ctx tools.Context) string {
	if !p.Configured() {
		return "Error: the phone is not set up (SIP_USER / SIP_PASSWORD)."
	}
	if !p.Registered() {
		return "Error: the phone is not registered at the FritzBox (not connected or this Echo does not handle calls); no call is possible."
	}
	to = strings.TrimSpace(to)
	var person string
	if to == "" {
		person = ctx.Person
		if person == "" {
			person = p.people.Admin()
		}
	} else {
		person = p.people.ByName(to)
	}
	if person == "" {
		person = p.people.ByPhone(to)
	}
	if person != "" {
		target, _ := p.people.Find(person)
		// The owner's phone is this speaker's hands-free unit right now, so they are at home:
		// a call to it would come back out of this speaker and through its microphone onto the
		// line again (#51).
		if target.Admin() && p.mobileOnline() {
			return "Error: " + target.Name + "'s mobile phone is connected with this speaker, so they are at home: " +
				"talk to them at the speaker (start_conversation) instead of calling."
		}
		number := p.numberOf(target.Phones, target.Admin())
		if number == "" {
			return "Error: no phone number registered for " + target.Name + "."
		}
		if ctx.Channel == tools.Voice && p.session.OnPhone() && person == ctx.Person {
			return "Error: you are on the phone with " + target.Name + " right now."
		}
		return "Calling " + target.Name + " (" + number + "), the assistant talks with them itself. " +
			p.request(number, task, false, person, ctx.Person)
	}
	if asking, ok := p.people.Find(ctx.Person); ok && asking.Child {
		return "Error: children may not have the assistant call people outside the household."
	}
	if err := checkTarget(to); err != "" {
		return err
	}
	if len(task) < 10 {
		return "Error: describe the task completely (what, when, for whom)."
	}
	by := "mail."
	if p.telegramConfigured() {
		by = "Telegram."
	}
	return "Calling " + textutil.PhoneNormalized(to) + " on the user's behalf. " +
		p.request(textutil.PhoneNormalized(to), task, true, "", ctx.Person) +
		" The result is reported afterwards by " + by
}

// dialForMe: the dial_for_me tool. The assistant dials and stays out of it: the call goes to the
// speaker and the microphone at home as soon as the other side picks up, and the user talks
// themselves. No session, so no transcript, no report and nothing to pay for the call itself.
// A number of the household is looked up here; for anyone else the model brings the number from
// the contacts (find_contact) or a web search.
func (p *Phone) dialForMe(to, via string, ctx tools.Context) string {
	if p.deps.Speaker == nil {
		return "Error: this device has no speaker to talk on."
	}
	to = strings.TrimSpace(to)
	if to == "" {
		return "Error: say who to call (a person of the household by name, or a phone number)."
	}
	person := p.people.ByName(to)
	if person == "" {
		person = p.people.ByPhone(to)
	}
	name, number := "", ""
	if person != "" {
		target, _ := p.people.Find(person)
		if number = p.numberOf(target.Phones, target.Admin()); number == "" {
			return "Error: no phone number registered for " + target.Name + "."
		}
		name = target.Name
	} else {
		if asking, ok := p.people.Find(ctx.Person); ok && asking.Child {
			return "Error: children may not have the assistant call people outside the household."
		}
		if err := checkTarget(to); err != "" {
			return err
		}
		number = textutil.PhoneNormalized(to)
	}
	if p.InCall() || p.MobileBusy() {
		return "Error: a call is already running."
	}
	route, err := p.dialRoute(via, ctx)
	if err != "" {
		return err
	}
	if route == "phone" {
		return p.mobileDialRequest(number, name, person, ctx.Person)
	}
	p.mu.Lock()
	p.wanted = &callRequest{number: number, name: name, person: person, by: ctx.Person,
		direct: true, at: time.Now()}
	p.mu.Unlock()
	who := number
	if name != "" {
		who = name + " (" + number + ")"
	}
	return "Dialling " + who + " over the landline. Two tones at the speaker say that they picked up; until then it " +
		"rings. Say in one short sentence who you are calling, nothing more: this conversation ends " +
		"now so the speaker is free for the call."
}

// pressKeys: the press_keys tool.
func (p *Phone) pressKeys(keys string) string {
	if !p.session.OnPhone() {
		return "Error: no phone call is running."
	}
	p.mu.Lock()
	line := p.call.line
	p.mu.Unlock()
	if line == nil || !line.Active() {
		return "Error: no phone call is running."
	}
	keys = strings.ReplaceAll(keys, " ", "")
	if keys == "" || len(keys) > 20 {
		return "Error: give 1 to 20 keys."
	}
	for _, key := range keys {
		if !(key >= '0' && key <= '9') && key != '*' && key != '#' {
			return "Error: only digits, * and # can be pressed."
		}
	}
	if !line.PressKeys(keys) {
		return "Error: the keys could not be sent (still busy with earlier ones?)."
	}
	log.Printf("[Phone] pressing %s", keys)
	return "Pressed " + keys + "."
}

// addDialForMe: the user calls someone themselves, over the landline or their mobile phone.
func (p *Phone) addDialForMe(r *tools.Registry) {
	via := ""
	if p.hf != nil {
		via = ` Over the user's mobile phone when it is connected with this speaker as hands-free unit, else over ` +
			`the landline; via names the line when the user does ('über das Festnetz' = landline, 'über mein Handy' = ` +
			`phone, which connects the phone for this one call). If one line fails the other takes over, and the room ` +
			`is told.`
	}
	r.Add(tools.Tool{
		Name: "dial_for_me",
		Description: "Calls someone and hands the call to the speaker and the microphone at home: the user talks with " +
			"them themselves, you are not in the call ('Rufe Anna an', 'verbinde mich mit der Praxis', 'ich will " +
			"selbst mit ihr reden'). That is the right tool whenever they name no task for you - with a task " +
			"('sag ihr, dass ...', 'frag nach einem Termin') it is phone_call. A person of the household by name; " +
			"anyone else as a phone number, which you look up in the contacts (find_contact) or by web search first - " +
			"confirm an unclear number with the user before dialling. German numbers only; children may call only " +
			"the household. Two tones at the speaker say that the other side picked up; 'Verbindung beenden' or the " +
			"action button ends the call. Only at the speaker at home." + via,
		Parameters: `{"type": "object", "additionalProperties": false,
            "properties": {"to": {"type": "string", "description": "a person of the household by name, or a phone number with area code, e.g. 07161 123456"},
                           "via": {"type": "string", "enum": ["auto", "phone", "landline"], "description": "the line; auto unless the user names one"}},
            "required": ["to"]}`,
		Channels: tools.Voice | tools.Console,
		Run: func(args map[string]any, ctx tools.Context) string {
			if ctx.Conversation != nil && ctx.Conversation.Label() == "phone" {
				return "Error: this is for the user at the speaker at home, not from a phone call."
			}
			answer := p.dialForMe(tools.String(args, "to"), tools.String(args, "via"), ctx)
			// The speaker belongs to the call now: end the conversation that asked for it.
			if ctx.Conversation != nil && !strings.HasPrefix(answer, "Error") {
				ctx.Conversation.EndSoon()
			}
			return answer
		},
	})
}

// AddTools registers phone_call and press_keys as the M5 does (end_call comes from package live).
// phone_call answers with an error while the phone is not registered.
func (p *Phone) AddTools(r *tools.Registry) {
	p.addMobileTools(r) // Bluetooth, no SIP needed
	if p.deps.Speaker != nil && (p.Configured() || p.hf != nil) {
		p.addDialForMe(r) // the landline or the mobile phone
	}
	if !p.Configured() {
		return
	}
	listenAt := ListenNowhere
	if p.deps.Speaker != nil {
		listenAt = ListenAtHome
	}
	r.Add(CallTool(listenAt, p.runCall))
	r.Add(tools.Tool{
		Name: "press_keys",
		Description: "Presses keys on the phone during the current call (touch tones), e.g. when an automatic announcement or a " +
			"menu asks for a key ('drücke drei' = '3', 'Raute' = '#', 'Stern' = '*'). Only during a phone call.",
		Parameters: `{"type": "object", "additionalProperties": false,
            "properties": {"keys": {"type": "string", "description": "the keys in order: digits, * and #, e.g. 3 or 1234#"}},
            "required": ["keys"]}`,
		Channels: tools.Voice | tools.Stranger,
		Run: func(args map[string]any, _ tools.Context) string {
			return p.pressKeys(tools.String(args, "keys"))
		},
	})
	p.addFollowupTool(r)
	if p.deps.Speaker == nil {
		return // no speaker on this device: no call on it either
	}
	r.Add(tools.Tool{
		Name: "speaker_call",
		Description: "Puts the phone call that is running on this speaker, so the user here follows it or takes " +
			"part in it ('lege das Gespräch auf den Lautsprecher', 'ich möchte mithören', 'ich möchte mitsprechen', " +
			"'Verbindung beenden'): the assistant's own call (outgoing or incoming) or the user's, also when it runs " +
			"at another of the assistant's speakers (it comes here then). " +
			"listen: the whole call, the other side and the assistant, is audible at home, the microphone stays off - say at " +
			"once that someone is listening in. talk: the microphone at home is on the line too - a three-way call, " +
			"The assistant stays in it and the user can step in (listen takes them out again). off: the call goes on without the speaker; " +
			"a call that is nothing but this connection ends with it. The action button at home does the same in " +
			"order. Only during a phone call.",
		Parameters: `{"type": "object", "additionalProperties": false,
            "properties": {"mode": {"type": "string", "enum": ["listen", "talk", "off"], "description": "listen = mithören, talk = mitsprechen, off = beenden"}},
            "required": ["mode"]}`,
		Channels: tools.Voice | tools.Console,
		Run: func(args map[string]any, ctx tools.Context) string {
			if ctx.Conversation != nil && ctx.Conversation.Label() == "phone" && !p.ConferenceActive() {
				return "Error: this is for the user at home. A caller is connected with connect_speaker."
			}
			// "Verbindung beenden" during a call on the mobile phone ends that one.
			if tools.String(args, "mode") == "off" && p.MobileActive() {
				return p.mobileAction("hangup")
			}
			// The call runs at another Echo: its room comes here (cmd/assistant/roomlink.go).
			if !p.CallHere() && p.deps.CallElsewhere != nil {
				if answer, ok := p.deps.CallElsewhere(tools.String(args, "mode")); ok {
					if ctx.Conversation != nil && !strings.HasPrefix(answer, "Error") && tools.String(args, "mode") != "off" {
						ctx.Conversation.EndSoon()
						answer += " Say one short sentence that it is on the speaker, nothing more: this conversation ends now."
					}
					return answer
				}
			}
			answer := p.SetBridge(tools.String(args, "mode"))
			// The conversation at home now has the speaker that the call needs: end it, else the
			// room would stay silent until the silence timer runs out.
			if ctx.Conversation != nil && !strings.HasPrefix(answer, "Error") && p.BridgeActive() {
				ctx.Conversation.EndSoon()
				answer += " Say one short sentence that it is on the speaker, nothing more: this conversation ends now."
			}
			return answer
		},
	})
	r.Add(tools.Tool{
		Name: "connect_speaker",
		Description: "Connects the caller of the running call with the speaker and the microphone at home, for the user " +
			"when they call from outside ('verbinde mich mit dem Lautsprecher zu Hause'). monitor: they hear the room " +
			"at home, nothing of theirs is audible there. talk: both ways, an intercom. Only for a caller who dialled " +
			"in from a released number, never for a guest. The conversation with you ends with it (the connection does " +
			"not need you and would cost money); at home it is announced and can be ended at any time. room: another of " +
			"the assistant's speakers ('verbinde mich mit dem Arbeitszimmer'); empty = the speaker that took the call. " +
			"In a call this is the way to any room, not intercom.",
		Parameters: `{"type": "object", "additionalProperties": false,
            "properties": {"mode": {"type": "string", "enum": ["monitor", "talk"], "description": "monitor = nur reinhören, talk = gegensprechen"},
                           "room": {"type": "string", "description": "room or name of the assistant speaker; empty = this one"}},
            "required": ["mode"]}`,
		// Also for a guest: a released number that is not in PHONE_CALLERS is a guest in the
		// conversation (no memory, no tools) and may still ask for the room. Who may is decided
		// in ConnectSpeaker, not by the channel.
		Channels: tools.Voice | tools.Stranger,
		Run: func(args map[string]any, ctx tools.Context) string {
			if ctx.Conversation != nil && ctx.Conversation.Label() != "phone" {
				return "Error: only the caller themselves asks for this, in the call. At home it is speaker_call."
			}
			mode := tools.String(args, "mode")
			if room := tools.String(args, "room"); room != "" && p.deps.OtherRoom != nil {
				if e := p.ConnectCheck(mode); e != "" {
					return e
				}
				// Another Echo fetches the call's room (ConnectSpeakerAt there through the peer link).
				if answer, elsewhere := p.deps.OtherRoom(room, mode); elsewhere {
					return answer
				}
			}
			return p.ConnectSpeaker(mode)
		},
	})
	p.addAccessTool(r)
}
