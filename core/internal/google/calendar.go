package google

import (
	"fmt"
	"log"
	"sort"
	"strings"
	"sync"
	"time"

	"echo-assistant/internal/prompts"
	"echo-assistant/internal/textutil"
	"echo-assistant/internal/tools"
)

// Calendars: all the user shows in Google Calendar (reading), writing only in the main calendar
// and the other own ones. Only events in the user's own calendars marked busy take their time;
// holidays and subscribed calendars are information. As the calendar part of
// devices/atoms3r/src/services/google.cpp.

const (
	calendarHost  = "https://www.googleapis.com"
	maxEvents     = 50 // per calendar and call
	calendarsKept = 10 * time.Minute
	maxFreeSlots  = 20
	badDate       = "Error: date/end_date as YYYY-MM-DD (until 2037), time as HH:MM or \"\" for all day"
)

type calendar struct {
	ID, Label string
	Primary   bool
	Own       bool // owner or writer: its busy events block the user's time
}

type calendarState struct {
	mu    sync.Mutex
	cache []calendar
	at    time.Time
}

func eventsPath(calendarID string) string {
	return calendarHost + "/calendar/v3/calendars/" + enc(calendarID) + "/events"
}

// calendars: the calendars shown in Google Calendar (selected, not hidden), the main one first.
// Subscribed iCal calendars are often named by their URL: then "subscription <host>".
func (a *Account) calendars() ([]calendar, error) {
	a.cal.mu.Lock()
	if len(a.cal.cache) > 0 && time.Since(a.cal.at) < calendarsKept {
		out := append([]calendar(nil), a.cal.cache...)
		a.cal.mu.Unlock()
		return out, nil
	}
	a.cal.mu.Unlock()
	ctx, cancel := callContext()
	defer cancel()
	var answer map[string]any
	if err := a.get(ctx, calendarHost+"/calendar/v3/users/me/calendarList?fields="+
		enc("items(id,summary,summaryOverride,primary,accessRole,selected,hidden)"), &answer); err != nil {
		return nil, err
	}
	var out []calendar
	for _, item := range digL(answer, "items") {
		primary := digB(item, "primary")
		if !primary && (!digB(item, "selected") || digB(item, "hidden")) {
			continue
		}
		c := calendar{ID: digS(item, "id"), Primary: primary}
		role := digS(item, "accessRole")
		c.Own = role == "owner" || role == "writer"
		c.Label = digS(item, "summaryOverride")
		if c.Label == "" {
			c.Label = digS(item, "summary")
		}
		if primary {
			c.Label = "main"
		} else if strings.HasPrefix(c.Label, "http") {
			start := strings.Index(c.Label, "//") + 2
			end := strings.IndexByte(c.Label[start:], '/')
			if end < 0 {
				end = len(c.Label) - start
			}
			c.Label = "subscription " + c.Label[start:start+end]
		}
		if primary {
			out = append([]calendar{c}, out...)
		} else {
			out = append(out, c)
		}
	}
	a.cal.mu.Lock()
	a.cal.cache, a.cal.at = append([]calendar(nil), out...), time.Now()
	a.cal.mu.Unlock()
	return out, nil
}

func mainCalendar() calendar { return calendar{ID: "primary", Label: "main", Primary: true, Own: true} }

// calendarByID: the calendar with this id (from the list), the main one if unknown.
func (a *Account) calendarByID(id string) calendar {
	if id != "primary" {
		if all, err := a.calendars(); err == nil {
			for _, c := range all {
				if c.ID == id {
					return c
				}
			}
		}
	}
	return mainCalendar()
}

func declined(event map[string]any) bool {
	for _, attendee := range digL(event, "attendees") {
		if digB(attendee, "self") && digS(attendee, "responseStatus") == "declined" {
			return true
		}
	}
	return false
}

// blocking: whether an event takes the user's time: busy (not marked free), in one of their own
// calendars, not declined. Holidays, school and other subscribed calendars are information only.
func blocking(event map[string]any, c calendar) bool {
	transparency := digS(event, "transparency")
	return c.Own && transparency != "transparent" && !declined(event)
}

// splitEventID: "<calendar id>/<event id>" for events outside the main calendar, else the event
// id alone.
func splitEventID(id string) (calendarID, eventID string) {
	if slash := strings.LastIndexByte(id, '/'); slash > 0 {
		return id[:slash], id[slash+1:]
	}
	return "primary", id
}

func eventPath(id string) string {
	calendarID, eventID := splitEventID(id)
	return eventsPath(calendarID) + "/" + enc(eventID)
}

// --- Event texts ---------------------------------------------------------------------------------

// describeLine: one line per event. Times of dateTime are local: the calls ask for
// CALENDAR_TIMEZONE.
func describeLine(event map[string]any) string {
	line := "- "
	if firstDay := digS(event, "start", "date"); firstDay != "" {
		line += dayName(firstDay)
		if end, ok := parseLocal(digS(event, "end", "date"), ""); ok {
			if last := dateStamp(addDays(end, -1)); last != firstDay {
				line += " to " + dayName(last)
			}
		}
		line += " all day"
	} else {
		begin, end := digS(event, "start", "dateTime"), digS(event, "end", "dateTime")
		line += dayName(sub(begin, 0, 10)) + " " + sub(begin, 11, 16)
		if len(end) >= 16 {
			line += "–"
			if sub(end, 0, 10) != sub(begin, 0, 10) {
				line += dayName(sub(end, 0, 10)) + " "
			}
			line += sub(end, 11, 16)
		}
	}
	summary := digS(event, "summary")
	if summary == "" {
		summary = "(no title)"
	}
	line += ": " + summary
	if location := digS(event, "location"); location != "" {
		line += " | place: " + location
	}
	if description := digS(event, "description"); description != "" {
		description = strings.ReplaceAll(description, "\r", "")
		description = strings.ReplaceAll(description, "\n", " ")
		line += " | note: " + textutil.Prefix(description, 150)
	}
	if digS(event, "recurringEventId") != "" {
		line += " | recurring"
	}
	return line
}

// describe: the event's line with its calendar (outside the main one), "info" when it does not
// block the user's time, and its id: "<calendar id>/<event id>" outside the main calendar, none
// for read-only calendars (nothing to change there).
func describe(event map[string]any, c calendar) string {
	line := describeLine(event)
	if !c.Primary {
		line += " | calendar: " + c.Label
	}
	if !blocking(event, c) {
		line += " | info"
	}
	if declined(event) {
		line += " | declined"
	}
	if c.Own {
		prefix := ""
		if !c.Primary {
			prefix = c.ID + "/"
		}
		line += " [id " + prefix + digS(event, "id") + "]"
	}
	return line + "\n"
}

// startKey: local start of an event ("YYYY-MM-DDTHH:MM", all day "T00:00"): sorts the lines.
func startKey(event map[string]any) string {
	if day := digS(event, "start", "date"); day != "" {
		return day + "T00:00"
	}
	return sub(digS(event, "start", "dateTime"), 0, 16)
}

// setTimes sets start and end of event: at clock for minutes, or all day (clock "") until lastDay
// inclusive ("" = the same day). Returns an error text or "".
func setTimes(event map[string]any, date, clock string, minutes int, lastDay string) string {
	start, ok := parseLocal(date, clock)
	if !ok {
		return badDate
	}
	delete(event, "start")
	delete(event, "end")
	if clock == "" {
		last := start
		if lastDay != "" {
			var ok bool
			if last, ok = parseLocal(lastDay, ""); !ok || last.Before(start) {
				return badDate
			}
		}
		event["start"] = map[string]any{"date": dateStamp(start)}
		event["end"] = map[string]any{"date": dateStamp(addDays(last, 1))} // exclusive
		return ""
	}
	if minutes <= 0 || minutes > 14*24*60 {
		return "Error: duration_minutes between 1 and 20160"
	}
	zone := prompts.Text("CALENDAR_TIMEZONE")
	event["start"] = map[string]any{"dateTime": localStamp(start), "timeZone": zone}
	event["end"] = map[string]any{"dateTime": localStamp(start.Add(time.Duration(minutes) * time.Minute)), "timeZone": zone}
	return ""
}

// --- Reading -------------------------------------------------------------------------------------

// forEachEvent calls found for every event of all shown calendars from start to end (exclusive),
// optionally only those matching query. Every calendar is one request. more: some calendar has
// more than maxEvents. An error only if the main calendar failed.
func (a *Account) forEachEvent(start, end time.Time, query string, found func(event map[string]any, c calendar)) (more bool, err error) {
	all, err := a.calendars()
	if err != nil {
		return false, err
	}
	for _, c := range all {
		path := fmt.Sprintf("%s?singleEvents=true&orderBy=startTime&maxResults=%d&timeZone=%s&timeMin=%s&timeMax=%s&fields=%s",
			eventsPath(c.ID), maxEvents, enc(prompts.Text("CALENDAR_TIMEZONE")), enc(utcStamp(start)), enc(utcStamp(end)),
			enc("items(id,summary,location,description,start,end,recurringEventId,transparency,"+
				"attendees(self,responseStatus)),nextPageToken"))
		if query != "" {
			path += "&q=" + enc(query)
		}
		ctx, cancel := callContext()
		var answer map[string]any
		err := a.get(ctx, path, &answer)
		cancel()
		if err != nil {
			if c.Primary {
				return false, err
			}
			log.Printf("[Google] calendar %s: %v", c.Label, err)
			continue // a broken subscription must not hide the rest
		}
		for _, event := range digL(answer, "items") {
			if m, ok := event.(map[string]any); ok {
				found(m, c)
			}
		}
		if digS(answer, "nextPageToken") != "" {
			more = true
		}
	}
	return more, nil
}

// Event is an event for the agenda.
type Event struct {
	Start, End time.Time // all day: midnight of the first day / of the day after the last
	AllDay     bool
	Info       bool   // does not take the user's time (holidays, subscriptions, marked free)
	ID         string // "<calendar id>/<event id>", stable across requests
	Title      string
	Line       string // as list_events shows it ("Mo 05.10.2026 09:00–10:00: ... | place: ...")
}

// Events: the events of all calendars shown in Google Calendar from from until to (exclusive),
// declined ones left out, sorted by start. Blocks for seconds (one request per calendar).
func (a *Account) Events(from, to time.Time) ([]Event, error) {
	type keyed struct {
		key   string
		event Event
	}
	var found []keyed
	_, err := a.forEachEvent(from, to, "", func(event map[string]any, c calendar) {
		if declined(event) {
			return
		}
		var e Event
		if day := digS(event, "start", "date"); day != "" {
			e.AllDay = true
			e.Start, _ = parseLocal(day, "")
			var ok bool
			if e.End, ok = parseLocal(digS(event, "end", "date"), ""); !ok {
				e.End = addDays(e.Start, 1)
			}
		} else {
			begin, end := digS(event, "start", "dateTime"), digS(event, "end", "dateTime")
			var ok bool
			if e.Start, ok = parseLocal(sub(begin, 0, 10), sub(begin, 11, 16)); !ok {
				return
			}
			if e.End, ok = parseLocal(sub(end, 0, 10), sub(end, 11, 16)); !ok {
				e.End = e.Start
			}
		}
		e.Info = !blocking(event, c)
		e.ID = c.ID + "/" + digS(event, "id")
		e.Title = digS(event, "summary")
		if e.Title == "" {
			e.Title = "(no title)"
		}
		e.Line = describeLine(event)[2:] // without "- "
		if !c.Primary {
			e.Line += " | calendar: " + c.Label
		}
		if e.Info {
			e.Line += " | info"
		}
		found = append(found, keyed{startKey(event), e})
	})
	if err != nil {
		return nil, err
	}
	sort.SliceStable(found, func(i, j int) bool { return found[i].key < found[j].key })
	out := make([]Event, len(found))
	for i, f := range found {
		out[i] = f.event
	}
	return out, nil
}

// matchesWords: every word of query is part of the event's title, notes or place (any case).
func matchesWords(event map[string]any, query string) bool {
	text := strings.ToLower(digS(event, "summary") + " " + digS(event, "description") + " " + digS(event, "location"))
	words := strings.Fields(strings.ToLower(query))
	for _, w := range words {
		if !strings.Contains(text, w) {
			return false
		}
	}
	return len(words) > 0
}

// --- Tools ---------------------------------------------------------------------------------------

func (a *Account) listEvents(args map[string]any, _ tools.Context) string {
	if !synced() {
		return noClock
	}
	from, to := tools.String(args, "from"), tools.String(args, "to")
	if from == "" {
		from = today()
	}
	if to == "" {
		to = from
	}
	start, ok1 := parseLocal(from, "")
	last, ok2 := parseLocal(to, "")
	if !ok1 || !ok2 || last.Before(start) {
		return "Error: from and to as YYYY-MM-DD (until 2037), to not before from"
	}
	if last.Sub(start) > 400*24*time.Hour {
		return "Error: at most a good year at once"
	}
	query := tools.String(args, "query")
	type line struct{ key, text string }
	var lines []line
	more, err := a.forEachEvent(start, addDays(last, 1), query, func(event map[string]any, c calendar) {
		lines = append(lines, line{startKey(event), describe(event, c)})
	})
	if err == nil && len(lines) == 0 && query != "" {
		// Google matches whole words only: "Elternbeirat" missed "1. Elternbeiratssitzung"
		// (30.10.2026). German compounds need parts of words: the range again, filtered here.
		more, err = a.forEachEvent(start, addDays(last, 1), "", func(event map[string]any, c calendar) {
			if matchesWords(event, query) {
				lines = append(lines, line{startKey(event), describe(event, c)})
			}
		})
	}
	if err != nil {
		return err.Error()
	}
	rangeText := dayName(from)
	if to != from {
		rangeText += " to " + dayName(to)
	}
	if query != "" {
		rangeText += " matching \"" + query + "\""
	}
	if len(lines) == 0 {
		return "No events " + rangeText + "."
	}
	sort.SliceStable(lines, func(i, j int) bool { return lines[i].key < lines[j].key })
	text := "Events " + rangeText + " (\"info\": does not take the user's time, e.g. holidays or a subscribed calendar):\n"
	for _, l := range lines {
		text += l.text
	}
	if more {
		text += fmt.Sprintf("(Some calendar has more than %d events: narrow the period.)", maxEvents)
	}
	return text
}

func (a *Account) createEvent(args map[string]any, _ tools.Context) string {
	a.refreshAgenda() // the agenda of the next conversation shows the change
	if !synced() {
		return noClock
	}
	title := tools.String(args, "title")
	if title == "" {
		return "Error: title missing"
	}
	event := map[string]any{"summary": title}
	if s, _ := argString(args, "location"); s != "" {
		event["location"] = s
	}
	if s, _ := argString(args, "description"); s != "" {
		event["description"] = s
	}
	minutes, ok := tools.Int(args, "duration_minutes")
	if !ok {
		minutes = 60
	}
	date, _ := argString(args, "date")
	clock, _ := argString(args, "time")
	endDate, _ := argString(args, "end_date")
	if invalid := setTimes(event, date, clock, minutes, endDate); invalid != "" {
		return invalid
	}
	ctx, cancel := callContext()
	defer cancel()
	var created map[string]any
	if err := a.JSON(ctx, "POST", eventsPath("primary"), event, &created); err != nil {
		return err.Error()
	}
	event["id"] = digS(created, "id")
	return "Created:\n" + describe(event, mainCalendar())
}

// fetchEvent: the event with local times; an error text if there is none.
func (a *Account) fetchEvent(id string) (map[string]any, error) {
	if id == "" {
		return nil, fmt.Errorf("Error: id missing (from list_events)")
	}
	ctx, cancel := callContext()
	defer cancel()
	var event map[string]any
	err := a.get(ctx, eventPath(id)+"?timeZone="+enc(prompts.Text("CALENDAR_TIMEZONE")), &event)
	if status := StatusOf(err); status == 404 || status == 410 {
		return nil, fmt.Errorf("Error: no event with this id (call list_events first)")
	}
	if err != nil {
		return nil, err
	}
	if digS(event, "status") == "cancelled" {
		return nil, fmt.Errorf("Error: this event is already deleted")
	}
	return event, nil
}

func (a *Account) updateEvent(args map[string]any, _ tools.Context) string {
	a.refreshAgenda() // the agenda of the next conversation shows the change
	if !synced() {
		return noClock
	}
	id, _ := argString(args, "id")
	event, err := a.fetchEvent(id)
	if err != nil {
		return err.Error()
	}
	// The backend fills the parameters it does not change with "" and 0 (strict schemas): those
	// mean unchanged. On 24.10.2026 changing only the place sent title "" and blanked the title.
	changed := false
	for _, key := range []string{"title", "location", "description"} {
		value, _ := argString(args, key)
		if strings.TrimSpace(value) == "" {
			continue
		}
		if key == "title" {
			key = "summary"
		}
		event[key] = value
		changed = true
	}
	date, _ := argString(args, "date")
	clock, _ := argString(args, "time")
	lastDay, _ := argString(args, "end_date")
	duration, _ := tools.Int(args, "duration_minutes")
	hasDate, hasTime, hasEnd, hasDuration := date != "", clock != "", lastDay != "", duration > 0
	if hasDate || hasTime || hasEnd || hasDuration {
		// Unchanged parts come from the event: its day, time, length (minutes or days).
		allDay := digS(event, "start", "date") != ""
		begin, end := digS(event, "start", "dateTime"), digS(event, "end", "dateTime")
		if allDay {
			begin, end = digS(event, "start", "date"), digS(event, "end", "date")
		}
		oldDate, oldClock := sub(begin, 0, 10), ""
		if !allDay {
			oldClock = sub(begin, 11, 16)
		}
		endClock := ""
		if !allDay {
			endClock = sub(end, 11, 16)
		}
		oldStart, _ := parseLocal(oldDate, oldClock)
		oldEnd, _ := parseLocal(sub(end, 0, 10), endClock)
		oldMinutes := 60
		if !allDay {
			oldMinutes = max(1, int(oldEnd.Sub(oldStart)/time.Minute))
		}
		if !hasDate {
			date = oldDate
		}
		switch clock {
		case "":
			clock = oldClock
		case "all_day":
			clock = ""
		}
		if lastDay == "" && clock == "" && allDay {
			if start, ok := parseLocal(date, ""); ok {
				days := max(1, int((oldEnd.Sub(oldStart)+12*time.Hour)/(24*time.Hour))) // DST: +- 1 h
				lastDay = dateStamp(addDays(start, days-1))
			}
		}
		minutes := oldMinutes
		if hasDuration {
			minutes = duration
		}
		if invalid := setTimes(event, date, clock, minutes, lastDay); invalid != "" {
			return invalid
		}
		changed = true
	}
	if !changed {
		return "Error: nothing to change given"
	}
	ctx, cancel := callContext()
	defer cancel()
	if err := a.JSON(ctx, "PUT", eventPath(id), event, nil); err != nil {
		return err.Error()
	}
	calendarID, _ := splitEventID(id)
	return "Changed:\n" + describe(event, a.calendarByID(calendarID))
}

func (a *Account) deleteEvent(args map[string]any, _ tools.Context) string {
	a.refreshAgenda() // the agenda of the next conversation shows the change
	id, _ := argString(args, "id")
	event, err := a.fetchEvent(id)
	if err != nil {
		return err.Error()
	}
	ctx, cancel := callContext()
	defer cancel()
	if err := a.JSON(ctx, "DELETE", eventPath(id), nil, nil); err != nil {
		return err.Error()
	}
	calendarID, _ := splitEventID(id)
	return "Deleted:\n" + describe(event, a.calendarByID(calendarID))
}

type interval struct{ start, end time.Time }

// findFreeTime: gaps of at least duration_minutes between earliest and latest each day, counting
// only the events that block the user's time (blocking).
func (a *Account) findFreeTime(args map[string]any, _ tools.Context) string {
	if !synced() {
		return noClock
	}
	from, to := tools.String(args, "from"), tools.String(args, "to")
	if from == "" {
		from = today()
	}
	if to == "" {
		to = from
	}
	first, ok1 := parseLocal(from, "")
	last, ok2 := parseLocal(to, "")
	if !ok1 || !ok2 || last.Before(first) {
		return "Error: from and to as YYYY-MM-DD (until 2037), to not before from"
	}
	if last.Sub(first) > 31*24*time.Hour {
		return "Error: at most a month at once"
	}
	minutes, ok := tools.Int(args, "duration_minutes")
	if !ok {
		minutes = 60
	}
	if minutes < 5 || minutes > 24*60 {
		return "Error: duration_minutes between 5 and 1440"
	}
	opens, closes := tools.String(args, "earliest"), tools.String(args, "latest")
	if opens == "" {
		opens = "08:00"
	}
	if closes == "" {
		closes = "20:00"
	}
	probeOpen, ok1 := parseLocal(from, opens)
	probeClose, ok2 := parseLocal(from, closes)
	if !ok1 || !ok2 || !probeClose.After(probeOpen) {
		return "Error: earliest and latest as HH:MM, earliest before latest"
	}

	all, err := a.calendars()
	if err != nil {
		return err.Error()
	}
	var busy []interval
	for _, c := range all {
		if !c.Own {
			continue // subscribed calendars never block
		}
		path := eventsPath(c.ID) + "?singleEvents=true&maxResults=250&timeZone=" + enc(prompts.Text("CALENDAR_TIMEZONE")) +
			"&timeMin=" + enc(utcStamp(first)) + "&timeMax=" + enc(utcStamp(addDays(last, 1))) +
			"&fields=" + enc("items(start,end,transparency,attendees(self,responseStatus))")
		ctx, cancel := callContext()
		var answer map[string]any
		err := a.get(ctx, path, &answer)
		cancel()
		if err != nil {
			return err.Error()
		}
		for _, item := range digL(answer, "items") {
			event, _ := item.(map[string]any)
			if event == nil || !blocking(event, c) {
				continue
			}
			var start, end time.Time
			if day := digS(event, "start", "date"); day != "" { // all day, marked busy: the whole days
				start, _ = parseLocal(day, "")
				end, _ = parseLocal(digS(event, "end", "date"), "")
			} else {
				begins, ends := digS(event, "start", "dateTime"), digS(event, "end", "dateTime")
				start, _ = parseLocal(sub(begins, 0, 10), sub(begins, 11, 16))
				end, _ = parseLocal(sub(ends, 0, 10), sub(ends, 11, 16))
			}
			if !start.IsZero() && end.After(start) {
				busy = append(busy, interval{start, end})
			}
		}
	}
	sort.Slice(busy, func(i, j int) bool {
		if !busy[i].start.Equal(busy[j].start) {
			return busy[i].start.Before(busy[j].start)
		}
		return busy[i].end.Before(busy[j].end)
	})
	text, slots := freeSlots(busy, first, last, opens, closes, time.Duration(minutes)*time.Minute, time.Now())
	frame := fmt.Sprintf("at least %d min between %s and %s", minutes, opens, closes)
	if text == "" {
		return "No free time " + frame + " from " + dayName(from) + " to " + dayName(to) + "."
	}
	more := ""
	if slots >= maxFreeSlots {
		more = "(more: narrow the period)\n"
	}
	return "Free time, " + frame + " (only events in the user's own calendars that are marked busy count; " +
		"holidays and subscribed calendars do not):\n" + text + more
}

// freeSlots lists per day the gaps of at least need between opens and closes that busy (sorted)
// leaves, from the next quarter hour after now on; at most maxFreeSlots in all.
func freeSlots(busy []interval, first, last time.Time, opens, closes string, need time.Duration, now time.Time) (string, int) {
	var text strings.Builder
	slots := 0
	for day := first; !day.After(last) && slots < maxFreeSlots; day = addDays(day, 1) {
		date := dateStamp(day)
		cursor, _ := parseLocal(date, opens)
		closeAt, _ := parseLocal(date, closes)
		if cursor.Before(now) {
			cursor = time.Unix((now.Unix()+899)/900*900, 0) // from the next quarter hour on
		}
		free := ""
		add := func(from, to time.Time) {
			if free != "" {
				free += ", "
			}
			free += from.Format("15:04") + "–" + to.Format("15:04")
			slots++
		}
		for _, b := range busy {
			if !b.end.After(cursor) {
				continue
			}
			if !b.start.Before(closeAt) {
				break
			}
			if b.start.Sub(cursor) >= need && slots < maxFreeSlots {
				add(cursor, b.start)
			}
			if b.end.After(cursor) {
				cursor = b.end
			}
		}
		if closeAt.Sub(cursor) >= need && slots < maxFreeSlots {
			add(cursor, closeAt)
		}
		if free != "" {
			text.WriteString("- " + dayName(date) + ": " + free + "\n")
		}
	}
	return text.String(), slots
}

// AddTools registers the Google tools in the M5's order: google_login (when the login can be
// renewed by a link), list_events, create_event, update_event, delete_event, find_free_time,
// find_contact, upcoming_birthdays and the console test tool caller_name. Does nothing without a
// Google account. Hooks must be set before. See also AddTodoTools and AddSheetTools.
func (a *Account) AddTools(r *tools.Registry) {
	if !a.Enabled() {
		return
	}
	a.addLoginTool(r)
	// All slow on the M5 (TLS to Google): here every call has its own goroutine.
	r.Add(tools.Tool{
		Name: "list_events",
		Description: "Lists the events of all calendars the user shows in Google Calendar from one day to another (both " +
			"inclusive), optionally only those matching query (words in title, notes, location). For 'what's on " +
			"tomorrow', 'when is the dentist', a daily overview. To find one event without a date search from today a " +
			"year ahead. One line per event with its calendar (outside the main one); lines marked 'info' (holidays, " +
			"subscribed calendars, events marked free) are information, not the user's appointments. Events the user " +
			"can change carry an id (for update_event and delete_event).",
		Parameters: `{"type": "object", "additionalProperties": false,
            "properties": {"from": {"type": "string", "description": "YYYY-MM-DD, '' = today"},
                           "to": {"type": "string", "description": "YYYY-MM-DD, '' = same day as from"},
                           "query": {"type": "string", "description": "search words, '' = all events"}},
            "required": ["from", "to"]}`,
		Private: true, Channels: tools.Everywhere, Run: a.listEvents})
	r.Add(tools.Tool{
		Name: "create_event",
		Description: "Adds an event to the user's Google Calendar, after you read out title, day and time and the user " +
			"confirmed them. Without time it is an all-day event (end_date for several days). Calendar reminders " +
			"come from Google; for a spoken reminder by the assistant use create_task as well if the user wants one.",
		Parameters: `{"type": "object", "additionalProperties": false,
            "properties": {"title": {"type": "string", "description": "short German title, e.g. 'Zahnarzt Dr. Müller'"},
                           "date": {"type": "string", "description": "YYYY-MM-DD"},
                           "time": {"type": "string", "description": "start HH:MM (24 h), '' = all day"},
                           "duration_minutes": {"type": "integer", "description": "default 60"},
                           "end_date": {"type": "string", "description": "all-day events: last day YYYY-MM-DD"},
                           "location": {"type": "string"},
                           "description": {"type": "string", "description": "notes"}},
            "required": ["title", "date", "time"]}`,
		Private: true, Channels: tools.Everywhere, Run: a.createEvent})
	r.Add(tools.Tool{
		Name: "update_event",
		Description: "Changes an event in the user's Google Calendar (id from list_events), after you read out the change and " +
			"the user confirmed it. Give only what changes, '' or 0 for the rest (unchanged); a new date or time keeps " +
			"the length. The id of a recurring event's occurrence changes only that occurrence.",
		Parameters: `{"type": "object", "additionalProperties": false,
            "properties": {"id": {"type": "string"},
                           "title": {"type": "string"},
                           "date": {"type": "string", "description": "YYYY-MM-DD"},
                           "time": {"type": "string", "description": "start HH:MM (24 h), 'all_day' = make it an all-day event"},
                           "duration_minutes": {"type": "integer"},
                           "end_date": {"type": "string", "description": "all-day events: last day YYYY-MM-DD"},
                           "location": {"type": "string"},
                           "description": {"type": "string", "description": "notes (replaces the old ones)"}},
            "required": ["id"]}`,
		Private: true, Channels: tools.Everywhere, Run: a.updateEvent})
	r.Add(tools.Tool{
		Name: "delete_event",
		Description: "Deletes an event from the user's Google Calendar (id from list_events), after you read it out and the " +
			"user confirmed. For a recurring event only this occurrence.",
		Parameters: `{"type": "object", "additionalProperties": false,
            "properties": {"id": {"type": "string"}}, "required": ["id"]}`,
		Private: true, Channels: tools.Everywhere, Run: a.deleteEvent})
	r.Add(tools.Tool{
		Name: "find_free_time",
		Description: "Finds free time in the user's calendars: gaps of at least duration_minutes between earliest and latest " +
			"each day, from one day to another. For 'wann habe ich nächste Woche zwei Stunden Zeit', 'passt ein " +
			"Termin am Donnerstag'. Only events that take the user's time count, not holidays, subscribed calendars " +
			"or events marked free.",
		Parameters: `{"type": "object", "additionalProperties": false,
            "properties": {"from": {"type": "string", "description": "YYYY-MM-DD, '' = today"},
                           "to": {"type": "string", "description": "YYYY-MM-DD, '' = same day as from"},
                           "duration_minutes": {"type": "integer", "description": "default 60"},
                           "earliest": {"type": "string", "description": "HH:MM, '' = 08:00"},
                           "latest": {"type": "string", "description": "HH:MM, '' = 20:00"}},
            "required": ["from", "to", "duration_minutes"]}`,
		Private: true, Channels: tools.Everywhere, Run: a.findFreeTime})
	a.addContactTools(r)
}
