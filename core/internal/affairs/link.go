package affairs

import (
	"log"
	"strings"

	"echo-assistant/internal/textutil"
)

// Observe links a calendar event or to-do the agenda fetched (src kind calendar or task) to an
// affair: the one of its source, one with a similar title on the same day (the booking from a
// mail, also a settled one: a cancelled trip hides its calendar event), or a new one. Returns its
// id and state, so the agenda names the event by the id and leaves out a settled one. No model
// call: run on every refresh.
func (b *Book) Observe(person string, src Source, kind, title, due string) (string, string) {
	if person == "" || src.Ref == "" {
		return "", ""
	}
	due = normDue(due)
	b.mu.Lock()
	defer b.mu.Unlock()
	if a := b.bySource(person, src); a != nil {
		return a.ID, a.State
	}
	if due != "" {
		for _, a := range b.st.Affairs {
			if a.Person == person && dueDay(a) == due[:10] && similar(a.Title, title) {
				addSource(a, src)
				b.log(a, src.Kind, "linked to "+src.Kind, title)
				b.save()
				log.Printf("[Affairs] %s linked to %s: %s", a.ID, src.Kind, title)
				return a.ID, a.State
			}
		}
	}
	now := b.Now().Unix()
	a := &Affair{ID: b.newID(), Title: textutil.Prefix(strings.TrimSpace(title), 100), Kind: kind, State: "open",
		Due: due, Person: person, Created: now}
	addSource(a, src)
	b.st.Affairs = append(b.st.Affairs, a)
	b.log(a, src.Kind, "new", "")
	b.prune()
	b.save()
	return a.ID, a.State
}

// State: an affair's state now ("" = unknown); a snoozed one is "open" again from its day on.
func (b *Book) State(id string) string {
	b.mu.Lock()
	defer b.mu.Unlock()
	a := b.find(id)
	if a == nil {
		return ""
	}
	if a.State == "snoozed" && open(a, b.Now().Format("2006-01-02")) {
		return "open"
	}
	return a.State
}

// OwnTasks: the affairs with a to-do in the assistant's own list and that to-do's id, for syncing back
// what the user ticked off in the app.
func (b *Book) OwnTasks() map[string]string {
	b.mu.Lock()
	defer b.mu.Unlock()
	out := map[string]string{}
	for _, a := range b.st.Affairs {
		for _, s := range a.Sources {
			if s.Kind == "task" && s.About == Own && !a.Settled() {
				out[s.Ref] = a.ID
			}
		}
	}
	return out
}
