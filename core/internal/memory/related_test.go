package memory

import (
	"strings"
	"testing"
	"time"

	"echo-assistant/internal/config"
	"echo-assistant/internal/people"
	"echo-assistant/internal/store"
)

func relatedMemory(t *testing.T) (*Memory, string) {
	store.Dir = t.TempDir()
	reg := people.New(config.Secrets{"OWNER_NAME": "Admin"})
	m := &Memory{People: reg}
	m.core.normalize()
	admin := reg.Admin()
	now := time.Now()
	add := func(hoursAgo int, person, title, summary, topics string) {
		at := now.Add(-time.Duration(hoursAgo) * time.Hour)
		m.addToIndex(indexEntry{At: at.Format("2006-01-02 15:04"), T: at.Unix(), Title: title, Summary: summary,
			Topics: topics, Person: person})
	}
	add(50, admin, "Heizung kalt", "Die Heizung ist kalt.", "Heizung")
	add(30, admin, "Strom und Heizung", "Neuer Stromanbieter, Heizung entlüftet.", "Heizung, Stromanbieter")
	add(10, "", "Einkauf", "Milch.", "Einkaufsliste")
	add(5, admin, "Heizung läuft", "Läuft wieder.", "Heizung")
	m.core.Topics = []Topic{{Key: "heizung", Title: "Heizung", Status: "Entlüftet, läuft."},
		{Key: "strom", Title: "Stromanbieter", Status: "Gewechselt.", Done: true}}
	m.core.Rules = []Rule{{ID: 1, Text: "Sag mir Heizungsprobleme sofort.", Person: admin, Area: "home"}}
	m.core.Facts = []Fact{{ID: 2, Category: "Haushalt", Text: "Die Heizung im Keller ist eine Gastherme."}}
	return m, admin
}

func TestRelated(t *testing.T) {
	m, _ := relatedMemory(t)
	person := m.related("admin")
	for _, want := range []string{"Topics with Admin:\n  Heizung (offen: Entlüftet, läuft.), 3 conversation(s)",
		"Stromanbieter (erledigt: Gewechselt.), 1 conversation(s)", "Conversations with Admin: 3", "Heizung läuft: Läuft wieder.",
		"Rules for Admin: Sag mir Heizungsprobleme sofort."} {
		if !strings.Contains(person, want) {
			t.Errorf("person: missing %q in\n%s", want, person)
		}
	}
	topic := m.related("Heizung")
	for _, want := range []string{"Linked to the topic Heizung (offen", "People: Admin (3)",
		"Stromanbieter (erledigt: Gewechselt.), 1 time(s)", "Conversations: 3", "[2] Die Heizung im Keller ist eine Gastherme."} {
		if !strings.Contains(topic, want) {
			t.Errorf("topic: missing %q in\n%s", want, topic)
		}
	}
	if word := m.related("einkauf"); !strings.Contains(word, "Linked to the topic Einkaufsliste") {
		t.Errorf("word: %s", word)
	}
	if none := m.related("Mondlandung"); !strings.Contains(none, "Try recall") {
		t.Errorf("nothing: %s", none)
	}
}

// The person's page: the vault's links (topics, rulebook), and the household's own notes stay.
func TestPersonPage(t *testing.T) {
	m, admin := relatedMemory(t)
	m.core.Profiles[admin] = map[string]string{"home": "Am Fluss"}
	rel := loadRelations()
	page := m.personPageLocked(admin, rel, "")
	for _, want := range []string{"typ: \"Person\"\nname: \"Admin\"\nthemen:\n  - \"[[Heizung]]\"\n",
		"# Admin\n", "## Profil\n- **Wohnort:** Am Fluss\n", "## Offene Themen\n- [[Heizung]] (3 Gespräch(e)): Entlüftet, läuft.\n",
		"## Erledigte Themen\n- [[Stromanbieter]] (1 Gespräch(e))\n", "- Sag mir Heizungsprobleme sofort.\n\nAlle Regeln: [[Regelbuch]]",
		"## Letzte Gespräche\n- **", "\n## Eigene Notizen\n"} {
		if !strings.Contains(page, want) {
			t.Errorf("missing %q in\n%s", want, page)
		}
	}
	edited := page + "Mag keinen Kaffee nach 16 Uhr.\n"
	again := m.personPageLocked(admin, rel, edited)
	if again != edited {
		t.Errorf("own notes lost or the page changed:\n%s", again)
	}
}
