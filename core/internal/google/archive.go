package google

import (
	"log"
	"strings"
	"time"

	"echo-assistant/internal/vault"
)

// Drive as the backup target (package backup) and as the memory's archive tier (memory.Archive):
// the Drive parts of devices/atoms3r/src/assistant/backup.cpp and memory_archive.cpp.

const (
	driveTopics        = "Themen" // Drive subfolders of the archive
	driveConversations = "Gespräche"
	legacyMIME         = "text/plain" // notes until 11/2026 (vault.Convert)
)

// BackupTarget stores backups as files in the folder the assistant. It implements backup.Target.
type BackupTarget struct{ d *Drive }

// BackupTarget: the Drive backup target of this account.
func (a *Account) BackupTarget() *BackupTarget { return &BackupTarget{a.Drive()} }

func (b *BackupTarget) Available() bool { return b.d.Configured() }

// Store saves doc (JSON) as the file name; subject and summary are for the mail target.
func (b *BackupTarget) Store(name, subject, summary string, doc []byte) error {
	return b.d.Store(name, "application/json", doc)
}

// Load returns the backup, nil without error if there is none (the mail target is asked then).
func (b *BackupTarget) Load(name, subject string) ([]byte, error) {
	data, found, err := b.d.Load(name)
	if err != nil || !found {
		return nil, err
	}
	return data, nil
}

// Due: this device neither stored nor found the backup name yet (backup::due).
func (b *BackupTarget) Due(name string) bool { return b.Available() && !b.d.Known(name) }

// Archive keeps the dossiers (The assistant/Themen) and conversations (The assistant/Gespräche) as text files,
// found again by appProperties topic / at. It implements memory.Archive.
type Archive struct{ d *Drive }

// Archive: the Drive archive of this account.
func (a *Account) Archive() *Archive { return &Archive{a.Drive()} }

func (r *Archive) Available() bool { return r.d.Configured() }

func propertyQuery(property, value string) string {
	return "appProperties has { key='" + property + "' and value=" + Quoted(value) + " }"
}

// noteText: the text of a file in Drive.
func (r *Archive) noteText(id string) (string, error) {
	data, err := r.d.Download(id)
	return string(data), err
}

// findNote: the first note in the archive: a dossier (topic key, or the title for topics without
// one) or a conversation (its time). Empty if there is none.
func (r *Archive) findNote(topic bool, key, title string) (string, error) {
	property, folder := "at", driveConversations
	if topic {
		property, folder = "topic", driveTopics
	}
	if key == "" && title == "" {
		return "", nil // a dossier without key is only written, never looked up (as sendNoteToDrive)
	}
	query := "name contains " + Quoted(title)
	if key != "" {
		query = propertyQuery(property, key)
	}
	found, err := r.d.List(folder, query, 1)
	if err != nil || len(found) == 0 {
		return "", err
	}
	return r.noteText(found[0].ID)
}

// LoadTopic: the dossier of a topic by its key (by title for topics without one).
func (r *Archive) LoadTopic(key, title string) (string, error) { return r.findNote(true, key, title) }

// LoadConversation: one earlier conversation with its transcript, by its time.
func (r *Archive) LoadConversation(at string) (string, error) { return r.findNote(false, at, "") }

// StoreTopic replaces a topic's dossier (found by its key); a new one is made when there is none.
// text is the whole dossier, history included. The file is named after the topic's title
// (renamed when the title changed, so wikilinks reach it).
func (r *Archive) StoreTopic(key, subject, text string) error {
	if key == "" { // "Thema [key]: title"
		if open, closing := strings.IndexByte(subject, '['), strings.IndexByte(subject, ']'); open >= 0 && closing > open {
			key = subject[open+1 : closing]
		}
	}
	name := vault.NameFromSubject(subject) + vault.Ext
	var found []Entry
	if key != "" {
		var err error
		if found, err = r.d.List(driveTopics, propertyQuery("topic", key), 1); err != nil {
			log.Printf("[Memory] Drive %s: %v", driveTopics, err)
			return err
		}
	}
	started := time.Now()
	var err error
	if len(found) > 0 {
		if err = r.d.Replace(found[0].ID, vault.MIME, []byte(text)); err == nil && (found[0].Name != name || found[0].Type != vault.MIME) {
			err = r.d.Rename(found[0].ID, name, vault.MIME)
		}
	} else {
		var props map[string]string
		if key != "" {
			props = map[string]string{"topic": key}
		}
		err = r.d.Create(driveTopics, name, vault.MIME, []byte(text), props)
	}
	logStored(driveTopics, subject, err, started)
	return err
}

// StoreConversation writes a conversation once (subject "Gespräch YYYY-MM-DD HH:MM – title"); one
// found by its name is left alone: an earlier try got through.
func (r *Archive) StoreConversation(at, subject, text string) error {
	if at == "" { // "Gespräch 2026-09-30 18:38 – ..."
		if space := strings.IndexByte(subject, ' '); space > 0 {
			at = sub(subject, space+1, space+17)
		}
	}
	// By name: two conversations can start in the same minute.
	name := vault.NameFromSubject(subject) + vault.Ext
	found, err := r.d.List(driveConversations, "name="+Quoted(name), 1)
	if err != nil {
		log.Printf("[Memory] Drive %s: %v", driveConversations, err)
		return err
	}
	if len(found) > 0 {
		return nil
	}
	started := time.Now()
	var props map[string]string
	if at != "" {
		props = map[string]string{"at": at}
	}
	err = r.d.Create(driveConversations, name, vault.MIME, []byte(text), props)
	logStored(driveConversations, subject, err, started)
	return err
}

func logStored(folder, subject string, err error, started time.Time) {
	result := "stored"
	if err != nil {
		result = err.Error()
	}
	log.Printf("[Memory] Drive %s: %s -> %s (%d ms)", folder, subject, result, time.Since(started).Milliseconds())
}

// noteTypes: the MIME types of notes: Markdown (as Drive names it for files from Obsidian), and
// text (the old format and the household's own).
const noteTypes = "(mimeType='text/markdown' or mimeType='text/x-markdown' or mimeType='text/plain')"

// Search: full text over the notes in the assistant's folder and the folders in it (dossiers,
// transcripts, and with the scope drive the household's own notes) for the first two terms; at
// most two notes.
func (r *Archive) Search(terms []string) ([]string, error) {
	if len(terms) == 0 {
		return nil, nil
	}
	query := "fullText contains " + Quoted(terms[0])
	if len(terms) > 1 {
		query += " or fullText contains " + Quoted(terms[1])
	}
	folders, err := r.d.Folders()
	if err != nil {
		return nil, err
	}
	var found []Entry
	for _, parents := range InParents(folders) {
		more, err := r.d.List("", noteTypes+" and ("+query+") and "+parents, 2)
		if err != nil {
			return nil, err
		}
		found = append(found, more...)
	}
	found = found[:min(len(found), 2)]
	return r.texts(found)
}

func (r *Archive) texts(found []Entry) ([]string, error) {
	var notes []string
	var lastErr error
	for _, file := range found {
		text, err := r.noteText(file.ID)
		if err != nil {
			lastErr = err
		} else if text != "" {
			notes = append(notes, text)
		}
	}
	return notes, lastErr
}

// Tidy keeps the archive an Obsidian vault: it converts up to limit notes of the old text format
// in Themen and Gespräche (and any other .txt there) into Markdown with their new name (package
// vault), and marks the assistant's folder. more: notes are left for the next round. Only the
// primary device calls it (memory.ArchiveWritable).
func (r *Archive) Tidy(limit int) (more bool, err error) {
	if err := r.d.MarkRoot(); err != nil {
		return false, err
	}
	converted := 0
	for _, folder := range []string{driveTopics, driveConversations} {
		found, err := r.d.List(folder, "mimeType='"+legacyMIME+"'", limit+1)
		if err != nil {
			return false, err
		}
		for _, file := range found {
			if converted == limit {
				return true, nil
			}
			data, err := r.d.Download(file.ID)
			if err != nil {
				return false, err
			}
			name, markdown, ok := vault.Convert(file.Name, string(data))
			if ok {
				err = r.d.Replace(file.ID, vault.MIME, []byte(markdown))
			} else { // not the assistant's format: the text stays, as a note Obsidian shows
				name = vault.FileName(strings.TrimSuffix(file.Name, ".txt"))
			}
			if err == nil {
				err = r.d.Rename(file.ID, name+vault.Ext, vault.MIME)
			}
			if err != nil {
				return false, err
			}
			log.Printf("[Drive] %s/%s -> %s%s", folder, file.Name, name, vault.Ext)
			converted++
		}
	}
	return false, nil
}
