package phone

import (
	"strings"

	"echo-assistant/internal/tools"
)

// Where the user listens in to a call the assistant makes (CallTool).
const (
	ListenNowhere = ""                       // a device without a speaker
	ListenAtHome  = "on the speaker at home" // the Echos
	ListenOnPhone = "on this phone"          // the phone's app: the call runs at home, its room comes here
)

// CallTool: phone_call, the assistant calls someone with a task and has the conversation itself.
// listenAt: where the user can listen in or talk along from the pick-up (listen, talk); the phone's
// app has no landline: it runs the call on the primary at home (cmd/assistant/phonetools.go) and
// takes the call's room onto the phone. run makes the call.
func CallTool(listenAt string, run func(args map[string]any, ctx tools.Context) string) tools.Tool {
	atHome := listenAt != ListenNowhere
	description := "Calls someone by phone over the landline and has the conversation by voice. A person of the " +
		"household (name, or '' for the user): The assistant talks with them itself, with memory and all tools ('ruf " +
		"mich an', a wake-up call, an urgent alert, 'ruf Anna an und sag ihr ...'). Anyone else (a phone number: book " +
		"a table, ask a shop or a practice): The assistant knows only the task there, and the result is reported " +
		"afterwards; confirm the details with the user first (number, date, time, people, name); the number from " +
		"find_contact, for a business from find_place. At a later time ('ruf mich um 7 an'): create_task. German " +
		"numbers only; children may call only the household. Only with a task for you: if the user wants to talk " +
		"themselves, or names no task at all ('Rufe Anna an'), it is dial_for_me. "
	listen := ""
	if listenAt == ListenOnPhone {
		description += "The call runs over the landline at home. "
	}
	if atHome {
		description += "listen: the user wants to hear the call " + listenAt + " from the start ('ich möchte " +
			"mithören'); talk: they also talk along from the start, a three-way call ('ich will mitreden', 'ich " +
			"rede mit'). During the call they can still step in, listen only or leave with speaker_call or the action " +
			"button; this conversation ends after your next sentence (the call has the speaker). "
		listen = `"listen": {"type": "boolean", "description": "true: audible ` + listenAt + ` from the pick-up"},
                           "talk": {"type": "boolean", "description": "true: the user talks along ` + listenAt + ` from the pick-up (three-way call)"},
                           `
	}
	description += "A call to someone else that does not reach its goal (busy, nobody answers, a mailbox, cut " +
		"off) is tried again by itself, and a call back from that number continues the task (call_followups). " +
		"report: 'call' when the user wants to be called back with the result ('ruf mich danach an', 'ruf mich " +
		"zurück'), else 'message'. Returns right away; the call starts once no other conversation runs."
	return tools.Tool{
		Name:        "phone_call",
		Description: description,
		Parameters: `{"type": "object", "additionalProperties": false,
            "properties": {"to": {"type": "string", "description": "a person of the household by name ('' = the user), or a phone number with area code, e.g. 07161 123456"},
                           "task": {"type": "string", "description": "in German: why the assistant calls and what to say, ask or arrange, with all details (for a reservation the user's name, what to accept and what not)"},
                           ` + listen + `"report": {"type": "string", "enum": ["message", "call"], "description": "how the user gets the result of a call to someone else: message (Telegram or mail) or call (The assistant calls them back)"}},
            "required": ["to", "task"]}`,
		Channels: tools.Everywhere,
		Run:      run,
	}
}

// runCall: phone_call on this device.
func (p *Phone) runCall(args map[string]any, ctx tools.Context) string {
	answer := p.placeCall(tools.String(args, "to"), tools.String(args, "task"), ctx)
	if tools.String(args, "report") == "call" && strings.Contains(answer, "on the user's behalf") {
		p.mu.Lock()
		if p.wanted != nil {
			p.wanted.reportCall = true
		}
		p.mu.Unlock()
		for _, by := range []string{"Telegram.", "mail."} {
			answer = strings.Replace(answer, "The result is reported afterwards by "+by,
				"The assistant calls the user back with the result afterwards.", 1)
		}
	}
	listen, _ := args["listen"].(bool)
	talk, _ := args["talk"].(bool)
	if (listen || talk) && !strings.HasPrefix(answer, "Error") {
		if p.deps.Speaker == nil {
			return answer + " (Listening in is not possible on this device: no speaker.)"
		}
		p.mu.Lock()
		if p.wanted != nil {
			p.wanted.listen, p.wanted.talk = true, talk
		}
		p.mu.Unlock()
		if talk {
			answer += " The user at home talks along from the pick-up (speaker and microphone); the other side is told so."
		} else {
			answer += " It is audible on the speaker at home from the pick-up; the other side is told so."
		}
		// The conversation that asked ends now: the speaker belongs to the call, and two of
		// The assistant talked at once (19.10.2026: this one went on narrating while the call ran).
		if ctx.Conversation != nil && ctx.Conversation.Label() == "speaker" {
			ctx.Conversation.EndSoon()
			answer += " Say only one short sentence that you are calling now; this conversation ends then."
		}
	}
	return answer
}
