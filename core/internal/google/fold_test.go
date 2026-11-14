package google

import (
	"strings"
	"testing"
	"time"

	"echo-assistant/internal/prompts"
	"echo-assistant/internal/vault"
)

// Old conversations become one week note; their notes go to the trash. Dreams, the household's
// notes, newer weeks and a week that reaches over the cutoff stay.
func TestFoldWeek(t *testing.T) {
	a, f := fakeAccount(t)
	root := f.add(prompts.Name(), folderMIME, "root", "", map[string]string{"assistant": "root"})
	conversations := f.add(driveConversations, folderMIME, root.id, "", nil)
	note := func(at, title, summary string) *fakeFile {
		text := vault.Conversation{At: at, Title: title, With: "Anna", Summary: summary, Topics: []string{"Heizung"},
			Transcript: "Anna: hallo"}.Markdown()
		return f.add(vault.ConversationName(at, title)+vault.Ext, vault.MIME, conversations.id, text, map[string]string{"at": at})
	}
	w46a := note("2026-11-10 07:00", "Heizung kalt", "Die Heizung ist kalt.")
	w46b := note("2026-11-14 18:38", "Heizung entlüftet", "Entlüftet.")
	w47 := note("2026-11-17 09:00", "Einkauf", "Milch.")
	dream := f.add("Traum 2026-11-11.md", vault.MIME, conversations.id, "x", map[string]string{"at": "2026-11-11 03:00"})
	own := f.add("Meine Notiz.md", vault.MIME, conversations.id, "x", nil)
	recent := note("2026-11-20 10:00", "Neu", "Neu.")
	recent.created = time.Now()

	archive := a.Archive()
	cutoff := time.Date(2026, 11, 20, 0, 0, 0, 0, time.Local) // W46 is before it, W47 reaches over it
	more, err := archive.FoldWeek(cutoff)
	if err != nil || more {
		t.Fatalf("fold: %v %v", more, err)
	}
	if !w46a.trashed || !w46b.trashed || w47.trashed || dream.trashed || own.trashed || recent.trashed {
		t.Errorf("trashed: w46 %v %v, w47 %v, dream %v, own %v, recent %v", w46a.trashed, w46b.trashed, w47.trashed,
			dream.trashed, own.trashed, recent.trashed)
	}
	var week *fakeFile
	for _, file := range f.files {
		if file.props["week"] == "2026-W46" {
			week = file
		}
	}
	if week == nil || week.name != "Woche 2026-W46.md" ||
		!strings.Contains(week.content, "- **2026-11-10 07:00** Heizung kalt — Die Heizung ist kalt. (mit [[Anna]]; Themen: [[Heizung]])\n"+
			"- **2026-11-14 18:38** Heizung entlüftet — Entlüftet.") || strings.Contains(week.content, "Anna: hallo") {
		t.Fatalf("week note: %+v", week)
	}

	// A late note of the same week joins the note; nothing else is left to fold.
	late := note("2026-11-12 12:00", "Nachtrag", "Spät.")
	if more, err = archive.FoldWeek(cutoff); err != nil || more || !late.trashed {
		t.Fatalf("second round: %v %v %v", more, err, late.trashed)
	}
	if !strings.Contains(week.content, "gespräche: 3") || !strings.Contains(week.content, "**2026-11-12 12:00** Nachtrag") {
		t.Errorf("merged week:\n%s", week.content)
	}
	if more, err = archive.FoldWeek(cutoff); err != nil || more {
		t.Errorf("nothing left: %v %v", more, err)
	}
}
