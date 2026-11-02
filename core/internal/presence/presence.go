// Package presence: who is at home, from Home Assistant's person entities (the HA app on the
// phones), as devices/atoms3r/src/services/presence.cpp. Fetched about once a minute and matched to
// the people register by id or name: person.alex -> alex.
package presence

import (
	"encoding/json"
	"fmt"
	"log"
	"strings"
	"sync"
	"time"

	"echo-assistant/internal/homeassistant"
	"echo-assistant/internal/people"
	"echo-assistant/internal/prompts"
	"echo-assistant/internal/tools"
)

type entry struct {
	person, state, since string
}

// Maps: the address lookup for locate_person (package maps).
type Maps interface {
	Address(lat, lon float64) (string, string)
	KmFromHome(lat, lon float64) float64
}

type Presence struct {
	// OnChange hears a person come home or leave (state "home" or another), not the first state
	// seen after a start; called with the lock released. Nil = only logged.
	OnChange func(person, state string)

	ha     *homeassistant.HA
	people *people.Register
	maps   Maps

	mu        sync.Mutex
	entries   []entry
	fetchedAt time.Time
	failures  int
}

func New(ha *homeassistant.HA, p *people.Register, m Maps) *Presence {
	return &Presence{ha: ha, people: p, maps: m}
}

func (p *Presence) fresh() bool {
	return !p.fetchedAt.IsZero() && time.Since(p.fetchedAt) < time.Duration(prompts.Int("PRESENCE_STALE_MS", 300000))*time.Millisecond
}

// localTime: "2026-09-29T05:10:03.123+00:00" -> local "05:10" today, else "28.09. 19:32".
func localTime(iso string) string {
	t, err := time.Parse(time.RFC3339Nano, iso)
	if err != nil {
		return ""
	}
	t = t.Local()
	if t.Format("2006-01-02") == time.Now().Format("2006-01-02") {
		return t.Format("15:04")
	}
	return t.Format("02.01. 15:04")
}

type state struct {
	EntityID    string `json:"entity_id"`
	State       string `json:"state"`
	LastChanged string `json:"last_changed"`
	LastUpdated string `json:"last_updated"`
	Attributes  struct {
		FriendlyName string   `json:"friendly_name"`
		Latitude     *float64 `json:"latitude"`
		Longitude    *float64 `json:"longitude"`
		GPSAccuracy  float64  `json:"gps_accuracy"`
	} `json:"attributes"`
}

func (p *Presence) states() ([]state, string) {
	status, body, err := p.ha.DomainStates("person") // not all of /api/states (#85)
	if err != nil || status != 200 {
		return nil, homeassistant.Failure(status, body, err)
	}
	var all []state
	if json.Unmarshal(body, &all) != nil {
		return nil, "Error: Home Assistant's answer is not readable"
	}
	var persons []state
	for _, s := range all {
		if strings.HasPrefix(s.EntityID, "person.") {
			persons = append(persons, s)
		}
	}
	return persons, ""
}

// match: person.alex or its name "Alex" -> the register's id.
func (p *Presence) match(s state) string {
	return matchPerson(p.people.ByName, s.EntityID, s.Attributes.FriendlyName)
}

// matchPerson: the entity's id, then its name, then the first names of both (person.alex_smith,
// "Alex Smith" -> alex: HA names a person by its user's full name; a new instance had
// person.<first>_<last> where the old one had person.<first>, and nobody was found, 20.10.2026).
func matchPerson(byName func(string) string, entityID, friendly string) string {
	id := strings.TrimPrefix(entityID, "person.")
	first := func(s string) string {
		if f := strings.FieldsFunc(s, func(r rune) bool { return r == '_' || r == ' ' }); len(f) > 0 {
			return f[0]
		}
		return ""
	}
	for _, name := range []string{id, friendly, first(id), first(friendly)} {
		if name == "" {
			continue
		}
		if person := byName(name); person != "" {
			return person
		}
	}
	return ""
}

// fetchNow returns "" or an error text.
func (p *Presence) fetchNow() string {
	persons, errText := p.states()
	if errText != "" {
		return errText
	}
	var found []entry
	for _, s := range persons {
		person := p.match(s)
		if person != "" && s.State != "" && s.State != "unknown" && s.State != "unavailable" {
			found = append(found, entry{person, s.State, localTime(s.LastChanged)})
		}
	}
	p.mu.Lock()
	var changed []entry
	for _, e := range found { // changes in the log, not every minute
		same, known := false, false
		for _, old := range p.entries {
			same = same || old.person == e.person && old.state == e.state
			known = known || old.person == e.person
		}
		if !same {
			log.Printf("[Anwesenheit] %s: %s", e.person, e.state)
			// home <-> away only: a move between two zones away is no arrival
			if known && (e.state == "home") != (p.stateLocked(e.person) == "home") {
				changed = append(changed, e)
			}
		}
	}
	p.entries, p.fetchedAt = found, time.Now()
	p.mu.Unlock()
	if p.OnChange != nil {
		for _, e := range changed {
			p.OnChange(e.person, e.state)
		}
	}
	return ""
}

// stateLocked: the person's last state seen ("" = none).
func (p *Presence) stateLocked(person string) string {
	for _, e := range p.entries {
		if e.person == person {
			return e.state
		}
	}
	return ""
}

// Run fetches every PRESENCE_POLL_MS. Start as a goroutine.
func (p *Presence) Run() {
	for {
		if errText := p.fetchNow(); errText == "" {
			p.failures = 0
		} else if p.failures++; p.failures == 1 || p.failures%30 == 0 {
			log.Printf("[Anwesenheit] %s", errText)
		}
		time.Sleep(time.Duration(prompts.Int("PRESENCE_POLL_MS", 60000)) * time.Millisecond)
	}
}

// Of: where a person of the register is: "home", "away", "" = unknown (no HA person or stale).
func (p *Presence) Of(person string) string {
	p.mu.Lock()
	defer p.mu.Unlock()
	if !p.fresh() {
		return ""
	}
	for _, e := range p.entries {
		if e.person == person {
			if e.state == "home" {
				return "home"
			}
			return "away"
		}
	}
	return ""
}

// Text for the prompts, "" without data.
// Home: how many of the register's people are at home and how many are known; known 0 = unknown
// (no HA person or stale).
func (p *Presence) Home() (home, known int) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if !p.fresh() {
		return 0, 0
	}
	for _, e := range p.entries {
		known++
		if e.state == "home" {
			home++
		}
	}
	return home, known
}

func (p *Presence) Text() string {
	p.mu.Lock()
	defer p.mu.Unlock()
	if !p.fresh() || len(p.entries) == 0 {
		return ""
	}
	var home, away []string
	for _, e := range p.entries {
		name := p.people.Name(e.person)
		since := ""
		if e.since != "" {
			since = "since " + e.since
		}
		if e.state == "home" {
			if since != "" {
				name += " (" + since + ")"
			}
			home = append(home, name)
			continue
		}
		where := ""
		if e.state != "not_home" {
			where = e.state
		}
		detail := where + since
		if where != "" && since != "" {
			detail = where + ", " + since
		}
		if detail != "" {
			name += " (" + detail + ")"
		}
		away = append(away, name)
	}
	text := "Presence according to Home Assistant (phone tracking, as of " + p.fetchedAt.Format("15:04") +
		", updated every minute; get_presence fetches it fresh):"
	if len(home) > 0 {
		text += " at home: " + strings.Join(home, ", ") + ";"
	}
	if len(away) > 0 {
		text += " away: " + strings.Join(away, ", ") + ";"
	}
	return text[:len(text)-1] + ". Anyone not listed is not tracked.\n"
}

// locate: where exactly a person is (GPS of the HA app, address from Google). At home no address
// is looked up.
func (p *Presence) locate(who string) string {
	person := p.people.ByName(who)
	if person == "" {
		return "Error: " + who + " is not a person of the household (list_people)."
	}
	persons, errText := p.states()
	if errText != "" {
		return errText
	}
	name := p.people.Name(person)
	for _, s := range persons {
		if p.match(s) != person {
			continue
		}
		since := localTime(s.LastChanged)
		if s.State == "home" {
			return name + " is at home (since " + since + ")."
		}
		a := s.Attributes
		if a.Latitude == nil || a.Longitude == nil {
			return name + ": Home Assistant shows \"" + s.State + "\" since " + since +
				", but no position (the HA app with location access on their phone sends it)."
		}
		where := "in the zone \"" + s.State + "\""
		if s.State == "not_home" {
			where = "away"
		}
		text := name + ": " + where + " since " + since + "; position of " + localTime(s.LastUpdated)
		if a.GPSAccuracy > 0 {
			text += fmt.Sprintf(" (accurate to %d m)", int(a.GPSAccuracy))
		}
		lat, lon := *a.Latitude, *a.Longitude
		address, errText := "", "no maps"
		if p.maps != nil {
			address, errText = p.maps.Address(lat, lon)
		}
		if address != "" {
			text += ": " + address
		} else {
			text += fmt.Sprintf("; address unknown (%s), coordinates %.5f, %.5f", errText, lat, lon)
		}
		if p.maps != nil {
			if km := p.maps.KmFromHome(lat, lon); km >= 0 {
				text += fmt.Sprintf("; %.1f km from home (straight line)", km)
			}
		}
		return text + ". For the way home use travel_time with this address as origin."
	}
	return "Error: Home Assistant has no person for " + name + " (a person with the HA app on their phone, named " +
		person + " or " + name + ")."
}

// AddTools registers get_presence and locate_person.
func (p *Presence) AddTools(r *tools.Registry) {
	r.Add(tools.Tool{
		Name: "get_presence",
		Description: "Who of the household is at home right now, from Home Assistant's phone tracking (the HA app), with " +
			"since when; fetched fresh. Use it for 'Wer ist zu Hause?', 'Ist Anna schon da?', 'Zeigt Home Assistant " +
			"mich als zu Hause?'. Home Assistant's other tools do not show people or phones.",
		Parameters: `{"type": "object", "additionalProperties": false, "properties": {}, "required": []}`,
		Run: func(map[string]any, tools.Context) string {
			if errText := p.fetchNow(); errText != "" {
				return errText
			}
			if found := p.Text(); found != "" {
				return found
			}
			return "Home Assistant tracks nobody of the household."
		},
	})
	r.Add(tools.Tool{
		Name: "locate_person",
		Description: "Where a person of the household is right now: the address of their phone (Home Assistant app, Google " +
			"Maps), since when, distance from home. For 'Wo ist Anna gerade?'. Only for the admin.",
		Parameters: `{"type": "object", "additionalProperties": false,
            "properties": {"person": {"type": "string", "description": "name of the person"}},
            "required": ["person"]}`,
		Private: true,
		Run:     func(args map[string]any, _ tools.Context) string { return p.locate(tools.String(args, "person")) },
	})
}
