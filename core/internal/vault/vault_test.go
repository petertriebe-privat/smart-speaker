package vault

import (
	"strings"
	"testing"
)

func TestFileName(t *testing.T) {
	for in, want := range map[string]string{
		"Urlaub: Mosel":            "Urlaub – Mosel",
		"2026-11-14 18:38":         "2026-11-14 18 38",
		"a/b\\c [x] #1 ^2 |3 ?*<>": "a b c x 1 2 3",
		".obsidian.":               "obsidian",
		"   ":                      "Notiz",
		"Zeile\nzwei":              "Zeile zwei",
	} {
		if got := FileName(in); got != want {
			t.Errorf("FileName(%q) = %q, want %q", in, got, want)
		}
	}
	if got := FileName(strings.Repeat("ä", 100)); len(got) > nameMax || !strings.HasPrefix(got, "ää") || strings.ContainsRune(got, '�') {
		t.Errorf("a long name is cut on a character: %q", got)
	}
}

func TestNames(t *testing.T) {
	for subject, want := range map[string]string{
		"Thema [heizung]: Heizung: entlüften":               "Heizung – entlüften",
		"Gespräch 2026-11-14 18:38 – Einkauf":               "2026-11-14 18.38 – Einkauf",
		"Gespräch 2026-11-14 18:38 – nicht zusammengefasst": "2026-11-14 18.38 – nicht zusammengefasst",
		"Traum 2026-11-14":                                  "Traum 2026-11-14",
	} {
		if got := NameFromSubject(subject); got != want {
			t.Errorf("NameFromSubject(%q) = %q, want %q", subject, got, want)
		}
	}
	if got := Link("Heizung: alt", "Heizung: alt"); got != "[[Heizung – alt|Heizung: alt]]" {
		t.Errorf("Link: %q", got)
	}
	if got := Link("Anna", "Anna"); got != "[[Anna]]" {
		t.Errorf("Link: %q", got)
	}
}

func TestConversation(t *testing.T) {
	text := Conversation{At: "2026-11-14 18:38", Title: "Heizung", With: "Anna", Channel: "Telegram",
		Summary: "Die Heizung wird entlüftet.", Keywords: "heizung, entlüften", Topics: []string{"Heizung"},
		Transcript: "Anna: Die Heizung gluckert.\nJarvis: Ich schaue nach."}.Markdown()
	for _, want := range []string{
		"---\ntyp: \"Gespräch\"\ndatum: 2026-11-14T18:38\nmit: \"[[Anna]]\"\nkanal: \"Telegram\"\n",
		"themen:\n  - \"[[Heizung]]\"\n", "stichwörter:\n  - \"heizung\"\n  - \"entlüften\"\n", "tags:\n  - \"gespräch\"\n---\n",
		"# Heizung\n\n**Kurzfassung:** Die Heizung wird entlüftet.\n\n## Transkript\n\nAnna: Die Heizung gluckert.",
	} {
		if !strings.Contains(text, want) {
			t.Errorf("missing %q in\n%s", want, text)
		}
	}
	if got := Head(text); got != "# Heizung\n**Kurzfassung:** Die Heizung wird entlüftet." {
		t.Errorf("Head: %q", got)
	}
	plain := Conversation{At: "2026-11-14 18:38", Transcript: "x"}.Markdown()
	if strings.Contains(plain, "mit:") || !strings.Contains(plain, "# Gespräch vom 2026-11-14 18:38\n\nGespräch vom 2026-11-14 18:38, nicht zusammengefasst.") {
		t.Errorf("not summarized:\n%s", plain)
	}
}

func TestTopicAndHistory(t *testing.T) {
	header := Topic{Key: "heizung", Title: "Heizung", Status: "Entlüftet.", Keywords: "heizung", Updated: "2026-11-14",
		Created: "2026-10-01"}.Markdown()
	if !strings.Contains(header, "status: \"offen\"\nstand: 2026-11-14\nangelegt: 2026-10-01\n") ||
		!strings.HasSuffix(header, "# Heizung\n\n**Stand (2026-11-14):** Entlüftet.\n") {
		t.Errorf("header:\n%s", header)
	}
	entry := Entry("2026-11-14 18:38", "Heizung", ConversationName("2026-11-14 18:38", "Heizung"), "Entlüftet.")
	if entry != "### 2026-11-14 18:38 – Heizung\nAus: [[2026-11-14 18.38 – Heizung|Heizung]]\nEntlüftet.\n" {
		t.Errorf("entry: %q", entry)
	}
	dossier := header + HistoryMarker + entry
	if History(dossier) != entry {
		t.Errorf("history: %q", History(dossier))
	}
	if head, ok := Header(dossier); !ok || head != header {
		t.Errorf("header of the dossier: %q", head)
	}
	legacy := "Thema: Heizung\nVerlauf:\n--- 2026-10-01 09:00 (Heizung kalt) ---\nKalt.\n--- 2026-10-02 03:00 (Nächtliche Durchsicht) ---\nGeprüft.\n"
	want := "### 2026-10-01 09:00 – Heizung kalt\nAus: [[2026-10-01 09.00 – Heizung kalt|Heizung kalt]]\nKalt.\n" +
		"### 2026-10-02 03:00 – Nächtliche Durchsicht\nGeprüft.\n"
	if got := History(legacy); got != want {
		t.Errorf("legacy history:\n%q\nwant\n%q", got, want)
	}
}

func TestConvert(t *testing.T) {
	topic := "Thema: Heizung\nSchlüssel: heizung\nStand (2026-10-02): Läuft wieder.\nStichwörter: heizung, wärme\n" +
		"Status: erledigt\nAngelegt: 2026-10-01\n\nVerlauf:\n--- 2026-10-01 09:00 (Heizung kalt) ---\nKalt.\n"
	name, md, ok := Convert("Thema [heizung]: Heizung.txt", topic)
	if !ok || name != "Heizung" || !strings.Contains(md, "schlüssel: \"heizung\"\nstatus: \"erledigt\"\nstand: 2026-10-02\n") ||
		!strings.Contains(md, "**Stand (2026-10-02):** Läuft wieder.\n"+HistoryMarker+"### 2026-10-01 09:00 – Heizung kalt\n") {
		t.Errorf("topic: %v %q\n%s", ok, name, md)
	}
	conv := "Gespräch vom 2026-10-01 09:00: Heizung kalt\nMit: Anna\nKanal: Telegram\nKurzfassung: Kalt.\n" +
		"Stichwörter: heizung\nThemen: Heizung, Wärme\n\nTranskript:\nAnna: Kalt hier.\nJarvis: Ich prüfe es."
	name, md, ok = Convert("Gespräch 2026-10-01 09:00 – Heizung kalt.txt", conv)
	if !ok || name != "2026-10-01 09.00 – Heizung kalt" || !strings.Contains(md, "mit: \"[[Anna]]\"") ||
		!strings.Contains(md, "  - \"[[Wärme]]\"\n") || !strings.HasSuffix(md, "## Transkript\n\nAnna: Kalt hier.\nJarvis: Ich prüfe es.") {
		t.Errorf("conversation: %v %q\n%s", ok, name, md)
	}
	raw := "Gespräch vom 2026-10-01 09:00 (nicht zusammengefasst)\n\nTranskript:\nAnna: Hallo."
	name, md, ok = Convert("Gespräch 2026-10-01 09:00 – nicht zusammengefasst.txt", raw)
	if !ok || name != "2026-10-01 09.00 – nicht zusammengefasst" || !strings.HasSuffix(md, "Anna: Hallo.") {
		t.Errorf("not summarized: %v %q\n%s", ok, name, md)
	}
	dream := "Traum vom 2026-10-02 (nächtliche Durchsicht des Gedächtnisses)\n\nIch habe aufgeräumt.\n\n- Thema „Heizung“ erledigt\n"
	name, md, ok = Convert("Traum 2026-10-02.txt", dream)
	if !ok || name != "Traum 2026-10-02" || !strings.Contains(md, "datum: 2026-10-02\n") || !strings.HasSuffix(md, "- Thema „Heizung“ erledigt\n") {
		t.Errorf("dream: %v %q\n%s", ok, name, md)
	}
	if _, _, ok = Convert("Einkauf.txt", "Milch, Brot"); ok {
		t.Error("a note of the household is no old note")
	}
	if _, _, ok = Convert("x.txt", md); ok {
		t.Error("a converted note again")
	}
}
