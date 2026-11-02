package phone

import (
	"log"
	"strings"
	"time"

	"echo-assistant/internal/bluetooth"
	"echo-assistant/internal/store"
	"echo-assistant/internal/tools"
)

// addMobileTools: only where the mobile phone is enabled (the Echo).
func (p *Phone) addMobileTools(r *tools.Registry) {
	if p.hf == nil {
		return
	}
	r.Add(tools.Tool{
		Name: "mobile_call",
		Description: "The call on the user's mobile phone, which is connected with this speaker over Bluetooth: it rings " +
			"at home with the caller's name. answer: take it on the speaker and the microphone at home ('geh ran', " +
			"'nimm ab'); reject: turn it away ('lehne ab', 'drück ihn weg'); hangup: end the call that is on the " +
			"speaker ('leg auf', 'Verbindung beenden'). Only at the speaker at home.",
		Parameters: `{"type": "object", "additionalProperties": false,
            "properties": {"action": {"type": "string", "enum": ["answer", "reject", "hangup"]}},
            "required": ["action"]}`,
		Channels: tools.Voice | tools.Console,
		Run: func(args map[string]any, ctx tools.Context) string {
			if ctx.Conversation != nil && ctx.Conversation.Label() == "phone" && !p.ConferenceActive() { // The assistant in the call may end it
				return "Error: this is for the user at the speaker at home."
			}
			action := tools.String(args, "action")
			answer := p.mobileAction(action)
			// The call needs the speaker: end the conversation that asked for it.
			if ctx.Conversation != nil && action == "answer" && !strings.HasPrefix(answer, "Error") {
				ctx.Conversation.EndSoon()
				answer += " Say nothing more: this conversation ends now."
			}
			return answer
		},
	})
	r.Add(tools.Tool{
		Name: "mobile_handsfree",
		Description: "Whether this speaker is the hands-free unit of the user's mobile phone over Bluetooth. auto (the " +
			"default): the switch for calls in the phone's Bluetooth settings decides. on ('nutze mein Handy für " +
			"Telefonate', 'Anrufe vom Handy hier'): connected from here, every call of the phone rings here and is " +
			"held on the speaker. off ('Anrufe wieder aufs Handy'): kept disconnected, the phone keeps its calls. " +
			"on and off stay until changed. Music from the phone over Bluetooth works either way.",
		Parameters: `{"type": "object", "additionalProperties": false,
            "properties": {"mode": {"type": "string", "enum": ["auto", "on", "off"]}},
            "required": ["mode"]}`,
		Private:  true,
		Channels: tools.Voice | tools.Console,
		Run: func(args map[string]any, ctx tools.Context) string {
			mode := tools.String(args, "mode")
			switch mode {
			case "auto":
				mode = ""
			case "on", "off":
			default:
				return "Error: mode must be auto, on or off."
			}
			p.mu.Lock()
			p.mob.mode = mode
			p.mob.powerRetry, p.mob.powerFails = time.Time{}, 0 // asked anew: try at once
			gone, online, paired := p.mob.gone, p.mob.online, p.mob.paired
			p.mu.Unlock()
			p.saveMobileMode(mode)
			log.Printf("[Mobile] hands-free %q", mode)
			switch {
			case mode == "off":
				return "Off: the phone keeps its calls."
			case mode == "":
				return "Auto: the switch for calls in the phone's Bluetooth settings decides."
			case gone:
				return "Switched on for later, but Bluetooth calls do not work on this speaker right now. " +
					strings.TrimPrefix(bluetooth.NotRunning(), "Error: ")
			case !paired:
				return "Switched on, but no phone is paired yet: pair it first (bluetooth_pair)."
			case online:
				return "On: the phone's calls ring here."
			}
			return "On: the phone's calls ring here as soon as it is connected (it has to be in reach, and the switch " +
				"for calls in its Bluetooth settings must be on)."
		},
	})
}

// The hands-free mode on the data partition: an "off" must hold after a restart of the assistant.
const mobileModeFile = "mobile.json"

func (p *Phone) saveMobileMode(mode string) {
	if err := store.SaveJSON(mobileModeFile, map[string]string{"mode": mode}); err != nil {
		log.Printf("[Mobile] mode not saved: %v", err)
	}
}

func (p *Phone) loadMobileMode() {
	var saved map[string]string
	if store.LoadJSON(mobileModeFile, &saved) {
		p.mu.Lock()
		p.mob.mode = saved["mode"]
		p.mu.Unlock()
	}
}

// mobileDiallingLocked: a call over the phone rings at the other end (Dialling: the LED ring
// runs round, as for the landline).
func (p *Phone) mobileDiallingLocked() bool {
	m := &p.mob
	return m.dial != nil || m.outgoing && (m.state == "dialing" || m.state == "alerting")
}
