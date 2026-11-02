package phone

import (
	"log"
	"strings"
	"time"

	"echo-assistant/internal/store"
	"echo-assistant/internal/textutil"
	"echo-assistant/internal/tools"
)

// Who may have the speaker and the microphone at home from outside (issue #49). Not the same
// question as PHONE_CALLERS: that list says who gets the assistant with memory and tools, this one says
// who gets the room. By default only the admin - a released number opens a live microphone in
// someone else's home, so it is nobody's by accident.
//
// The list lives on the data partition and is changed by voice ("gib Anna den Lautsprecher
// frei", "nimm Anna die Freigabe"), by the admin only.

const speakerFile = "phone_speaker.json"

type speakerEntry struct {
	Number string `json:"number"`
	Name   string `json:"name,omitempty"`
	Since  string `json:"since"`
}

type speakerList struct {
	Numbers []speakerEntry `json:"numbers"`
}

func (p *Phone) loadSpeakerAccess() {
	var list speakerList
	if store.LoadJSON(speakerFile, &list) {
		p.speakers = list.Numbers
		log.Printf("[Phone] %d number(s) released for the speaker at home", len(p.speakers))
	}
}

// saveSpeakerAccessLocked: called with p.mu held.
func (p *Phone) saveSpeakerAccessLocked() {
	if err := store.SaveJSON(speakerFile, speakerList{Numbers: p.speakers}); err != nil {
		log.Printf("[Phone] could not save the speaker list: %v", err)
	}
}

// maySpeaker: may this caller have the room at home? The admin always (by their people entry or
// by PHONE_OWNER), everyone else only after being released. Called with p.mu held.
func (p *Phone) maySpeaker(c *call) bool {
	number := textutil.PhoneNormalized(c.number)
	if number == "" {
		return false
	}
	if admin := p.people.Admin(); admin != "" && c.person == admin {
		return true
	}
	if owner := p.ownerNumber(); owner != "" && owner == number {
		return true
	}
	for _, entry := range p.speakers {
		if entry.Number == number {
			return true
		}
	}
	return false
}

// speakerTarget: the number and name behind what the user said - a person of the household by
// name, or a phone number the model brought along (from the contacts or a web search).
func (p *Phone) speakerTarget(who string) (string, string, string) {
	who = strings.TrimSpace(who)
	if who == "" {
		return "", "", "Error: say whose number it is (a person of the household by name, or the number)."
	}
	person := p.people.ByName(who)
	if person == "" {
		person = p.people.ByPhone(who)
	}
	if person != "" {
		target, _ := p.people.Find(person)
		number := p.numberOf(target.Phones, target.Admin())
		if number == "" {
			return "", "", "Error: no phone number registered for " + target.Name + "."
		}
		return textutil.PhoneNormalized(number), target.Name, ""
	}
	number := textutil.PhoneNormalized(who)
	if len(number) < 6 {
		return "", "", "Error: not a person of the household and not a complete phone number (with area code)."
	}
	return number, "", ""
}

// allowSpeaker releases a number for the speaker and the microphone at home.
func (p *Phone) allowSpeaker(who string) string {
	number, name, err := p.speakerTarget(who)
	if err != "" {
		return err
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	for _, entry := range p.speakers {
		if entry.Number == number {
			return describeNumber(number, entry.Name) + " is already released."
		}
	}
	p.speakers = append(p.speakers, speakerEntry{Number: number, Name: name,
		Since: time.Now().Format("2006-01-02")})
	p.saveSpeakerAccessLocked()
	log.Printf("[Phone] %s released for the speaker at home", number)
	return describeNumber(number, name) + " may now ask for the speaker and the microphone at home. " +
		"Say it back, so the user hears which number it is."
}

// denySpeaker takes the release away again. A call that is running on it ends with it.
func (p *Phone) denySpeaker(who string) string {
	number, name, err := p.speakerTarget(who)
	if err != "" {
		return err
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	kept := p.speakers[:0]
	found := false
	for _, entry := range p.speakers {
		if entry.Number == number {
			found, name = true, entry.Name
			continue
		}
		kept = append(kept, entry)
	}
	if !found {
		return describeNumber(number, name) + " was not released."
	}
	p.speakers = kept
	p.saveSpeakerAccessLocked()
	log.Printf("[Phone] %s is no longer released for the speaker at home", number)
	// A connection that is running on exactly this number ends now, not at the next call.
	if p.call.stage == stageBridge && textutil.PhoneNormalized(p.call.number) == number {
		p.hangUp("the release was taken away")
		return describeNumber(number, name) + " is no longer released; the running connection is ended."
	}
	return describeNumber(number, name) + " is no longer released."
}

// listSpeaker: who may have the room, for the user asking.
func (p *Phone) listSpeaker() string {
	p.mu.Lock()
	defer p.mu.Unlock()
	admin := p.people.Name(p.people.Admin())
	if admin == "" {
		admin = "the admin"
	}
	if len(p.speakers) == 0 {
		return admin + " only; nobody else is released for the speaker and the microphone at home."
	}
	parts := make([]string, 0, len(p.speakers))
	for _, entry := range p.speakers {
		parts = append(parts, describeNumber(entry.Number, entry.Name)+" (since "+entry.Since+")")
	}
	return admin + ", and released: " + strings.Join(parts, ", ")
}

func describeNumber(number, name string) string {
	if name != "" {
		return name + " (" + number + ")"
	}
	return number
}

// addAccessTool registers speaker_access; only the admin may use it (Private).
func (p *Phone) addAccessTool(r *tools.Registry) {
	r.Add(tools.Tool{
		Name: "speaker_access",
		Description: "Who may ask for the speaker and the microphone at home from outside (connect_speaker), besides the " +
			"user themselves. allow releases a number ('gib Anna den Lautsprecher frei'), deny takes it back ('nimm " +
			"Anna die Freigabe') and ends a connection running on it, list says who is released. A released number " +
			"can listen into the home and talk into the room, so confirm with the user who it is before releasing, " +
			"and say the number back afterwards. A person of the household by name; anyone else as a phone number " +
			"from the contacts. Not the same as PHONE_CALLERS, which says who gets you with memory and tools.",
		Parameters: `{"type": "object", "additionalProperties": false,
            "properties": {"action": {"type": "string", "enum": ["allow", "deny", "list"]},
                           "who": {"type": "string", "description": "a person of the household by name, or a phone number; empty for list"}},
            "required": ["action"]}`,
		Channels: tools.Voice | tools.Console,
		Private:  true,
		Run: func(args map[string]any, _ tools.Context) string {
			who := tools.String(args, "who")
			switch tools.String(args, "action") {
			case "allow":
				return p.allowSpeaker(who)
			case "deny":
				return p.denySpeaker(who)
			case "list":
				return p.listSpeaker()
			}
			return "Error: action must be allow, deny or list."
		},
	})
}
