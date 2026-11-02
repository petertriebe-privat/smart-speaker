package affairs

import (
	"fmt"
	"slices"
	"strings"
	"time"
)

// Trust: what the assistant does with the user's own calendar events and to-dos when the affair they
// belong to is settled (#71, the stages as the mail watch's #37):
//   - hide (default): only his agenda leaves them out;
//   - suggest: he offers to delete the event or tick off the to-do;
//   - act: he ticks off the user's to-dos himself; calendar events are still only offered (never
//     deleted or declined without asking, someone else's least of all).
//
// The assistant's own to-do list ("Von Jarvis") it keeps in step freely at every stage.
var TrustLevels = []string{"hide", "suggest", "act"}

func (b *Book) Trust(person string) string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.trustLocked(person)
}

func (b *Book) trustLocked(person string) string {
	if t := b.st.Trust[person]; t != "" {
		return t
	}
	return "hide"
}

func (b *Book) SetTrust(person, level string) error {
	if !valid(level, TrustLevels) {
		return fmt.Errorf("trust is hide, suggest or act")
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.st.Trust == nil {
		b.st.Trust = map[string]string{}
	}
	b.st.Trust[person] = level
	b.save()
	return nil
}

// followUpLocked: what the answer to update_affair says about the calendar events and to-dos of
// the affairs it settled. Caller holds the lock.
func (b *Book) followUpLocked(settled []Affair) string {
	var out strings.Builder
	for _, a := range settled {
		trust := b.trustLocked(a.Person)
		for _, s := range a.Sources {
			switch {
			case s.Kind == "task" && s.About == Own:
				out.WriteString("\n[" + a.ID + "] the assistant's own to-do is ticked off too.")
			case s.Kind == "task" && trust == "act":
				out.WriteString("\n[" + a.ID + "] The user's to-do (" + s.Ref + ") is ticked off too.")
			case s.Kind == "task" && trust == "suggest":
				out.WriteString("\n[" + a.ID + "] It is also in the user's to-do list: offer to tick it off (change_todo complete, id " +
					s.Ref + "); only on their yes.")
			case s.Kind == "calendar" && trust != "hide":
				out.WriteString("\n[" + a.ID + "] It is also in the user's calendar: offer to delete the event (delete_event, id " +
					s.Ref + "); only on their yes, and never someone else's invitation without asking.")
			case s.Kind == "calendar" || s.Kind == "task":
				out.WriteString("\n[" + a.ID + "] the assistant's agenda leaves it out; the " + map[string]string{"calendar": "calendar event",
					"task": "to-do"}[s.Kind] + " itself is unchanged.")
			}
		}
	}
	return out.String()
}

// SettledNote: for the person, in German, that a mail settled affair a and what became of its
// to-dos and calendar events (the trust decides); "" if a is not settled.
func (b *Book) SettledNote(a Affair) string {
	if !a.Settled() {
		return ""
	}
	b.mu.Lock()
	trust := b.trustLocked(a.Person)
	b.mu.Unlock()
	text := "✔ Damit erledigt: „" + a.Title + "“"
	if a.State == "obsolete" {
		text = "✔ Damit hinfällig: „" + a.Title + "“"
	}
	var more []string
	for _, s := range a.Sources {
		switch {
		case s.Kind == "task" && (s.About == Own || trust == "act"):
			more = append(more, "das To-do habe ich abgehakt")
		case s.Kind == "task":
			more = append(more, "dein To-do ist noch offen – soll ich es abhaken?")
		case s.Kind == "calendar":
			when := ""
			if len(a.Due) >= 10 {
				if day, err := time.Parse("2006-01-02", a.Due[:10]); err == nil {
					when = " am " + day.Format("02.01.")
				}
			}
			more = append(more, "der Termin"+when+" steht noch im Kalender – soll ich ihn löschen?")
		}
	}
	if len(more) > 0 {
		text += ": " + strings.Join(slices.Compact(more), "; ")
	}
	return text
}

// Quiet: the day until which the person wants no unasked hints ("Heute nicht, Jarvis"), "" = none.
func (b *Book) Quiet(person string) string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.quietLocked(person)
}

func (b *Book) quietLocked(person string) string {
	until := b.st.Quiet[person]
	if until == "" || until < b.Now().Format("2006-01-02") {
		return ""
	}
	return until
}

// SetQuiet: no unasked hints until the end of `until` (YYYY-MM-DD; "" = today; "off" = again).
func (b *Book) SetQuiet(person, until string) (string, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.st.Quiet == nil {
		b.st.Quiet = map[string]string{}
	}
	switch until = strings.TrimSpace(until); until {
	case "off":
		delete(b.st.Quiet, person)
		b.save()
		return "Hints are on again.", nil
	case "":
		until = b.Now().Format("2006-01-02")
	default:
		if d := normDue(until); d == "" {
			return "", fmt.Errorf("until as YYYY-MM-DD, '' = today, 'off'")
		} else {
			until = d[:10]
		}
	}
	b.st.Quiet[person] = until
	b.save()
	return "No unasked hints until the end of " + until + " (asked questions are answered as always; only " +
		"something urgent the user can still act on today is said).", nil
}
