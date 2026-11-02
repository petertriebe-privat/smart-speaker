package mail

import (
	"fmt"
	"log"
	"strconv"
	"strings"
	"time"

	"echo-assistant/internal/prompts"
	"echo-assistant/internal/textutil"
)

// The assistant's notes (memory archive, backups) are mails in folders of its own mailbox, created on
// first use. Only the assistant puts mails there (IMAP APPEND); anyone can send to the inbox, so notes
// never live there.

// noteEnd closes every note; the M5 needed it because ReadyMail decoded the tail of the FETCH
// response into the body. It stays, so notes written by either device read the same.
const noteEnd = "\n-- Ende der Notiz --"

// jsonMarker separates the readable summary of a backup mail from its JSON.
const jsonMarker = "\n---JSON---\n"

func uitoa(n uint32) string { return strconv.FormatUint(uint64(n), 10) }

// ReadNotes returns the newest `count` notes per folder matching the IMAP search `criteria`
// (e.g. `SUBJECT "[mosel]"`), newest first, as plain text of at most `limit` bytes (0 =
// NOTE_READ_LIMIT); one login for all folders. The error is set when the mailbox was unreachable
// (notes read before that are returned too). Blocks for seconds.
func (s *Service) ReadNotes(folders []string, criteria string, count, limit int) ([]string, error) {
	if !s.Configured() {
		return nil, fail("no mailbox configured")
	}
	if limit <= 0 {
		limit = s.noteLimit
	}
	unlock := s.lock()
	defer unlock()
	// One login for all folders: each TLS handshake costs ~1 s and memory.
	c, err := s.login(nil)
	if err != nil {
		return nil, err
	}
	defer c.logout()
	var notes []string
	for _, folder := range folders {
		if err := c.ensureFolder(folder, true); err != nil {
			return notes, err
		}
		uids, _, err := c.search(criteria)
		if err != nil {
			return notes, fail("mailbox not reachable (%v)", err)
		}
		want := len(notes) + count
		for i := len(uids) - 1; i >= 0 && len(notes) < want; i-- {
			p, err := c.fetchParsed(uids[i], parseOptions{}, 0)
			if err != nil {
				return notes, fail("mailbox not reachable (%v)", err)
			}
			if p == nil {
				continue
			}
			text := p.Plain
			if end := strings.Index(text, noteEnd); end >= 0 {
				text = text[:end]
			}
			notes = append(notes, textutil.Prefix(text, limit))
		}
	}
	return notes, nil
}

// StoreNote stores a note, then deletes the older notes matching `replaceCriteria` ("" = keep
// all). Only after the new one is in: an update never loses the note.
func (s *Service) StoreNote(folder, subject, text, replaceCriteria string) error {
	if !s.Configured() {
		return fail("no mailbox configured")
	}
	unlock := s.lock()
	defer unlock()
	c, err := s.login(nil)
	if err != nil {
		return err
	}
	defer c.logout()
	if err := c.ensureFolder(folder, false); err != nil {
		return err
	}
	var old []uint32
	if replaceCriteria != "" {
		old, _, _ = c.search(replaceCriteria)
	}
	note := outMessage{From: s.fromHeader(), To: s.fromHeader(), Subject: subject, Text: text + noteEnd + "\n",
		Base64Text: true, Date: time.Now()} // base64: long transcript lines, umlauts
	if err := c.appendMessage(folder, "\\Seen", note.bytes()); err != nil {
		return fail("storing failed (%s)", oneLine(err.Error(), nil))
	}
	if len(old) > 0 {
		if _, err := c.do("UID STORE " + uidList(old) + " +FLAGS (\\Deleted)"); err == nil {
			_, err = c.do("EXPUNGE")
			if err != nil {
				log.Printf("[Mail] old notes in %s not deleted (%s)", folder, c.status)
			}
		} else {
			log.Printf("[Mail] old notes in %s not deleted (%s)", folder, c.status)
		}
	}
	return nil
}

// ClearNotes deletes all notes in `folder`.
func (s *Service) ClearNotes(folder string) error {
	if !s.Configured() {
		return fail("no mailbox configured")
	}
	unlock := s.lock()
	defer unlock()
	c, err := s.login(nil)
	if err != nil {
		return err
	}
	defer c.logout()
	if err := c.ensureFolder(folder, false); err != nil {
		return err
	}
	if uids, _, _ := c.search("ALL"); len(uids) > 0 {
		c.command("UID STORE 1:* +FLAGS (\\Deleted)")
		c.command("EXPUNGE")
	}
	return nil
}

// --- Backups (backup.Target) --------------------------------------------------------------------

// BackupTarget stores JSON backups (people register, scheduled tasks, core memory) as mails in
// MEMORY_FOLDER_CORE: a readable summary, then the document. Put it into backup.Targets.
type BackupTarget struct{ s *Service }

// Backup returns the mailbox as a backup.Target.
func (s *Service) Backup() *BackupTarget { return &BackupTarget{s} }

func (b *BackupTarget) Available() bool { return b.s.Configured() }

// Store saves doc under subject, replacing the older backup with the same subject (name is for
// Drive and unused here).
func (b *BackupTarget) Store(name, subject, summary string, doc []byte) error {
	return b.s.StoreNote(prompts.Text("MEMORY_FOLDER_CORE"), subject, summary+jsonMarker+string(doc), SubjectCriteria(subject))
}

// Load returns the newest backup with subject: nil without error if there is none.
func (b *BackupTarget) Load(name, subject string) ([]byte, error) {
	limit := prompts.Int("MEMORY_BACKUP_CHARS", 60000) + 4000 // M5: NOTE_READ_LIMIT; a larger core must not be cut
	notes, err := b.s.ReadNotes([]string{prompts.Text("MEMORY_FOLDER_CORE")}, SubjectCriteria(subject), 1, limit)
	if err != nil {
		return nil, err
	}
	if len(notes) == 0 {
		return nil, nil
	}
	return backupJSON(notes[0]), nil
}

// backupJSON: what follows the marker, else from the first '{' (backups of the memory core have
// no summary); nil if there is no JSON.
func backupJSON(note string) []byte {
	if at := strings.Index(note, jsonMarker); at >= 0 {
		return []byte(note[at+len(jsonMarker):])
	}
	if start := strings.IndexByte(note, '{'); start >= 0 {
		return []byte(note[start:])
	}
	return nil
}

// --- Memory archive (memory.Archive) --------------------------------------------------------------

// ArchiveTier is the mailbox tier of the memory archive (the non-Drive path of the M5's
// memory_archive.cpp): dossiers in MEMORY_FOLDER_TOPICS, transcripts in MEMORY_FOLDER_CONVERSATIONS.
type ArchiveTier struct{ s *Service }

// Archive returns the mailbox as a memory.Archive.
func (s *Service) Archive() *ArchiveTier { return &ArchiveTier{s} }

func (a *ArchiveTier) Available() bool { return a.s.Configured() }

func topicsFolder() string        { return prompts.Text("MEMORY_FOLDER_TOPICS") }
func conversationsFolder() string { return prompts.Text("MEMORY_FOLDER_CONVERSATIONS") }

// first reads the newest note matching criteria; "" if there is none.
func (a *ArchiveTier) first(folder, criteria string) (string, error) {
	notes, err := a.s.ReadNotes([]string{folder}, criteria, 1, 0)
	if len(notes) == 0 {
		return "", err
	}
	return notes[0], err
}

// topicCriteria finds a dossier by its key (subject "Thema [key]: title"), or by title for topics
// without a key.
func topicCriteria(key, title string) string {
	if key != "" {
		return SubjectCriteria("[" + key + "]")
	}
	return SubjectCriteria(title)
}

// LoadTopic returns the dossier of a topic ("" if there is none).
func (a *ArchiveTier) LoadTopic(key, title string) (string, error) {
	return a.first(topicsFolder(), topicCriteria(key, title))
}

// StoreTopic replaces the dossier of the topic `key`.
func (a *ArchiveTier) StoreTopic(key, subject, text string) error {
	replace := ""
	if key != "" {
		replace = SubjectCriteria("[" + key + "]")
	}
	return a.s.StoreNote(topicsFolder(), subject, text, replace)
}

// StoreConversation writes a transcript once (subject "Gespräch YYYY-MM-DD HH:MM – title").
func (a *ArchiveTier) StoreConversation(at, subject, text string) error {
	return a.s.StoreNote(conversationsFolder(), subject, text, "")
}

// LoadConversation returns the transcript of the conversation at `at` ("" if there is none).
func (a *ArchiveTier) LoadConversation(at string) (string, error) {
	return a.first(conversationsFolder(), SubjectCriteria(at))
}

// Search: full text for the first two terms, at most two notes (dossiers, transcripts).
func (a *ArchiveTier) Search(terms []string) ([]string, error) {
	if len(terms) == 0 {
		return nil, nil
	}
	criteria := fmt.Sprintf(`TEXT "%s"`, searchString(terms[0]))
	if len(terms) > 1 {
		criteria = "OR " + criteria + fmt.Sprintf(` TEXT "%s"`, searchString(terms[1]))
	}
	return a.s.ReadNotes([]string{topicsFolder(), conversationsFolder()}, criteria, 2, 0)
}
