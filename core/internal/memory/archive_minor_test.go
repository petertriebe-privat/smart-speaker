package memory

import (
	"testing"
	"time"

	"echo-assistant/internal/config"
	"echo-assistant/internal/people"
	"echo-assistant/internal/store"
)

type nullArchive struct{}

func (nullArchive) Available() bool                                  { return true }
func (nullArchive) LoadTopic(key, title string) (string, error)      { return "", nil }
func (nullArchive) StoreTopic(key, subject, text string) error       { return nil }
func (nullArchive) StoreConversation(at, subject, text string) error { return nil }
func (nullArchive) LoadConversation(at string) (string, error)       { return "", nil }
func (nullArchive) Search(terms []string) ([]string, error)          { return nil, nil }

// A short command gets no note in the archive (only the index); a conversation does, and so does a
// short one that touched a topic (its dossier links the note).
func TestMinorNotArchived(t *testing.T) {
	store.Dir = t.TempDir()
	m := &Memory{People: people.New(config.Secrets{"OWNER_NAME": "Admin"}), Archive: nullArchive{}}
	m.core.normalize()
	started := time.Now().Add(-time.Hour).Unix()
	conv := conversation{transcript: "Du: Licht an\nJarvis: Erledigt.", started: started}

	m.apply(update{Title: "Licht an", Summary: "Licht eingeschaltet.", Minor: true}, conv)
	if n := len(store.List(outboxDir)); n != 0 {
		t.Fatalf("a short command was archived: %d notes", n)
	}
	if len(m.core.Recent) != 1 {
		t.Errorf("the short command is missing in the recent conversations: %+v", m.core.Recent)
	}

	m.apply(update{Title: "Urlaub planen", Summary: "Mosel im Mai."}, conv)
	if n := len(store.List(outboxDir)); n != 1 {
		t.Fatalf("a conversation was not archived: %d notes", n)
	}

	u := update{Title: "Heizung", Summary: "Thermostat auf 21 Grad.", Minor: true}
	u.Topics = append(u.Topics, struct {
		Title    string
		Status   string
		Keywords []string
		Notes    string
		Done     bool
	}{Title: "Heizung", Status: "21 Grad eingestellt."})
	m.apply(u, conv)
	if n := len(store.List(outboxDir)); n < 2 {
		t.Errorf("a short command with a topic has no conversation note: %d notes", n)
	}
}
