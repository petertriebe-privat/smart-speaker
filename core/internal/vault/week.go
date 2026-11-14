package vault

import (
	"fmt"
	"regexp"
	"sort"
	"strings"
	"time"
)

// Conversations older than a month are folded into one note per week (folder Wochen): a line per
// conversation with its summary, person and topics; the conversation notes with their transcripts
// go to Drive's trash. What lasts is in the memory's core and the topics' dossiers; the week keeps
// what was talked about, findable by recall (google.Archive.FoldWeek).

// Week: the ISO week of the time at ("2026-11-14 18:38") as "2026-W46", and its Monday and
// Sunday. ok false when at is no time.
func Week(at string) (key string, monday, sunday time.Time, ok bool) {
	t, err := time.ParseInLocation("2006-01-02 15:04", strings.TrimSpace(at), time.Local)
	if err != nil {
		if t, err = time.ParseInLocation("2006-01-02", strings.TrimSpace(at), time.Local); err != nil {
			return "", time.Time{}, time.Time{}, false
		}
	}
	if t.Year() < 2000 { // a clock not set yet (1970): no week to file it under
		return "", time.Time{}, time.Time{}, false
	}
	year, week := t.ISOWeek()
	day := (int(t.Weekday()) + 6) % 7 // Monday 0
	monday = time.Date(t.Year(), t.Month(), t.Day()-day, 0, 0, 0, 0, time.Local)
	return fmt.Sprintf("%d-W%02d", year, week), monday, monday.AddDate(0, 0, 6), true
}

// WeekName: the file name of the week's note.
func WeekName(key string) string { return FileName("Woche " + key) }

// ParseConversation: a conversation note (Markdown of Conversation.Markdown, or another note with
// the file name name) as far as a week line needs it.
func ParseConversation(name, text string) Conversation {
	text = strings.ReplaceAll(text, "\r\n", "\n")
	var c Conversation
	if strings.HasPrefix(text, "---\n") {
		if end := strings.Index(text[4:], "\n---\n"); end >= 0 {
			key := ""
			for _, line := range strings.Split(text[4:4+end], "\n") {
				if item, ok := strings.CutPrefix(line, "  - "); ok {
					if key == "themen" {
						c.Topics = append(c.Topics, linkLabel(unquoteYAML(item)))
					}
					continue
				}
				k, v, _ := strings.Cut(line, ":")
				key, v = k, unquoteYAML(strings.TrimSpace(v))
				switch key {
				case "datum":
					c.At = strings.Replace(v, "T", " ", 1)
				case "mit":
					c.With = linkLabel(v)
				case "kanal":
					c.Channel = v
				}
			}
		}
	}
	for _, line := range strings.Split(Body(text), "\n") {
		switch {
		case c.Title == "" && strings.HasPrefix(line, "# "):
			c.Title = strings.TrimSpace(line[2:])
		case strings.HasPrefix(line, "**Kurzfassung:** "):
			c.Summary = strings.TrimSpace(strings.TrimPrefix(line, "**Kurzfassung:** "))
		}
	}
	if c.Title == "" || strings.HasPrefix(c.Title, "Gespräch vom ") {
		c.Title = strings.TrimSuffix(name, Ext)
		if len(c.Title) > 16 && strings.HasPrefix(c.Title[16:], " – ") { // "2026-11-14 18.38 – title"
			c.Title = c.Title[16+len(" – "):]
		}
		if c.Title == "nicht zusammengefasst" {
			c.Title = "Gespräch"
		}
	}
	return c
}

func unquoteYAML(v string) string {
	if len(v) >= 2 && v[0] == '"' && v[len(v)-1] == '"' {
		v = strings.NewReplacer(`\"`, `"`, `\\`, `\`).Replace(v[1 : len(v)-1])
	}
	return v
}

// linkLabel: "[[Note|Label]]" or "[[Note]]" as its label; other text as it is.
func linkLabel(v string) string {
	v = strings.TrimSpace(v)
	if !strings.HasPrefix(v, "[[") || !strings.HasSuffix(v, "]]") {
		return v
	}
	v = v[2 : len(v)-2]
	if bar := strings.IndexByte(v, '|'); bar >= 0 {
		return v[bar+1:]
	}
	return v
}

// WeekLine: one conversation in a week's note.
func WeekLine(c Conversation) string {
	line := "- **" + c.At + "** " + strings.ReplaceAll(c.Title, "\n", " ")
	summary := strings.ReplaceAll(c.Summary, "\n", " ")
	if summary == "" {
		summary = "nicht zusammengefasst"
	}
	line += " — " + summary
	var about []string
	if c.With != "" {
		about = append(about, "mit "+Link(c.With, c.With))
	}
	if len(c.Topics) > 0 {
		var links []string
		for _, t := range c.Topics {
			links = append(links, Link(t, t))
		}
		about = append(about, "Themen: "+strings.Join(links, ", "))
	}
	if c.Channel != "" {
		about = append(about, c.Channel)
	}
	if len(about) > 0 {
		line += " (" + strings.Join(about, "; ") + ")"
	}
	return line
}

var (
	weekWithRe  = regexp.MustCompile(`\(mit \[\[([^\]|]+)(?:\|[^\]]*)?\]\]`)
	weekTopicRe = regexp.MustCompile(`\[\[([^\]|]+)(?:\|([^\]]*))?\]\]`)
)

// WeekNote: the week's note with the lines of its current version old ("" = none) and lines, one
// per conversation, sorted by time, without doubles.
func WeekNote(key string, monday, sunday time.Time, old string, lines []string) string {
	seen := map[string]bool{}
	var all []string
	for _, line := range strings.Split(strings.ReplaceAll(old, "\r\n", "\n"), "\n") {
		if strings.HasPrefix(line, "- **") {
			lines = append(lines, line)
		}
	}
	for _, line := range lines {
		if !seen[line] {
			seen[line] = true
			all = append(all, line)
		}
	}
	sort.Strings(all)
	var people, topics []string
	have := map[string]bool{}
	for _, line := range all {
		if g := weekWithRe.FindStringSubmatch(line); g != nil && !have["p"+g[1]] {
			have["p"+g[1]] = true
			people = append(people, "[["+g[1]+"]]")
		}
		if at := strings.Index(line, "Themen: "); at >= 0 {
			for _, g := range weekTopicRe.FindAllStringSubmatch(line[at:], -1) {
				if !have["t"+g[1]] {
					have["t"+g[1]] = true
					topics = append(topics, "[["+g[1]+"]]")
				}
			}
		}
	}
	text := FrontMatter(
		Property{Key: "typ", Value: "Woche"},
		Property{Key: "woche", Value: key},
		Property{Key: "von", Value: monday.Format("2006-01-02"), Plain: true},
		Property{Key: "bis", Value: sunday.Format("2006-01-02"), Plain: true},
		Property{Key: "gespräche", Value: fmt.Sprint(len(all)), Plain: true},
		Property{Key: "personen", Values: append([]string{}, people...)},
		Property{Key: "themen", Values: append([]string{}, topics...)},
		Property{Key: "tags", Values: []string{"woche"}},
	)
	text += "# Woche " + key + " (" + monday.Format("02.01.") + "–" + sunday.Format("02.01.2006") + ")\n\n" +
		"Die Gespräche dieser Woche in Kurzfassung. Ihre Transkripte sind nach einem Monat gelöscht; was davon " +
		"bleibt, steht in den Themen und im Gedächtnis.\n\n## Gespräche\n\n"
	return text + strings.Join(all, "\n") + "\n"
}
