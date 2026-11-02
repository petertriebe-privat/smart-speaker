package google

import (
	"context"
	"fmt"
	"log"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"echo-assistant/internal/prompts"
	"echo-assistant/internal/tools"
)

// Contacts (read only) over the People API, and the caller directory: a copy of every contact's
// numbers, so a caller is greeted by name without a network call during the call. As the contacts
// part of devices/atoms3r/src/services/google.cpp. (save_contact and delete_contact of the M5 are
// not ported: contacts are read only on the Echo.)

const peopleHost = "https://people.googleapis.com"

type directoryState struct {
	mu      sync.Mutex
	names   map[string]string // normalized number -> name
	entries int
	dueAt   time.Time // next refresh (zero = now)
	last    time.Time // last attempt
	missed  atomic.Bool
}

// normalizedNumber: 0171 1234567 / +49 171 1234567 / 0049171... -> 01711234567 (as phone/ writes
// the caller).
func normalizedNumber(number string) string {
	var b strings.Builder
	for _, r := range number {
		if r >= '0' && r <= '9' || r == '+' && b.Len() == 0 {
			b.WriteRune(r)
		}
	}
	digits := b.String()
	if strings.HasPrefix(digits, "+") {
		digits = "00" + digits[1:]
	}
	if strings.HasPrefix(digits, "0049") {
		digits = "0" + digits[4:]
	}
	return digits
}

// CallerName: the name of the contact with this phone number ("" if none), for greeting callers.
// Looks it up in the copy of all contacts' numbers (refreshed by Run every GOOGLE_DIRECTORY_MS):
// instant, no network.
func (a *Account) CallerName(number string) string {
	key := normalizedNumber(number)
	if len(key) < 6 {
		return ""
	}
	a.dir.mu.Lock()
	name := a.dir.names[key]
	a.dir.mu.Unlock()
	if name == "" {
		a.dir.missed.Store(true)
	}
	return name
}

// birthday: "05.09.1980" or "05.09." without the year.
func birthday(date any) string {
	day, month, year := digN(date, "day"), digN(date, "month"), digN(date, "year")
	if day == 0 || month == 0 {
		return ""
	}
	if year != 0 {
		return fmt.Sprintf("%02d.%02d.%04d", day, month, year)
	}
	return fmt.Sprintf("%02d.%02d.", day, month)
}

func (a *Account) findContact(args map[string]any, _ tools.Context) string {
	query := tools.String(args, "name")
	if query == "" {
		return "Error: name missing"
	}
	ctx, cancel := callContext()
	defer cancel()
	// searchContacts needs a warm-up request with an empty query to fill its cache.
	warmingUp := !a.warmedUp.Load()
	if warmingUp {
		var ignored map[string]any
		if err := a.get(ctx, peopleHost+"/v1/people:searchContacts?query=&readMask=names", &ignored); err != nil {
			return err.Error()
		}
		a.warmedUp.Store(true)
	}
	path := peopleHost + "/v1/people:searchContacts?pageSize=10&query=" + enc(query) +
		"&readMask=names,nicknames,phoneNumbers,emailAddresses,birthdays&fields=" +
		enc("results(person(resourceName,names/displayName,nicknames/value,"+
			"phoneNumbers(value,type),emailAddresses(value,type),birthdays/date))")
	var parsed map[string]any
	for attempt := 0; attempt < 2; attempt++ {
		parsed = nil
		if err := a.get(ctx, path, &parsed); err != nil {
			return err.Error()
		}
		if len(digL(parsed, "results")) > 0 || !warmingUp {
			break
		}
		time.Sleep(2 * time.Second) // the cache right after the warm-up can still be empty
	}
	results := digL(parsed, "results")
	if len(results) == 0 {
		return "No contact found for \"" + query + "\" (try the first or last name)."
	}
	text := "Contacts for \"" + query + "\":\n"
	for _, result := range results {
		person := dig(result, "person")
		name := digS(person, "names", 0, "displayName")
		if name == "" {
			name = "(no name)"
		}
		text += "- " + name
		if nick := digS(person, "nicknames", 0, "value"); nick != "" {
			text += " (" + nick + ")"
		}
		var details []string
		for _, phone := range digL(person, "phoneNumbers") {
			details = append(details, "phone "+digS(phone, "type")+" "+digS(phone, "value"))
		}
		for _, mail := range digL(person, "emailAddresses") {
			details = append(details, "mail "+digS(mail, "value"))
		}
		if born := birthday(dig(person, "birthdays", 0, "date")); born != "" {
			details = append(details, "birthday "+born)
		}
		if len(details) > 0 {
			text += ": " + strings.Join(details, ", ")
		}
		text += " [id " + digS(person, "resourceName") + "]\n"
	}
	return text
}

// connections calls fn for every page (at most 5) of the user's contacts with personFields.
func (a *Account) connections(ctx context.Context, personFields, fields string, fn func(person any)) error {
	pageToken := ""
	for page := 0; page < 5; page++ {
		path := peopleHost + "/v1/people/me/connections?personFields=" + personFields + "&pageSize=1000&fields=" + enc(fields)
		if pageToken != "" {
			path += "&pageToken=" + enc(pageToken)
		}
		var parsed map[string]any
		if err := a.get(ctx, path, &parsed); err != nil {
			return err
		}
		for _, person := range digL(parsed, "connections") {
			fn(person)
		}
		if pageToken = digS(parsed, "nextPageToken"); pageToken == "" {
			break
		}
	}
	return nil
}

// Birthday: a contact's next birthday.
type Birthday struct {
	Name string
	Next time.Time // midnight of the day
	Away int       // days from today
	Age  int       // the age then, 0 = year unknown
}

// Birthdays: the contacts' birthdays in the next `days` days, soonest first (the initiative, #82).
func (a *Account) Birthdays(days int) ([]Birthday, error) {
	todayStart, _ := parseLocal(today(), "")
	var found []Birthday
	ctx, cancel := callContext()
	defer cancel()
	err := a.connections(ctx, "names,birthdays", "connections(names/displayName,birthdays/date),nextPageToken", func(person any) {
		date := dig(person, "birthdays", 0, "date")
		day, month, year := digN(date, "day"), digN(date, "month"), digN(date, "year")
		name := digS(person, "names", 0, "displayName")
		if day == 0 || month == 0 || name == "" {
			return
		}
		next := todayStart
		// Next birthday from today on; 29.02. is the 01.03. in other years.
		for offset := 0; offset < 2; offset++ {
			next = time.Date(todayStart.Year()+offset, time.Month(month), day, 0, 0, 0, 0, time.Local)
			if !next.Before(todayStart) {
				break
			}
		}
		away := int((next.Sub(todayStart) + 12*time.Hour) / (24 * time.Hour))
		if away > days {
			return
		}
		b := Birthday{Name: name, Next: next, Away: away}
		if year != 0 {
			b.Age = next.Year() - year
		}
		found = append(found, b)
	})
	sort.SliceStable(found, func(i, j int) bool { return found[i].Away < found[j].Away })
	return found, err
}

func (a *Account) upcomingBirthdays(args map[string]any, _ tools.Context) string {
	if !synced() {
		return noClock
	}
	days, ok := tools.Int(args, "days")
	if !ok {
		days = 30
	}
	days = min(max(days, 0), 366)
	found, err := a.Birthdays(days)
	if err != nil {
		return err.Error()
	}
	if len(found) == 0 {
		return fmt.Sprintf("No birthdays in the next %d days (according to the Google contacts).", days)
	}
	text := fmt.Sprintf("Birthdays in the next %d days:\n", days)
	for _, b := range found {
		line := "- "
		switch b.Away {
		case 0:
			line += "today"
		case 1:
			line += "tomorrow"
		default:
			line += dayName(dateStamp(b.Next))[:9]
		}
		line += ": " + b.Name
		if b.Age != 0 {
			line += fmt.Sprintf(" (turns %d)", b.Age)
		}
		text += line + "\n"
	}
	return text
}

// loadDirectory reads every contact's numbers (canonical and as written) into the directory.
func (a *Account) loadDirectory() error {
	ctx, cancel := callContext()
	defer cancel()
	names := map[string]string{}
	entries := 0
	err := a.connections(ctx, "names,phoneNumbers",
		"connections(names/displayName,phoneNumbers(value,canonicalForm)),nextPageToken", func(person any) {
			name := digS(person, "names", 0, "displayName")
			name = strings.NewReplacer("\t", " ", "\n", " ").Replace(name)
			if name == "" {
				return
			}
			for _, phone := range digL(person, "phoneNumbers") {
				canonical, written := normalizedNumber(digS(phone, "canonicalForm")), normalizedNumber(digS(phone, "value"))
				for i, number := range [2]string{canonical, written} {
					if len(number) < 6 || (i == 1 && written == canonical) {
						continue
					}
					if _, taken := names[number]; !taken { // the first entry wins
						names[number] = name
					}
					entries++
				}
			}
		})
	if err != nil {
		return err
	}
	a.dir.mu.Lock()
	a.dir.names, a.dir.entries = names, entries
	a.dir.mu.Unlock()
	return nil
}

// refreshDirectory: background work of Run: the directory at the start and every
// GOOGLE_DIRECTORY_MS, between conversations; after a failure again in 5 minutes.
func (a *Account) refreshDirectory() {
	d := &a.dir
	now := time.Now()
	// An unknown caller: reload after the call (at most every 10 minutes), so a contact made
	// meanwhile is known at the next call.
	if d.missed.Load() && !d.last.IsZero() && now.Sub(d.last) > 10*time.Minute {
		d.dueAt = now
	}
	if now.Before(d.dueAt) || a.busy() || !synced() {
		return
	}
	d.missed.Store(false)
	d.last = now
	if err := a.loadDirectory(); err != nil {
		log.Printf("[Google] address book for callers: %v", err)
		d.dueAt = time.Now().Add(5 * time.Minute)
		return
	}
	a.dir.mu.Lock()
	n := a.dir.entries
	a.dir.mu.Unlock()
	log.Printf("[Google] address book for callers: %d numbers", n)
	d.dueAt = time.Now().Add(time.Duration(prompts.Int("GOOGLE_DIRECTORY_MS", 21600000)) * time.Millisecond)
}

func (a *Account) addContactTools(r *tools.Registry) {
	r.Add(tools.Tool{
		Name: "find_contact",
		Description: "Looks up people in the user's Google contacts by name (first name, last name or nickname): phone numbers, " +
			"e-mail addresses, birthday. Use it before phone_call or send_email to someone the user knows, and for " +
			"questions like 'wann hat Anna Geburtstag'. Several matches: ask which one is meant.",
		Parameters: `{"type": "object", "additionalProperties": false,
            "properties": {"name": {"type": "string"}}, "required": ["name"]}`,
		Private: true, Channels: tools.Everywhere, Run: a.findContact})
	r.Add(tools.Tool{
		Name: "upcoming_birthdays",
		Description: "Lists the birthdays in the user's Google contacts in the next days (with the new age where the year is " +
			"known), e.g. 'wer hat diese Woche Geburtstag', or in a morning overview.",
		Parameters: `{"type": "object", "additionalProperties": false,
            "properties": {"days": {"type": "integer", "description": "how many days ahead, 0 = today only, default 30"}},
            "required": ["days"]}`,
		Private: true, Channels: tools.Everywhere, Run: a.upcomingBirthdays})
	// Only for tests over the console: GPT Live gets the name with the call itself.
	r.Add(tools.Tool{
		Name:        "caller_name",
		Description: "Name of the contact with this phone number (address book copy for callers).",
		Parameters: `{"type": "object", "additionalProperties": false,
            "properties": {"number": {"type": "string"}}, "required": ["number"]}`,
		Channels: tools.Console,
		Run: func(args map[string]any, _ tools.Context) string {
			if name := a.CallerName(tools.String(args, "number")); name != "" {
				return name
			}
			return "unknown"
		}})
}

// Run does the background work until ctx ends: the caller directory (at the start and every
// GOOGLE_DIRECTORY_MS), and the login renewal (a received code is exchanged; when the login
// expired the admin gets a link, again after GOOGLE_LOGIN_REMIND_MS). Start it as a goroutine.
func (a *Account) Run(ctx context.Context) {
	if !a.Enabled() {
		return
	}
	tick := time.NewTicker(5 * time.Second)
	defer tick.Stop()
	for {
		a.refreshDirectory()
		if a.renewable() {
			a.loginWork()
		}
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
		}
	}
}
