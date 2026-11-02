package phone

import (
	"log"
	"time"

	"echo-assistant/internal/tools"
)

// Which line a call takes (issue #51): "whose voice, whose number". The user talking themselves
// (dial_for_me) goes over their mobile phone when its hands-free connection stands - the one
// called sees their number and can call back - else over the landline, and the user can name
// the line ("über das Festnetz", "über mein Handy"). Over the phone without hands-free standing,
// it is connected for this call only and dropped again after it. The assistant's own calls (phone_call)
// always take the landline: his voice, not the user's number - and the phone stays free.
// Only the phone's owner (the admin) may dial over it: their SIM, their bill.

var mobileDialWait = ms("MOBILE_DIAL_WAIT_MS", 15000) // the phone's hands-free connection for one call

// mobileDial: a call to make over the phone once it is connected.
type mobileDial struct {
	number, name, person, by string
	at                       time.Time
}

// dialRoute: "phone" or "landline" for dial_for_me, or an error for the tool.
func (p *Phone) dialRoute(via string, ctx tools.Context) (string, string) {
	landline := p.Configured() && p.Registered()
	owner := ctx.Person == "" || ctx.Person == p.people.Admin()
	p.mu.Lock()
	paired, online := p.hf != nil && p.mob.paired && !p.mob.gone, p.mob.online
	p.mu.Unlock()
	phone := paired && owner
	switch via {
	case "landline":
		if !landline {
			return "", "Error: the landline is not registered at the FritzBox; no call over it is possible."
		}
		return "landline", ""
	case "phone":
		switch {
		case !paired:
			return "", "Error: no mobile phone is connected with this speaker over Bluetooth."
		case !owner:
			return "", "Error: only the phone's owner may call over it."
		}
		return "phone", ""
	}
	switch {
	case phone && online:
		return "phone", ""
	case landline:
		return "landline", ""
	case phone:
		return "phone", "" // the landline is down: the phone for this call
	}
	if !p.Configured() {
		return "", "Error: the phone is not set up (SIP_USER / SIP_PASSWORD)."
	}
	return "", "Error: the phone is not registered at the FritzBox (not connected or this Echo does not handle calls); no call is possible."
}

// mobileDialRequest: dial_for_me over the phone. Hands-free is connected for this call if it is
// not on anyway; the loop dials once it stands (mobileDialStep).
func (p *Phone) mobileDialRequest(number, name, person, by string) string {
	p.mu.Lock()
	p.mob.dial = &mobileDial{number: number, name: name, person: person, by: by, at: time.Now()}
	connecting := !p.mob.online
	if connecting && p.mob.mode != "on" {
		p.mob.oneCall = true // connected for this call, dropped after it
	}
	p.mob.powerRetry = time.Time{} // asked now: try at once
	p.mu.Unlock()
	p.mobileDuck(true)
	who := number
	if name != "" {
		who = name + " (" + number + ")"
	}
	answer := "Dialling " + who + " over the user's mobile phone"
	if connecting {
		answer += " (the phone is connected for this call first, a few seconds)"
	}
	return answer + ". The ringback plays at the speaker, two tones say that they picked up. Say in one short " +
		"sentence who you are calling, nothing more: this conversation ends now so the speaker is free for the call."
}

// mobileDialStep: the pending call over the phone is dialled once hands-free stands; if that
// does not happen within mobileDialWait, or dialling fails, the landline takes it, and the room
// is told.
func (p *Phone) mobileDialStep(now time.Time, modem string, online bool) {
	p.mu.Lock()
	d := p.mob.dial
	p.mu.Unlock()
	if d == nil {
		return
	}
	if online && modem != "" {
		err := p.hf.Dial(modem, d.number)
		p.mu.Lock()
		p.mob.dial = nil
		p.mu.Unlock()
		if err == nil {
			log.Printf("[Mobile] dialling %s over the phone", d.number)
			return
		}
		log.Printf("[Mobile] dialling over the phone failed: %v", err)
		p.mobileFallback(d, "Über das Handy ging es nicht")
		return
	}
	if now.Sub(d.at) < mobileDialWait {
		return
	}
	p.mu.Lock()
	p.mob.dial = nil
	p.mu.Unlock()
	log.Printf("[Mobile] the phone did not connect within %s", mobileDialWait)
	p.mobileFallback(d, "Das Handy ist nicht erreichbar")
}

// mobileFallback: the landline instead, if it can; the room hears why either way.
func (p *Phone) mobileFallback(d *mobileDial, why string) {
	p.mu.Lock()
	p.mob.oneCall = false
	p.mu.Unlock()
	p.mobileDuck(false)
	who := d.number
	if d.name != "" {
		who = d.name
	}
	if !p.Configured() || !p.Registered() || p.InCall() {
		p.say(why + ", " + who + " wird nicht angerufen.")
		return
	}
	p.mu.Lock()
	p.wanted = &callRequest{number: d.number, name: d.name, person: d.person, by: d.by, direct: true, at: time.Now()}
	p.mu.Unlock()
	p.say(why + ", ich rufe " + who + " übers Festnetz an.")
}
