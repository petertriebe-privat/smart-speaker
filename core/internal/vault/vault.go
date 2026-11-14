// Package vault: the assistant's Drive folder as an Obsidian vault. Every note the assistant
// writes there is Markdown with properties (YAML front matter) and wikilinks: a conversation links
// the person and its topics, a topic's history links the conversations. File names are what a
// wikilink can reach and Windows can store (no : [ ] # ^ | \ / * ? " < >). Notes in the old text
// format (until 11/2026) are converted by Convert. The same format is written by
// devices/atoms3r/src/assistant/memory.cpp and memory_archive.cpp.
package vault

import (
	"regexp"
	"strings"
	"unicode/utf8"
)

const (
	// MIME and Ext of every note.
	MIME = "text/markdown"
	Ext  = ".md"
	// HistoryMarker separates a dossier's header from its history entries.
	HistoryMarker = "\n## Verlauf\n"
	// LegacyHistoryMarker: the same in the old text format.
	LegacyHistoryMarker = "\nVerlauf:\n"
	// EntryPrefix starts a history entry (a line "### <time> – <heading>").
	EntryPrefix = "\n### "
	nameMax     = 120 // bytes of a file name without extension
)

// FileName: text as a note's file name (without extension) that a wikilink reaches.
func FileName(text string) string {
	text = strings.ReplaceAll(text, ": ", " – ")
	var b strings.Builder
	for _, r := range text {
		switch {
		case r < ' ' || strings.ContainsRune(`:[]#^|\/*?"<>`, r):
			b.WriteByte(' ')
		default:
			b.WriteRune(r)
		}
	}
	name := strings.Join(strings.Fields(b.String()), " ")
	if len(name) > nameMax {
		cut := nameMax
		for cut > 0 && !utf8.RuneStart(name[cut]) {
			cut--
		}
		name = strings.TrimSpace(name[:cut])
	}
	name = strings.Trim(name, ". ") // Obsidian hides a leading dot, Windows drops a trailing one
	if name == "" {
		name = "Notiz"
	}
	return name
}

// Link: a wikilink to the note name, with label when it differs.
func Link(name, label string) string {
	name = FileName(name)
	label = strings.NewReplacer("]]", "", "|", " ", "\n", " ").Replace(label)
	if label == "" || label == name {
		return "[[" + name + "]]"
	}
	return "[[" + name + "|" + label + "]]"
}

// TimeName: "2026-11-14 18:38" as a file name part ("2026-11-14 18.38").
func TimeName(at string) string { return strings.ReplaceAll(at, ":", ".") }

// ConversationName: the file name of the conversation at the time at with title.
func ConversationName(at, title string) string {
	if title == "" {
		return FileName(TimeName(at))
	}
	return FileName(TimeName(at) + " – " + title)
}

// DreamName: the file name of the night's review of day (2026-11-14).
func DreamName(day string) string { return FileName("Traum " + day) }

// NameFromSubject: the file name of an archive note from its mail subject ("Thema [key]: title",
// "Gespräch 2026-11-14 18:38 – title", "Traum 2026-11-14").
func NameFromSubject(subject string) string {
	if strings.HasPrefix(subject, "Thema [") {
		if end := strings.Index(subject, "]: "); end > 0 {
			return FileName(subject[end+3:])
		}
	}
	if rest, ok := strings.CutPrefix(subject, "Gespräch "); ok && len(rest) >= 16 {
		title := strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(rest[16:]), "–"))
		return ConversationName(rest[:16], title)
	}
	return FileName(subject)
}

// --- Properties -------------------------------------------------------------------------------

// Property is one line of the front matter; Values for a list, Value for one value.
type Property struct {
	Key    string
	Value  string
	Values []string
	Plain  bool // a date, a number: unquoted
}

func quote(s string) string {
	s = strings.ReplaceAll(strings.ReplaceAll(s, `\`, `\\`), `"`, `\"`)
	return `"` + strings.ReplaceAll(s, "\n", " ") + `"`
}

// FrontMatter: the properties block, empty ones left out.
func FrontMatter(props ...Property) string {
	var b strings.Builder
	b.WriteString("---\n")
	for _, p := range props {
		switch {
		case p.Values != nil:
			var values []string
			for _, v := range p.Values {
				if v = strings.TrimSpace(v); v != "" {
					values = append(values, v)
				}
			}
			if len(values) == 0 {
				continue
			}
			b.WriteString(p.Key + ":\n")
			for _, v := range values {
				b.WriteString("  - " + quote(v) + "\n")
			}
		case p.Value == "":
		case p.Plain:
			b.WriteString(p.Key + ": " + p.Value + "\n")
		default:
			b.WriteString(p.Key + ": " + quote(p.Value) + "\n")
		}
	}
	b.WriteString("---\n")
	return b.String()
}

// SplitList: "a, b, c" as a list.
func SplitList(text string) []string {
	var out []string
	for _, part := range strings.Split(text, ",") {
		if part = strings.TrimSpace(part); part != "" {
			out = append(out, part)
		}
	}
	if out == nil {
		out = []string{}
	}
	return out
}

func isoTime(at string) string { return strings.Replace(at, " ", "T", 1) }

// Body: text without its front matter.
func Body(text string) string {
	if !strings.HasPrefix(text, "---\n") {
		return text
	}
	if end := strings.Index(text[4:], "\n---\n"); end >= 0 {
		return strings.TrimLeft(text[4+end+5:], "\n")
	}
	return text
}

// Head: the first two lines of a note's body (title and the next line with text), for a search hit.
func Head(text string) string {
	var lines []string
	for _, line := range strings.Split(Body(text), "\n") {
		if strings.TrimSpace(line) != "" {
			lines = append(lines, line)
		}
		if len(lines) == 2 {
			break
		}
	}
	return strings.Join(lines, "\n")
}

// --- Notes ------------------------------------------------------------------------------------

// Conversation is a conversation's note in Gespräche.
type Conversation struct {
	At         string // "2026-11-14 18:38"
	Title      string
	With       string // the person's name, "" = the household
	Channel    string
	Summary    string // "" = not summarized
	Keywords   string // "a, b"
	Topics     []string
	Transcript string
}

// Markdown: the note.
func (c Conversation) Markdown() string {
	var topics []string
	for _, t := range c.Topics {
		topics = append(topics, Link(t, t))
	}
	with := ""
	if c.With != "" {
		with = Link(c.With, c.With)
	}
	text := FrontMatter(
		Property{Key: "typ", Value: "Gespräch"},
		Property{Key: "datum", Value: isoTime(c.At), Plain: true},
		Property{Key: "mit", Value: with},
		Property{Key: "kanal", Value: c.Channel},
		Property{Key: "themen", Values: topics},
		Property{Key: "stichwörter", Values: SplitList(c.Keywords)},
		Property{Key: "tags", Values: []string{"gespräch"}},
	)
	title := c.Title
	if title == "" {
		title = "Gespräch vom " + c.At
	}
	text += "# " + title + "\n\n"
	if c.Summary != "" {
		text += "**Kurzfassung:** " + c.Summary + "\n\n"
	} else {
		text += "Gespräch vom " + c.At + ", nicht zusammengefasst.\n\n"
	}
	return text + "## Transkript\n\n" + c.Transcript
}

// Dream is the night's review (in Gespräche).
type Dream struct {
	Day, Diary, Report string
}

// Markdown: the note.
func (d Dream) Markdown() string {
	text := FrontMatter(
		Property{Key: "typ", Value: "Traum"},
		Property{Key: "datum", Value: d.Day, Plain: true},
		Property{Key: "tags", Values: []string{"traum"}},
	) + "# Traum vom " + d.Day + "\n\nNächtliche Durchsicht des Gedächtnisses.\n\n" + strings.TrimSpace(d.Diary) + "\n"
	if report := strings.TrimSpace(d.Report); report != "" {
		text += "\n" + report + "\n"
	}
	return text
}

// Topic is a dossier's header in Themen; the history follows from HistoryMarker on.
type Topic struct {
	Key, Title, Status, Keywords, Updated, Created string
	Done                                           bool
}

// Markdown: the header.
func (t Topic) Markdown() string {
	state := "offen"
	if t.Done {
		state = "erledigt"
	}
	return FrontMatter(
		Property{Key: "typ", Value: "Thema"},
		Property{Key: "schlüssel", Value: t.Key},
		Property{Key: "status", Value: state},
		Property{Key: "stand", Value: t.Updated, Plain: true},
		Property{Key: "angelegt", Value: t.Created, Plain: true},
		Property{Key: "stichwörter", Values: SplitList(t.Keywords)},
		Property{Key: "tags", Values: []string{"thema"}},
	) + "# " + t.Title + "\n\n**Stand (" + t.Updated + "):** " + t.Status + "\n"
}

// Entry: a history entry of a dossier: at the time at, heading (a conversation's title), link the
// note it came from ("" = none), then the text.
func Entry(at, heading, link, text string) string {
	entry := strings.TrimPrefix(EntryPrefix, "\n") + at
	if heading != "" {
		entry += " – " + heading
	}
	entry += "\n"
	if link != "" {
		entry += "Aus: " + Link(link, heading) + "\n"
	}
	return entry + text + "\n"
}

// History: the history of a dossier (after the marker, either format), old entries in the new
// format.
func History(dossier string) string {
	at := strings.Index(dossier, HistoryMarker)
	if at >= 0 {
		return dossier[at+len(HistoryMarker):]
	}
	if at = strings.Index(dossier, LegacyHistoryMarker); at >= 0 {
		return legacyEntryRe.ReplaceAllStringFunc(dossier[at+len(LegacyHistoryMarker):], legacyEntry)
	}
	return ""
}

// Header: a dossier up to its history (either format).
func Header(dossier string) (string, bool) {
	for _, marker := range []string{HistoryMarker, LegacyHistoryMarker} {
		if at := strings.Index(dossier, marker); at >= 0 {
			return dossier[:at], true
		}
	}
	return dossier, false
}

// --- The old text format ----------------------------------------------------------------------

var (
	legacyEntryRe = regexp.MustCompile(`(?m)^--- (\d{4}-\d\d-\d\d \d\d:\d\d)(?: \((.*)\))? ---$`)
	legacyFieldRe = regexp.MustCompile(`(?m)^([A-Za-zÄÖÜäöüß]+)(?: \(([^)]*)\))?: (.*)$`)
	legacyConvRe  = regexp.MustCompile(`^Gespräch vom (\d{4}-\d\d-\d\d \d\d:\d\d)(?:: (.*)| \(nicht zusammengefasst\))\n`)
	legacyDreamRe = regexp.MustCompile(`^Traum vom (\d{4}-\d\d-\d\d) \(nächtliche Durchsicht des Gedächtnisses\)\n\n`)
)

func legacyEntry(line string) string {
	g := legacyEntryRe.FindStringSubmatch(line)
	if g[2] == "" || g[2] == "Nächtliche Durchsicht" {
		return strings.TrimSuffix(Entry(g[1], g[2], "", ""), "\n\n")
	}
	return strings.TrimSuffix(Entry(g[1], g[2], ConversationName(g[1], g[2]), ""), "\n\n")
}

// fields: "Name: value" lines of an old header.
func fields(header string) map[string]string {
	out := map[string]string{}
	for _, g := range legacyFieldRe.FindAllStringSubmatch(header, -1) {
		out[g[1]] = g[3]
		if g[2] != "" {
			out[g[1]+"Datum"] = g[2]
		}
	}
	return out
}

// Convert: a note in the old text format (file name with .txt) as Markdown with its new file name
// (without extension); ok false when text is no note of the old format (left as it is).
func Convert(fileName, text string) (name, markdown string, ok bool) {
	text = strings.ReplaceAll(text, "\r\n", "\n")
	if strings.HasPrefix(text, "---\n") {
		return "", "", false // already converted
	}
	base := strings.TrimSuffix(fileName, ".txt")
	switch {
	case strings.HasPrefix(text, "Thema: "):
		header, _ := Header(text)
		f := fields(header)
		t := Topic{Key: f["Schlüssel"], Title: f["Thema"], Status: f["Stand"], Keywords: f["Stichwörter"],
			Updated: f["StandDatum"], Created: f["Angelegt"], Done: f["Status"] == "erledigt"}
		if t.Title == "" {
			return "", "", false
		}
		markdown = t.Markdown()
		if history := strings.TrimSpace(History(text)); history != "" {
			markdown += HistoryMarker + history + "\n"
		}
		return FileName(t.Title), markdown, true
	case legacyConvRe.MatchString(text):
		g := legacyConvRe.FindStringSubmatch(text)
		head, transcript, _ := strings.Cut(text, "\nTranskript:\n")
		f := fields(head)
		c := Conversation{At: g[1], Title: g[2], With: f["Mit"], Channel: f["Kanal"], Summary: f["Kurzfassung"],
			Keywords: f["Stichwörter"], Topics: SplitList(f["Themen"]), Transcript: strings.TrimLeft(transcript, "\n")}
		name = ConversationName(c.At, c.Title)
		if c.Title == "" {
			name = NameFromSubject(base)
		}
		return name, c.Markdown(), true
	case legacyDreamRe.MatchString(text):
		g := legacyDreamRe.FindStringSubmatch(text)
		return DreamName(g[1]), Dream{Day: g[1], Diary: text[len(g[0]):]}.Markdown(), true
	}
	return "", "", false
}
