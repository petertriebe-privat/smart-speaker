package google

import (
	"log"
	"sort"
	"strings"
	"time"

	"echo-assistant/internal/vault"
)

const (
	driveWeeks = "Wochen" // the week notes (vault.WeekNote)
	foldBatch  = 100      // conversation notes per round
)

// FoldWeek folds the conversation notes of the oldest week before cutoff into the week's note
// (Wochen/Woche 2026-W46.md: a line per conversation, see package vault) and moves them to Drive's
// trash (restorable for 30 days). Only the assistant's own conversation notes (appProperties at),
// never dreams or the household's notes. more: notes are left for the next round. Only the
// primary device calls it (memory.ArchiveWritable).
func (r *Archive) FoldWeek(cutoff time.Time) (more bool, err error) {
	found, err := r.d.List(driveConversations, "createdTime < '"+cutoff.UTC().Format(time.RFC3339)+"'", 200)
	if err != nil {
		return false, err
	}
	type item struct {
		e       Entry
		at, key string
	}
	weeks := map[string][]item{}
	var monday, sunday = map[string]time.Time{}, map[string]time.Time{}
	for _, e := range found {
		at := e.Properties["at"]
		if at == "" || strings.HasPrefix(e.Name, "Traum ") || e.Type != vault.MIME {
			continue // not a conversation of the assistant's, a dream, or not converted yet (Tidy)
		}
		key, mon, sun, ok := vault.Week(at)
		if !ok || !mon.AddDate(0, 0, 7).Before(cutoff) {
			continue // the whole week must be before the cutoff
		}
		weeks[key] = append(weeks[key], item{e, at, key})
		monday[key], sunday[key] = mon, sun
	}
	if len(weeks) == 0 {
		return false, nil
	}
	var keys []string
	for k := range weeks {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	key := keys[0]
	items := weeks[key]
	more = len(keys) > 1 || len(found) == 200
	if len(items) > foldBatch {
		items, more = items[:foldBatch], true
	}

	var lines []string
	for _, it := range items {
		data, err := r.d.Download(it.e.ID)
		if err != nil {
			return false, err
		}
		c := vault.ParseConversation(it.e.Name, string(data))
		if c.At == "" {
			c.At = it.at
		}
		lines = append(lines, vault.WeekLine(c))
	}
	// The week's note: replaced with the lines it had and the new ones.
	name := vault.WeekName(key) + vault.Ext
	existing, err := r.d.List(driveWeeks, propertyQuery("week", key), 1)
	if err != nil {
		return false, err
	}
	if len(existing) > 0 {
		old, err := r.d.Download(existing[0].ID)
		if err != nil {
			return false, err
		}
		err = r.d.Replace(existing[0].ID, vault.MIME, []byte(vault.WeekNote(key, monday[key], sunday[key], string(old), lines)))
		if err != nil {
			return false, err
		}
	} else if err := r.d.Create(driveWeeks, name, vault.MIME, []byte(vault.WeekNote(key, monday[key], sunday[key], "", lines)),
		map[string]string{"week": key}); err != nil {
		return false, err
	}
	// Only now, with the week's note stored: the conversation notes to the trash.
	for _, it := range items {
		if err := r.d.TrashFile(it.e.ID); err != nil {
			return false, err
		}
	}
	log.Printf("[Drive] %s: %d conversations folded into %s", driveConversations, len(items), name)
	return more, nil
}
