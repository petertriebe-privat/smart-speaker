// Package parceltrack: the parcels from the household's mails followed with 17TRACK through Home
// Assistant's seventeentrack integration (#7). The mail watch reads the tracking numbers out of
// shipping mails (any carrier); the tracker adds each new number to the household's 17TRACK
// account (seventeentrack.add_package), reads the packages back (seventeentrack.get_packages)
// every PARCEL_POLL_MS while parcels are open, and tells their person when one changes. HA itself
// polls 17TRACK every 10 minutes and fires no event on a change, so the tracker pulls from HA and
// compares; it never talks to 17TRACK or a carrier itself (no key, no quota of its own).
//
// Whether a change is told is the person's: their behaviour rules ("bei Paketen nur zugestellt")
// decide per status, one small question per person, status and rules; without a rule every change
// is told. A delivered parcel is told, archived in 17TRACK and kept in parcels.json a while so the
// same number is not added again. Run only on the primary device: the M5 does not poll a second
// time.
package parceltrack

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"log"
	"regexp"
	"strings"
	"sync"
	"time"

	"echo-assistant/internal/netx"
	"echo-assistant/internal/prefer"
	"echo-assistant/internal/prompts"
	"echo-assistant/internal/store"
)

const stateFile = "parcels.json"

// Shipment is a parcel the mail watch found: whose, which number, who sent it.
type Shipment struct {
	Person, Number, Carrier, Sender string
	Delivered                       bool
}

// Tracker follows the parcels. Rest and Shipments are needed; the other hooks are optional.
type Tracker struct {
	// Rest: one request to HA's REST API (homeassistant.HA.Rest).
	Rest func(method, path string, payload any) (int, []byte, error)
	// Shipments: the parcels from the mails (mailwatch.Service.Tracked).
	Shipments func() []Shipment
	// Update: 17TRACK's state back into the mail watch, so its parcel list, digest and the
	// initiative say the same (mailwatch.Service.UpdateShipment).
	Update func(number, status string, delivered bool)
	// Rules: the person's behaviour rules as text (memory.Rules).
	Rules func(person string) string
	// Tell: the news to the person, in a running conversation with them or as a message; true
	// when it reached them (it is tried again on the next change otherwise, not repeated).
	Tell func(person, text string) bool
	// Ask: the rules question (prefer.Decide); tests replace it.
	Ask func(input string, choices []string, unit string) (string, error)
	Now func() time.Time

	mu        sync.Mutex
	st        state
	entry     string          // the seventeentrack config entry in HA
	decisions map[string]bool // person|status|rules hash -> tell
	lastPoll  time.Time
	missing   bool // logged once: the integration is not set up
}

type state struct {
	Parcels []*parcel `json:"parcels"`
}

type parcel struct {
	Number   string   `json:"number"`
	Persons  []string `json:"persons"`
	Carrier  string   `json:"carrier,omitempty"`
	Sender   string   `json:"sender,omitempty"`
	Added    bool     `json:"added,omitempty"` // in the 17TRACK account
	Tries    int      `json:"tries,omitempty"` // failed add_package calls
	NextTry  int64    `json:"next_try,omitempty"`
	Seen     bool     `json:"seen,omitempty"` // a first state was read (it is not news)
	Status   string   `json:"status,omitempty"`
	Info     string   `json:"info,omitempty"`
	Location string   `json:"location,omitempty"`
	Stamp    string   `json:"stamp,omitempty"`
	Done     bool     `json:"done,omitempty"` // delivered, given up or too old: not followed any more
	T        int64    `json:"t"`              // when it was taken up
	Changed  int64    `json:"changed,omitempty"`
}

// pkg is one package in get_packages' answer.
type pkg struct {
	TrackingNumber string `json:"tracking_number"`
	Status         string `json:"status"`
	InfoText       string `json:"info_text"`
	Location       string `json:"location"`
	Timestamp      string `json:"timestamp"`
}

func New() *Tracker {
	t := &Tracker{Ask: prefer.Decide, Now: time.Now, decisions: map[string]bool{}}
	if !store.LoadJSON(stateFile, &t.st) {
		t.st = state{}
	}
	return t
}

var numberPattern = regexp.MustCompile(`^[A-Z0-9]{8,40}$`)

// Number: the tracking number as 17TRACK takes it, "" if it is none (it comes from a mail anyone
// can send: only letters and digits go to HA).
func Number(raw string) string {
	n := strings.ToUpper(strings.NewReplacer(" ", "", "-", "").Replace(strings.TrimSpace(raw)))
	if !numberPattern.MatchString(n) {
		return ""
	}
	return n
}

// Slug: the status as get_packages' package_state names it ("In Transit" -> in_transit).
func Slug(status string) string {
	return strings.ReplaceAll(strings.ToLower(strings.TrimSpace(status)), " ", "_")
}

var labels = map[string]string{
	"not_found":             "noch nicht erfasst",
	"in_transit":            "unterwegs",
	"expired":               "Verfolgung abgelaufen",
	"ready_to_be_picked_up": "liegt zur Abholung bereit",
	"undelivered":           "nicht zugestellt",
	"delivered":             "zugestellt",
	"alert":                 "Problem gemeldet",
}

// Label: the status in German for the person.
func Label(slug string) string {
	if l, ok := labels[slug]; ok {
		return l
	}
	return strings.ReplaceAll(slug, "_", " ")
}

func (t *Tracker) save() {
	if err := store.SaveJSON(stateFile, &t.st); err != nil {
		log.Printf("[Parcels] saving failed: %v", err)
	}
}

func (t *Tracker) find(number string) *parcel {
	for _, p := range t.st.Parcels {
		if p.Number == number {
			return p
		}
	}
	return nil
}

func contains(list []string, s string) bool {
	for _, x := range list {
		if x == s {
			return true
		}
	}
	return false
}

// take: the mail watch's parcels with a number, not delivered, not followed yet. Caller holds the
// lock. True if one is open.
func (t *Tracker) take(now time.Time) bool {
	for _, s := range t.Shipments() {
		n := Number(s.Number)
		if n == "" || s.Person == "" {
			continue
		}
		p := t.find(n)
		if p == nil {
			if s.Delivered {
				continue
			}
			p = &parcel{Number: n, Carrier: s.Carrier, Sender: s.Sender, T: now.Unix()}
			t.st.Parcels = append(t.st.Parcels, p)
			log.Printf("[Parcels] following %s (%s from %s)", n, s.Carrier, s.Sender)
		}
		if !contains(p.Persons, s.Person) {
			p.Persons = append(p.Persons, s.Person)
		}
	}
	open := false
	for _, p := range t.st.Parcels {
		open = open || !p.Done
	}
	return open
}

// prune: finished parcels stay PARCEL_KEEP_DAYS (so the number is not followed again), open ones
// that never got anywhere end after PARCEL_MAX_DAYS. Caller holds the lock. Returns the numbers
// to archive in 17TRACK.
func (t *Tracker) prune(now time.Time) []string {
	var archive []string
	kept := t.st.Parcels[:0]
	for _, p := range t.st.Parcels {
		age := now.Unix() - p.T
		if !p.Done && age > int64(prompts.Int("PARCEL_MAX_DAYS", 45))*86400 {
			log.Printf("[Parcels] %s: no end after %d days, not followed any more", p.Number, age/86400)
			p.Done = true
			if p.Added {
				archive = append(archive, p.Number)
			}
		}
		if p.Done && age > int64(prompts.Int("PARCEL_KEEP_DAYS", 60))*86400 {
			continue
		}
		kept = append(kept, p)
	}
	t.st.Parcels = kept
	return archive
}

// Run pulls every PARCEL_POLL_MS (HA's own interval) while parcels are open. Blocks.
func (t *Tracker) Run(ctx context.Context) {
	tick := time.NewTicker(30 * time.Second)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
			if netx.TimeSynced && t.Now().Sub(t.lastPoll) >= time.Duration(prompts.Int("PARCEL_POLL_MS", 600000))*time.Millisecond {
				t.lastPoll = t.Now()
				t.Step()
			}
		}
	}
}

// Step: one pass: take up new numbers, add them to 17TRACK, read the packages, tell the changes.
func (t *Tracker) Step() {
	now := t.Now()
	t.mu.Lock()
	open := t.take(now)
	archive := t.prune(now)
	t.save()
	t.mu.Unlock()
	if !open && len(archive) == 0 {
		return
	}
	entry, err := t.entryID()
	if err != nil {
		if !t.missing {
			log.Printf("[Parcels] %v", err)
			t.missing = true
		}
		return
	}
	t.missing = false
	for _, n := range archive {
		t.archive(entry, n)
	}
	t.addNew(entry, now)
	packages, err := t.packages(entry)
	if err != nil {
		log.Printf("[Parcels] reading the packages: %v", err)
		return
	}
	t.compare(entry, packages, now)
}

func (t *Tracker) call(service string, data map[string]any, response bool) ([]byte, error) {
	path := "/api/services/seventeentrack/" + service
	if response {
		path += "?return_response"
	}
	status, body, err := t.Rest("POST", path, data)
	if err != nil {
		return nil, err
	}
	if status != 200 {
		return nil, fmt.Errorf("%s: HTTP %d %.200s", service, status, body)
	}
	return body, nil
}

// entryID: the seventeentrack config entry (the actions need it), found from its entities.
func (t *Tracker) entryID() (string, error) {
	t.mu.Lock()
	id := t.entry
	t.mu.Unlock()
	if id != "" {
		return id, nil
	}
	status, body, err := t.Rest("POST", "/api/template", map[string]string{
		"template": "{{ integration_entities('seventeentrack') | map('config_entry_id') | reject('none') | first | default('') }}"})
	if err != nil {
		return "", fmt.Errorf("HA not reachable: %v", err)
	}
	id = strings.TrimSpace(string(body))
	if status != 200 || id == "" {
		return "", fmt.Errorf("the 17TRACK integration is not set up in Home Assistant (HTTP %d)", status)
	}
	t.mu.Lock()
	t.entry = id
	t.mu.Unlock()
	return id, nil
}

// addNew: add_package for the numbers not in the account yet; a failure is tried again later
// (an hour, doubled each time), at most PARCEL_ADD_TRIES times.
func (t *Tracker) addNew(entry string, now time.Time) {
	t.mu.Lock()
	var todo []*parcel
	for _, p := range t.st.Parcels {
		if !p.Done && !p.Added && p.NextTry <= now.Unix() {
			todo = append(todo, p)
		}
	}
	t.mu.Unlock()
	for _, p := range todo {
		name := p.Sender
		if name == "" {
			name = p.Carrier
		}
		if name == "" {
			name = p.Number
		}
		_, err := t.call("add_package", map[string]any{"config_entry_id": entry, "package_tracking_number": p.Number,
			"package_friendly_name": clip(name, 40)}, false)
		t.mu.Lock()
		if err == nil || strings.Contains(strings.ToLower(err.Error()), "already") {
			p.Added = true
			log.Printf("[Parcels] %s added to 17TRACK as %q", p.Number, name)
		} else {
			p.Tries++
			p.NextTry = now.Add(time.Hour * time.Duration(1<<min(p.Tries-1, 5))).Unix()
			log.Printf("[Parcels] %s: adding failed (%d): %v", p.Number, p.Tries, err)
			if p.Tries >= prompts.Int("PARCEL_ADD_TRIES", 5) {
				p.Done = true
			}
		}
		t.save()
		t.mu.Unlock()
	}
}

func (t *Tracker) packages(entry string) (map[string]pkg, error) {
	body, err := t.call("get_packages", map[string]any{"config_entry_id": entry}, true)
	if err != nil {
		return nil, err
	}
	var resp struct {
		ServiceResponse struct {
			Packages []pkg `json:"packages"`
		} `json:"service_response"`
	}
	if err := json.Unmarshal(body, &resp); err != nil {
		return nil, fmt.Errorf("answer not readable: %v", err)
	}
	out := map[string]pkg{}
	for _, p := range resp.ServiceResponse.Packages {
		out[Number(p.TrackingNumber)] = p
	}
	return out, nil
}

func (t *Tracker) archive(entry, number string) {
	if _, err := t.call("archive_package", map[string]any{"config_entry_id": entry, "package_tracking_number": number}, false); err != nil {
		log.Printf("[Parcels] %s: archiving failed: %v", number, err)
	}
}

type news struct {
	p      *parcel
	slug   string
	text   string
	people []string
}

// compare: what changed since the last pass; the first state read is no news (the shipping mail
// was). Changes go back into the mail watch and to the people whose rules want them.
func (t *Tracker) compare(entry string, packages map[string]pkg, now time.Time) {
	var changes []news
	t.mu.Lock()
	for _, p := range t.st.Parcels {
		if p.Done || !p.Added {
			continue
		}
		k, ok := packages[p.Number]
		if !ok {
			continue
		}
		slug := Slug(k.Status)
		info := clip(strings.TrimSpace(k.InfoText), 200)
		loc := clip(strings.TrimSpace(k.Location), 80)
		if p.Seen && slug == p.Status && info == p.Info && k.Timestamp == p.Stamp {
			continue
		}
		first := !p.Seen
		p.Seen, p.Status, p.Info, p.Location, p.Stamp, p.Changed = true, slug, info, loc, k.Timestamp, now.Unix()
		log.Printf("[Parcels] %s: %s, %q", p.Number, slug, info)
		changes = append(changes, news{p: p, slug: slug, text: Text(p), people: append([]string(nil), p.Persons...)})
		if first && slug != "delivered" {
			changes[len(changes)-1].people = nil // the shipping mail told them; only the state is kept
		}
	}
	t.save()
	t.mu.Unlock()
	for _, c := range changes {
		delivered := c.slug == "delivered"
		if t.Update != nil {
			t.Update(c.p.Number, statusLine(c.p), delivered)
		}
		for _, person := range c.people {
			if t.Tell != nil && t.wants(person, c.slug) {
				t.Tell(person, c.text)
			}
		}
		if delivered {
			t.archive(entry, c.p.Number)
			t.mu.Lock()
			c.p.Done = true
			t.save()
			t.mu.Unlock()
		}
	}
}

// Report: one pass now, then what 17TRACK says through HA and what the tracker keeps (ctl parcels).
func (t *Tracker) Report() string {
	t.Step()
	entry, err := t.entryID()
	if err != nil {
		return "error: " + err.Error()
	}
	body, err := t.call("get_packages", map[string]any{"config_entry_id": entry}, true)
	if err != nil {
		return "error: " + err.Error()
	}
	t.mu.Lock()
	kept, _ := json.Marshal(t.st.Parcels)
	t.mu.Unlock()
	return "17TRACK: " + string(body) + "\nkept: " + string(kept)
}

// statusLine: the state for the mail watch's parcel list.
func statusLine(p *parcel) string {
	line := Label(p.Status)
	if p.Info != "" {
		line += ": " + p.Info
	}
	return line
}

// Text: the message about a parcel's new state.
func Text(p *parcel) string {
	who := "Paket"
	if p.Sender != "" {
		who += " von " + p.Sender
	}
	if p.Carrier != "" {
		who += " (" + p.Carrier + ")"
	}
	text := "📦 " + who + ": " + Label(p.Status)
	if p.Info != "" && !strings.EqualFold(p.Info, Label(p.Status)) {
		text += " – " + p.Info
	}
	if p.Location != "" {
		text += " (" + p.Location + ")"
	}
	return text
}

// wants: whether the person's rules want a change to this status told. No rules or no rule about
// it: yes. One question per person, status and rules; an error says yes and asks again next time.
func (t *Tracker) wants(person, slug string) bool {
	if t.Rules == nil || t.Ask == nil {
		return true
	}
	rules := t.Rules(person)
	if rules == "" {
		return true
	}
	sum := sha256.Sum256([]byte(rules))
	key := fmt.Sprintf("%s|%s|%x", person, slug, sum[:6])
	t.mu.Lock()
	tell, known := t.decisions[key]
	t.mu.Unlock()
	if known {
		return tell
	}
	answer, err := t.Ask(Question(rules, slug), []string{"default", "tell", "skip"}, "tell or skip")
	if err != nil {
		log.Printf("[Parcels] rules question for %q: %v", person, err)
		return true
	}
	tell = answer != "skip"
	if !tell {
		log.Printf("[Parcels] %q's rules: no message when a parcel is %s", person, slug)
	}
	t.mu.Lock()
	t.decisions[key] = tell
	t.mu.Unlock()
	return tell
}

// Question: what the rules question reads.
func Question(rules, slug string) string {
	return fmt.Sprintf("%s\n\n# Question\n%s follows the person's parcels and can send them a message whenever a "+
		"parcel's tracking state changes. The new state is %q (%s). Does one of the rules above say whether the person "+
		"wants such a message for this state (e.g. only when delivered, not every step, no parcel messages at all)? Then "+
		"answer tell or skip. A rule about something else does not count; otherwise answer default.", rules, prompts.Name(),
		slug, Label(slug))
}

func clip(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n])
}
