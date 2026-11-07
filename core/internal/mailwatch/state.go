package mailwatch

import (
	"log"
	"strings"
	"time"

	"echo-assistant/internal/mail"
	"echo-assistant/internal/prompts"
	"echo-assistant/internal/store"
	"echo-assistant/internal/textutil"
)

const stateFile = "mailwatch.json"

// state is /mailwatch.json of the M5 (same keys). Log and shipments are oldest first.
type state struct {
	Version   int                         `json:"version"`
	Boxes     map[string]*boxState        `json:"boxes"`
	Log       []*entry                    `json:"log"`
	Shipments []*shipment                 `json:"shipments"`
	Digest    map[string]string           `json:"digest"`
	Rules     []*rule                     `json:"rules"`
	RuleNext  int                         `json:"rule_next,omitempty"`
	Suggested map[string]map[string]int64 `json:"suggested"`
	RuleMoves map[string]map[string]int   `json:"rule_moves"`
}

// boxState is what the check keeps per mailbox ("<label>|<user>").
type boxState struct {
	Validity      uint32   `json:"validity,omitempty"` // IMAP: UIDVALIDITY
	Last          uint32   `json:"last,omitempty"`     // IMAP: highest UID seen
	History       string   `json:"history,omitempty"`  // Gmail: history id
	Failures      int      `json:"failures,omitempty"`
	Address       string   `json:"address,omitempty"` // Gmail: the account's address
	Indexed       bool     `json:"indexed,omitempty"`
	Index         []string `json:"index,omitempty"` // refs still to file
	IndexFailures int      `json:"index_failures,omitempty"`
}

// entry is one sorted mail. Moved/Where/MID/MUID/MovedT/Rule/Trash are set by mail_sort.
type entry struct {
	T        int64  `json:"t"`
	Person   string `json:"person"`
	Box      string `json:"box"`
	Ref      string `json:"ref,omitempty"`
	From     string `json:"from"`
	Subject  string `json:"subject"`
	Category string `json:"category"`
	Notify   string `json:"notify"`
	Summary  string `json:"summary"`
	Deadline string `json:"deadline,omitempty"`
	Told     bool   `json:"told,omitempty"`
	Old      bool   `json:"old,omitempty"`   // filed later (the index): nobody was told
	Moved    string `json:"moved,omitempty"` // folder for people
	Where    string `json:"where,omitempty"` // IMAP folder / Gmail label id, "TRASH", "ARCHIVE"
	MID      string `json:"mid,omitempty"`   // Message-ID
	MUID     uint32 `json:"muid,omitempty"`  // UID there
	MovedT   int64  `json:"moved_t,omitempty"`
	Rule     int    `json:"rule,omitempty"`
	Trash    bool   `json:"trash,omitempty"`
}

type shipment struct {
	Person    string `json:"person"`
	Carrier   string `json:"carrier"`
	Number    string `json:"number"`
	Sender    string `json:"sender,omitempty"`
	Status    string `json:"status"`
	Expected  string `json:"expected,omitempty"`
	Delivered bool   `json:"delivered"`
	T         int64  `json:"t"`
}

type rule struct {
	ID       int    `json:"id"`
	Person   string `json:"person"`
	Box      string `json:"box"`
	Category string `json:"category"`
	From     string `json:"from"`
	Subject  string `json:"subject"`
	Target   string `json:"target"`
	Where    string `json:"where,omitempty"`
	T        int64  `json:"t"`
	Hits     int    `json:"hits"`
	Trash    bool   `json:"trash,omitempty"`
}

func (s *state) normalize() {
	if s.Boxes == nil {
		s.Boxes = map[string]*boxState{}
	}
	if s.Digest == nil {
		s.Digest = map[string]string{}
	}
	if s.Suggested == nil {
		s.Suggested = map[string]map[string]int64{}
	}
	if s.RuleMoves == nil {
		s.RuleMoves = map[string]map[string]int{}
	}
	s.Version = 4
}

// box returns the state of a mailbox, made when missing. Caller holds the lock.
func (s *state) box(key string) *boxState {
	b := s.Boxes[key]
	if b == nil {
		b = &boxState{}
		s.Boxes[key] = b
	}
	return b
}

func (s *Service) load() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.st = state{}
	if !store.LoadJSON(stateFile, &s.st) {
		s.st = state{}
	}
	s.st.normalize()
}

// save writes the state file; caller holds the lock. (The M5 deferred this during conversations
// because flash writes stalled its cores; the Echo's data partition does not have that problem.)
func (s *Service) save() {
	if err := store.SaveJSON(stateFile, &s.st); err != nil {
		log.Printf("[MailWatch] saving failed: %v", err)
	}
}

func lim(name string, def int) int { return prompts.Int(name, def) }

func days(n int) time.Duration { return time.Duration(n) * 24 * time.Hour }

// logged: the mail is in the log already. Caller holds the lock.
func (s *Service) logged(box Mailbox, ref string) bool {
	for _, e := range s.st.Log {
		if e.Ref == ref && e.Box == box.Label {
			return true
		}
	}
	return false
}

// prune drops old log entries and finished parcels. Caller holds the lock.
func (s *Service) prune(now time.Time, own string) {
	own = strings.ToLower(own)
	kept := s.st.Log[:0]
	for _, e := range s.st.Log {
		old := now.Unix()-e.T > int64(lim("MAILWATCH_LOG_DAYS", 60))*86400
		if old || (own != "" && strings.Contains(strings.ToLower(e.From), own)) {
			continue
		}
		kept = append(kept, e)
	}
	s.st.Log = kept
	for len(s.st.Log) > lim("MAILWATCH_LOG_MAX", 800) {
		oldest := 0
		for i, e := range s.st.Log {
			if e.T < s.st.Log[oldest].T {
				oldest = i
			}
		}
		s.st.Log = append(s.st.Log[:oldest], s.st.Log[oldest+1:]...)
	}
	keptShipments := s.st.Shipments[:0]
	for _, p := range s.st.Shipments {
		age := now.Unix() - p.T
		if age > int64(lim("MAILWATCH_SHIPMENT_DAYS", 21))*86400 || (p.Delivered && age > 2*86400) || overdue(p, now) {
			continue
		}
		keptShipments = append(keptShipments, p)
	}
	s.st.Shipments = keptShipments
}

// overdue: expected more than MAILWATCH_SHIPMENT_LATE_DAYS ago and no news since: delivered
// without a mail saying so, or long gone; not "on the way" any more.
func overdue(p *shipment, now time.Time) bool {
	return len(p.Expected) == 10 &&
		p.Expected < now.Add(-days(lim("MAILWATCH_SHIPMENT_LATE_DAYS", 2))).Format("2006-01-02")
}

func shipmentLine(p *shipment) string {
	line := p.Carrier
	if p.Sender != "" {
		line += " von " + p.Sender
	}
	if p.Status != "" {
		line += ": " + p.Status
	}
	if p.Expected != "" {
		line += " (erwartet " + p.Expected + ")"
	}
	return line
}

// Parcel: one of a person's parcels for the initiative (#82): Key changes with its state.
type Parcel struct {
	Key, Line, Expected string
	Delivered           bool
}

// Parcels: the person's parcels worth a thought: not delivered (late ones included until
// MAILWATCH_SHIPMENT_LATE_DAYS), or delivered since yesterday.
func (s *Service) Parcels(person string) []Parcel {
	now := time.Now()
	yesterday := now.Add(-days(1)).Unix()
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []Parcel
	for _, p := range s.st.Shipments {
		if p.Person != person || (p.Delivered && p.T < yesterday) || (!p.Delivered && overdue(p, now)) {
			continue
		}
		id := p.Number
		if id == "" {
			id = p.Carrier + "/" + p.Sender
		}
		out = append(out, Parcel{Key: id + "|" + p.Status + "|" + p.Expected, Line: shipmentLine(p), Expected: p.Expected,
			Delivered: p.Delivered})
	}
	return out
}

// Tracked: one of the parcels with a tracking number, for the tracking with 17TRACK (parceltrack).
type Tracked struct {
	Person, Number, Carrier, Sender string
	Delivered                       bool
}

// TrackedShipments: every parcel with a tracking number the mails named.
func (s *Service) TrackedShipments() []Tracked {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []Tracked
	for _, p := range s.st.Shipments {
		if p.Number != "" {
			out = append(out, Tracked{Person: p.Person, Number: p.Number, Carrier: p.Carrier, Sender: p.Sender,
				Delivered: p.Delivered})
		}
	}
	return out
}

// UpdateShipment: the tracking's newer state of a parcel (every person's entry with the number,
// spaces and dashes ignored). Its time becomes now, so an older mail does not undo it.
func (s *Service) UpdateShipment(number, status string, delivered bool) {
	norm := func(n string) string { return strings.ToUpper(strings.NewReplacer(" ", "", "-", "").Replace(n)) }
	s.mu.Lock()
	defer s.mu.Unlock()
	changed := false
	for _, p := range s.st.Shipments {
		if p.Number != "" && norm(p.Number) == norm(number) {
			p.Status, p.Delivered, p.T = status, delivered, time.Now().Unix()
			changed = true
		}
	}
	if changed {
		s.save()
	}
}

// openShipments: the parcels of `person` not delivered yet, one per line with `prefix`. Caller
// holds the lock.
func (s *Service) openShipments(person, prefix string, now time.Time) string {
	var b strings.Builder
	for _, p := range s.st.Shipments {
		if p.Person == person && !p.Delivered && !overdue(p, now) {
			b.WriteString(prefix + shipmentLine(p) + "\n")
		}
	}
	return b.String()
}

// addShipment: `mailAt` is when the mail was sent. The parcel's time is that of its newest mail,
// not when the assistant read it (filing old mails made parcels of past months look new, 06.10.). An
// older mail does not overwrite what a newer one said. Caller holds the lock.
func (s *Service) addShipment(person string, found sortedShipment, mailAt int64) {
	var p *shipment
	for _, c := range s.st.Shipments {
		if c.Person != person {
			continue
		}
		same := false
		if found.Number != "" {
			same = found.Number == c.Number
		} else {
			same = c.Number == "" && found.Carrier == c.Carrier && found.Sender == c.Sender
		}
		if same {
			p = c
		}
	}
	if p != nil && p.T > mailAt {
		return
	}
	if p == nil {
		p = &shipment{Person: person, Number: found.Number}
		s.st.Shipments = append(s.st.Shipments, p)
	}
	p.Carrier = found.Carrier
	if found.Sender != "" {
		p.Sender = found.Sender
	}
	p.Status = found.Status
	if found.Expected != "" {
		p.Expected = found.Expected
	}
	p.Delivered = found.Delivered
	p.T = mailAt
}

// dropOwn leaves out what the assistant sent itself (replies, recipes, news to the owner).
func dropOwn(mails []mail.Incoming, own string) []mail.Incoming {
	own = strings.ToLower(own)
	if own == "" {
		return mails
	}
	kept := mails[:0]
	for _, m := range mails {
		if !strings.Contains(strings.ToLower(m.From), own) {
			kept = append(kept, m)
		}
	}
	return kept
}

func clip(s string, n int) string { return textutil.Prefix(s, n) }
