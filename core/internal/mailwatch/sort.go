package mailwatch

// Sorting the watched private mailboxes (issue #37, stages 2 and 3), as assistant/mail_sort.cpp:
// The assistant moves mails into folders, the archive or the trash when the owner asks (sort_mail), and
// keeps rules the owner gave him (mail_rule), applied to new mails right after he sorted them. The
// rule matches the category of his sorting and optionally words of the sender or subject; the
// firmware decides from that, never from what a mail says. Nothing is ever deleted for good: the
// trash stays the provider's trash, and every move can be undone (sort_mail restore). What rules
// moved goes into the evening digest, and the digest suggests a rule when one kind of mail piles up.
//
// IMAP: SELECT and UID MOVE in the owner's mailbox (mail.MoveOther); the Message-ID finds a mail
// again after it moved. Gmail: labels for folders (users.labels), batchModify for the inbox label,
// trash / untrash; needs the scope gmail.modify.

import (
	"context"
	"errors"
	"log"
	"sort"
	"strconv"
	"strings"
	"time"

	"echo-assistant/internal/mail"
	"echo-assistant/internal/tools"
)

var categories = []string{"personal", "finance", "shopping", "delivery", "appointment", "authority", "work",
	"account", "newsletter", "advertising", "social", "spam", "other"}

func knownCategory(c string) bool { return contains(categories, c) }

// target is where mails go, as people say it and as the provider names it.
type target struct {
	Name   string // "Papierkorb", "Archiv" or the folder / label ("Werbung")
	Where  string // IMAP: the server's folder name; Gmail: "TRASH", "ARCHIVE" or the label id
	Trash  bool
	Create bool // IMAP: the folder is made first
}

func lastSegment(name string) string {
	if i := strings.LastIndexByte(name, '/'); i >= 0 {
		return name[i+1:]
	}
	return name
}

func findFolder(folders []mail.Folder, name string) *mail.Folder {
	for i := range folders {
		if strings.EqualFold(folders[i].Name, name) {
			return &folders[i]
		}
	}
	for i := range folders {
		if strings.EqualFold(lastSegment(folders[i].Name), name) {
			return &folders[i]
		}
	}
	return nil
}

func gmailError(status int, err error) error {
	if status == 403 {
		return errors.New("Error: The assistant may only read this Gmail so far. The owner has to allow changes once: " +
			"tools/google_auth.py again (scope gmail.modify), then the new token into secrets.h.")
	}
	if err != nil {
		return err
	}
	return errors.New("Error: Gmail answered " + strconv.Itoa(status))
}

type gLabels struct {
	Labels []struct {
		ID   string `json:"id"`
		Name string `json:"name"`
		Type string `json:"type"`
	} `json:"labels"`
}

// resolve finds where `to` ("trash", "archive" or a folder name) is in the mailbox.
func (s *Service) resolve(ctx context.Context, box Mailbox, to string) (target, error) {
	var out target
	out.Trash = to == "trash"
	archive := to == "archive"
	if box.Gmail() {
		if out.Trash || archive {
			if out.Trash {
				return target{Name: "Papierkorb", Where: "TRASH", Trash: true}, nil
			}
			return target{Name: "Archiv", Where: "ARCHIVE"}, nil
		}
		var labels gLabels
		if status, err := s.gmailDo(ctx, box, "GET", "labels?fields=labels(id,name,type)", nil, &labels); err != nil {
			return out, gmailError(status, err)
		}
		for _, whole := range []bool{true, false} {
			for _, l := range labels.Labels {
				if (whole && strings.EqualFold(l.Name, to)) || (!whole && strings.EqualFold(lastSegment(l.Name), to)) {
					return target{Name: l.Name, Where: l.ID}, nil
				}
			}
		}
		var created struct {
			ID string `json:"id"`
		}
		status, err := s.gmailDo(ctx, box, "POST", "labels", map[string]any{"name": to,
			"labelListVisibility": "labelShow", "messageListVisibility": "show"}, &created)
		if err != nil {
			return out, gmailError(status, err)
		}
		if created.ID == "" {
			return out, errors.New("Error: Gmail made no label")
		}
		return target{Name: to, Where: created.ID}, nil
	}
	if s.Mail == nil {
		return out, errors.New("Error: no mail service")
	}
	folders, separator, err := s.Mail.FoldersOther(account(box))
	if err != nil {
		return out, err
	}
	if out.Trash || archive {
		for _, f := range folders {
			if f.Use == to {
				return target{Name: f.Name, Where: f.Server, Trash: out.Trash}, nil
			}
		}
		names := []string{"Archiv", "Archive"}
		if out.Trash {
			names = []string{"Papierkorb", "Trash", "Deleted Items", "Deleted Messages", "Gelöschte Elemente", "Gelöschte Objekte", "Gelöscht"}
		}
		for _, name := range names {
			if f := findFolder(folders, name); f != nil {
				return target{Name: f.Name, Where: f.Server, Trash: out.Trash}, nil
			}
		}
		if out.Trash {
			return out, errors.New("Error: " + box.Label + " has no trash folder.")
		}
	}
	name := to
	if archive {
		name = "Archiv"
	}
	if f := findFolder(folders, name); f != nil {
		return target{Name: f.Name, Where: f.Server, Trash: out.Trash}, nil
	}
	// A new folder where the owner keeps theirs: below the inbox if they sort there (or the server
	// keeps every folder there), else at the top.
	belowInbox := false
	for _, f := range folders {
		belowInbox = belowInbox || (separator != "" && strings.HasPrefix(f.Server, "INBOX"+separator))
	}
	server := name
	if separator != "" && separator != "/" {
		server = strings.ReplaceAll(server, "/", separator)
	}
	prefix := ""
	if belowInbox {
		prefix = "INBOX" + separator
	}
	return target{Name: name, Where: prefix + mail.FolderName(server), Create: true}, nil
}

// entryFor is the log entry of a mail of `box`: by its ref, or by "<uid>@<folder>" where the assistant
// moved it. Caller holds the lock.
func (s *Service) entryFor(box Mailbox, person, ref string) *entry {
	for _, e := range s.st.Log {
		if e.Box != box.Label || e.Person != person {
			continue
		}
		if e.Ref == ref || (e.MUID != 0 && ref == strconv.FormatUint(uint64(e.MUID), 10)+"@"+e.Where) {
			return e
		}
	}
	return nil
}

// item is a mail to move, and where it is now.
type item struct {
	ref   string // its ref in the log (or as the search gave it)
	where string // its folder / label now ("" = the inbox)
	imap  mail.Moved
}

func (s *Service) gmailModify(ctx context.Context, box Mailbox, ids []string, add, remove string) (int, error) {
	req := map[string]any{"ids": ids}
	if add != "" {
		req["addLabelIds"] = []string{add}
	}
	if remove != "" {
		req["removeLabelIds"] = []string{remove}
	}
	return s.gmailDo(ctx, box, "POST", "messages/batchModify", req, nil)
}

func isLabel(where string) bool { return where != "" && where != "TRASH" && where != "ARCHIVE" }

// gmailMove moves `items` (all from the same place `from`) of a Gmail account.
func (s *Service) gmailMove(ctx context.Context, box Mailbox, from string, items []item, to target, restore bool) error {
	var ids []string
	for _, it := range items {
		ids = append(ids, it.ref)
	}
	var status int
	var err error
	if (restore && from == "TRASH") || (!restore && to.Where == "TRASH") {
		for _, id := range ids { // no batch for the trash (batchDelete deletes for good: never)
			verb := "/trash"
			if restore {
				verb = "/untrash"
			}
			status, err = s.gmailDo(ctx, box, "POST", "messages/"+id+verb, nil, nil)
			if err != nil && status != 404 {
				break
			}
			status, err = 200, nil
		}
		if err == nil && restore {
			status, err = s.gmailModify(ctx, box, ids, "INBOX", "")
		}
	} else if restore {
		remove := ""
		if isLabel(from) {
			remove = from
		}
		status, err = s.gmailModify(ctx, box, ids, "INBOX", remove)
	} else {
		add := ""
		if isLabel(to.Where) {
			add = to.Where
		}
		status, err = s.gmailModify(ctx, box, ids, add, "INBOX")
		if err == nil && isLabel(from) {
			status, err = s.gmailModify(ctx, box, ids, "", from)
		}
	}
	if err == nil {
		return nil
	}
	return gmailError(status, err)
}

// relocate moves the mails `refs` of `box` to `to`, or back to the inbox (`restore`), and notes
// it in the log. `ruleID`: the rule that did it, 0 = the person asked. Returns how many moved and
// an error text ("" if something moved and nothing failed).
func (s *Service) relocate(ctx context.Context, box Mailbox, person string, refs []string, to target, restore bool, ruleID int) (int, error) {
	byPlace := map[string][]item{} // where they are now -> the mails
	s.mu.Lock()
	for _, ref := range refs {
		it := item{ref: ref}
		if e := s.entryFor(box, person, ref); e != nil && e.Where != "" {
			it.ref, it.where, it.imap.UID, it.imap.MessageID = e.Ref, e.Where, e.MUID, e.MID
		} else if !box.Gmail() {
			if at := strings.IndexByte(ref, '@'); at > 0 {
				it.where = ref[at+1:]
			}
			n, _ := strconv.ParseUint(leadingNumber(ref), 10, 32)
			it.imap.UID = uint32(n)
		}
		if (restore && it.where == "") || (!restore && it.where == to.Where) {
			continue // already there
		}
		if !box.Gmail() && it.imap.UID == 0 && it.imap.MessageID == "" {
			continue
		}
		byPlace[it.where] = append(byPlace[it.where], it)
	}
	s.mu.Unlock()
	if len(byPlace) == 0 {
		return 0, nil
	}
	places := make([]string, 0, len(byPlace))
	for p := range byPlace {
		places = append(places, p)
	}
	sort.Strings(places)
	count := 0
	var failed error
	for _, place := range places {
		items := byPlace[place]
		type done struct {
			it item
			m  mail.Moved
		}
		var moved []done
		if box.Gmail() {
			if err := s.gmailMove(ctx, box, place, items, to, restore); err != nil {
				failed = err
				continue
			}
			for _, it := range items {
				moved = append(moved, done{it, it.imap})
			}
		} else {
			var mails []mail.Moved
			for _, it := range items {
				mails = append(mails, it.imap)
			}
			from := place
			if from == "" {
				from = "INBOX"
			}
			dest := to.Where
			if restore {
				dest = "INBOX"
			}
			result, err := s.Mail.MoveOther(account(box), from, dest, !restore && to.Create, mails)
			if err != nil {
				failed = err
				continue
			}
			for _, m := range result {
				for _, it := range items {
					if (it.imap.UID != 0 && it.imap.UID == m.UID) || (it.imap.UID == 0 && it.imap.MessageID != "" && it.imap.MessageID == m.MessageID) {
						moved = append(moved, done{it, m})
					}
				}
			}
		}
		s.mu.Lock()
		now := time.Now().Unix()
		for _, d := range moved {
			count++
			e := s.entryFor(box, person, d.it.ref)
			if e == nil {
				continue // a search hit the assistant never sorted
			}
			e.Moved, e.Where, e.MUID, e.MovedT, e.Rule, e.Trash = "", "", 0, 0, 0, false
			if restore {
				if !box.Gmail() && d.m.NewUID != 0 {
					e.Ref = strconv.FormatUint(uint64(d.m.NewUID), 10)
				}
				continue
			}
			e.Moved, e.Where = to.Name, to.Where
			e.Trash = to.Trash
			if d.m.MessageID != "" {
				e.MID = d.m.MessageID
			}
			if d.m.NewUID != 0 {
				e.MUID = d.m.NewUID
			}
			e.MovedT = now
			e.Rule = ruleID
		}
		s.save()
		s.mu.Unlock()
	}
	direction, where := "to", to.Name
	if restore {
		direction, where = "back to the inbox from", "folders"
	}
	suffix := ""
	if failed != nil {
		suffix = " (" + failed.Error() + ")"
	}
	log.Printf("[MailSort] %s: %d mails %s %s%s", box.Label, count, direction, where, suffix)
	if count > 0 {
		return count, nil
	}
	return 0, failed
}

// leadingNumber: "123@Folder" -> "123".
func leadingNumber(ref string) string {
	if at := strings.IndexByte(ref, '@'); at >= 0 {
		return ref[:at]
	}
	return ref
}

// --- Rules -----------------------------------------------------------------------------------

func containsFold(text, word string) bool {
	return word == "" || strings.Contains(strings.ToLower(text), strings.ToLower(word))
}

// matches: the rule applies to the log entry of a mail of `box`.
func (r *rule) matches(e *entry, box Mailbox) bool {
	return (r.Box == "" || strings.EqualFold(r.Box, box.Label)) &&
		(r.Category == "" || r.Category == e.Category) &&
		containsFold(e.From, r.From) && containsFold(e.Subject, r.Subject)
}

func targetText(t string) string {
	switch t {
	case "trash":
		return "the trash"
	case "archive":
		return "the archive"
	}
	return "folder " + t
}

func (r *rule) text() string {
	var conditions []string
	if r.Category != "" {
		conditions = append(conditions, "category "+r.Category)
	}
	if r.From != "" {
		conditions = append(conditions, `sender contains "`+r.From+`"`)
	}
	if r.Subject != "" {
		conditions = append(conditions, `subject contains "`+r.Subject+`"`)
	}
	box := r.Box
	if box == "" {
		box = "all mailboxes"
	}
	return "rule " + strconv.Itoa(r.ID) + ": " + box + ", " + strings.Join(conditions, ", ") + " -> " + targetText(r.Target) +
		" (moved so far: " + strconv.Itoa(r.Hits) + ")"
}

func (s *Service) ruleTool(args map[string]any, ctx tools.Context) string {
	person := s.reader(ctx)
	action := tools.String(args, "action")
	label, category, from, subject := tools.String(args, "mailbox"), tools.String(args, "category"),
		tools.String(args, "from"), tools.String(args, "subject")
	var boxes []Mailbox
	if action == "add" {
		var errText string
		boxes, errText = s.pick(person, label)
		if len(boxes) == 0 {
			return errText
		}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	switch action {
	case "list":
		var text string
		for _, r := range s.st.Rules {
			if r.Person == person {
				text += "- " + r.text() + "\n"
			}
		}
		if text == "" {
			return "No mail rules for " + s.People.Name(person) + "."
		}
		return "Mail rules of " + s.People.Name(person) + ":\n" + text
	case "delete":
		id, _ := tools.Int(args, "id")
		for i, r := range s.st.Rules {
			if r.ID == id && r.Person == person {
				text := r.text()
				s.st.Rules = append(s.st.Rules[:i], s.st.Rules[i+1:]...)
				s.save()
				return "Deleted " + text + ". Mails it moved stay where they are (sort_mail restore brings them back)."
			}
		}
		return "Error: no rule " + strconv.Itoa(id) + " of " + s.People.Name(person) + " (mail_rule list)."
	case "add":
	default:
		return "Error: action is add, list or delete."
	}
	to := tools.String(args, "target")
	switch strings.ToLower(to) {
	case "trash", "papierkorb":
		to = "trash"
	case "archive", "archiv":
		to = "archive"
	}
	if to == "" || len(to) > 60 || strings.EqualFold(to, "inbox") {
		return `Error: target is a folder name, "archive" or "trash".`
	}
	if category != "" && !knownCategory(category) {
		return "Error: unknown category " + category + "."
	}
	if category == "" && from == "" && subject == "" {
		return "Error: a rule needs a category, a sender or a subject word."
	}
	var found *rule
	own := 0
	for _, r := range s.st.Rules {
		if r.Person != person {
			continue
		}
		own++
		if strings.EqualFold(label, r.Box) && category == r.Category && from == r.From && subject == r.Subject {
			found = r // the same conditions: the new target replaces the old one
		}
	}
	if found == nil && own >= lim("MAILSORT_RULE_MAX", 30) {
		return "Error: " + strconv.Itoa(lim("MAILSORT_RULE_MAX", 30)) + " rules already; delete one first."
	}
	if found == nil {
		s.st.RuleNext++
		found = &rule{ID: s.st.RuleNext, Person: person}
		s.st.Rules = append(s.st.Rules, found)
	}
	found.Box = ""
	if label != "" {
		found.Box = boxes[0].Label
	}
	found.Category, found.From, found.Subject, found.Target = category, from, subject, to
	found.Where = ""
	found.T = time.Now().Unix()
	// How many mails of the index already match, still in the inbox: sort_mail can move them now.
	waiting := 0
	for _, e := range s.st.Log {
		if e.Person != person || e.Where != "" {
			continue
		}
		for _, b := range boxes {
			if b.Label == e.Box && found.matches(e, b) {
				waiting++
			}
		}
	}
	s.save()
	text := "Saved " + found.text() + ". From now on the assistant moves new mails that match right after sorting them"
	if to == "trash" {
		text += " (the provider's trash: recoverable, nothing deleted for good)"
	}
	text += ". "
	if waiting > 0 {
		return text + strconv.Itoa(waiting) + " mails the assistant already filed match it and are still in the inbox; sort_mail moves them if the user wants (ask)."
	}
	return text + "No mail the assistant filed so far matches it."
}

// --- sort_mail ---------------------------------------------------------------------------------

func (s *Service) folderList(ctx context.Context, box Mailbox) string {
	text := "# " + box.Label + "\n"
	if box.Gmail() {
		var labels gLabels
		if status, err := s.gmailDo(ctx, box, "GET", "labels?fields=labels(id,name,type)", nil, &labels); err != nil {
			return text + gmailError(status, err).Error() + "\n"
		}
		var own []string
		for _, l := range labels.Labels {
			if l.Type == "user" {
				own = append(own, l.Name)
			}
		}
		list := "(none of their own)"
		if len(own) > 0 {
			list = strings.Join(own, ", ")
		}
		return text + "Labels: " + list + ". Besides: inbox, archive (all mail), trash.\n"
	}
	if s.Mail == nil {
		return text + "Error: no mail service\n"
	}
	folders, _, err := s.Mail.FoldersOther(account(box))
	if err != nil {
		return text + err.Error() + "\n"
	}
	for _, f := range folders {
		text += "- " + f.Name
		if f.Use != "" {
			text += " (" + f.Use + ")"
		}
		text += "\n"
	}
	return text
}

func stringList(args map[string]any, name string) []string {
	var out []string
	if list, ok := args[name].([]any); ok {
		for _, v := range list {
			if str, ok := v.(string); ok && str != "" {
				out = append(out, str)
			}
		}
	}
	return out
}

func (s *Service) sortTool(args map[string]any, tctx tools.Context) string {
	ctx, cancel := callContext()
	defer cancel()
	person := s.reader(tctx)
	action := tools.String(args, "action")
	boxes, errText := s.pick(person, tools.String(args, "mailbox"))
	if len(boxes) == 0 {
		return errText
	}
	if action == "folders" {
		text := "Folders of " + s.People.Name(person) + "'s mailboxes:\n"
		for _, b := range boxes {
			text += s.folderList(ctx, b)
		}
		return text
	}
	if action == "file_older" {
		n, ok := tools.Int(args, "days")
		if !ok {
			n = 90
		}
		n = max(1, min(n, 365))
		var text string
		for _, b := range boxes {
			text += s.fileOlder(ctx, b, n) + "\n"
		}
		return text
	}
	if len(boxes) != 1 {
		return "Error: name the mailbox (mail_news lists them)."
	}
	box := boxes[0]
	restore := action == "restore"
	to := tools.String(args, "folder")
	if action == "trash" || action == "archive" {
		to = action
	}
	if action == "move" && (to == "" || len(to) > 60 || strings.EqualFold(to, "inbox")) {
		return "Error: move needs the folder's name."
	}
	if !restore && action != "move" && action != "trash" && action != "archive" {
		return "Error: action is folders, move, archive, trash, restore or file_older."
	}
	refs := stringList(args, "refs")
	category := tools.String(args, "category")
	if len(refs) == 0 {
		// Every filed mail of the category in the last days (restore: what the assistant moved, into the
		// folder named, if any).
		if !restore && !knownCategory(category) {
			return "Error: give refs or a category."
		}
		n, ok := tools.Int(args, "days")
		if !ok {
			n = 7
		}
		n = max(1, min(n, lim("MAILWATCH_LOG_DAYS", 60)))
		since := time.Now().Add(-days(n)).Unix()
		s.mu.Lock()
		for _, e := range s.st.Log {
			if e.Person != person || e.Box != box.Label {
				continue
			}
			if category != "" && category != "all" && category != e.Category {
				continue
			}
			if restore {
				if e.Where == "" || e.MovedT < since {
					continue
				}
				if to != "" && !strings.EqualFold(to, e.Moved) && !strings.EqualFold(to, lastSegment(e.Moved)) {
					continue
				}
			} else if e.Where != "" || e.T < since {
				continue
			}
			refs = append(refs, e.Ref)
		}
		s.mu.Unlock()
		if len(refs) == 0 {
			if restore {
				return "No mail the assistant moved matches that."
			}
			return "No filed mail in the inbox matches that."
		}
	}
	if m := lim("MAILSORT_MAX_MAILS", 200); len(refs) > m {
		refs = refs[:m]
	}
	var dest target
	if !restore {
		var err error
		if dest, err = s.resolve(ctx, box, to); err != nil {
			return err.Error()
		}
	}
	count, err := s.relocate(ctx, box, person, refs, dest, restore, 0)
	if count == 0 {
		if err != nil {
			return err.Error()
		}
		return "Nothing to move: they are there already."
	}
	if restore {
		return "Back in the inbox of " + box.Label + ": " + strconv.Itoa(count) + " mails."
	}
	where := dest.Name
	if dest.Trash {
		where = "the trash (recoverable)"
	}
	text := "Moved " + strconv.Itoa(count) + " mails of " + box.Label + " to " + where
	if dest.Create {
		text += " (folder created)"
	}
	text += "."
	if err != nil {
		text += " Some failed: " + err.Error()
	}
	return text
}

// applyRules moves what matches the person's rules among the mails of `box` just logged (`refs`,
// not the index's old ones) right away. Between conversations (the check), without the lock held.
func (s *Service) applyRules(ctx context.Context, box Mailbox, person string, refs []string) {
	type job struct {
		rule   int
		target string
		refs   []string
	}
	var jobs []*job
	s.mu.Lock()
	for _, ref := range refs {
		e := s.entryFor(box, person, ref)
		if e == nil {
			continue
		}
		for _, r := range s.st.Rules {
			if r.Person != person || !r.matches(e, box) {
				continue
			}
			var found *job
			for _, j := range jobs {
				if j.rule == r.ID {
					found = j
				}
			}
			if found == nil {
				jobs = append(jobs, &job{r.ID, r.Target, []string{ref}})
			} else {
				found.refs = append(found.refs, ref)
			}
			break // the first rule that matches
		}
	}
	s.mu.Unlock()
	for _, j := range jobs {
		dest, err := s.resolve(ctx, box, j.target)
		count := 0
		if err == nil {
			count, err = s.relocate(ctx, box, person, j.refs, dest, false, j.rule)
		}
		if err != nil {
			log.Printf("[MailSort] rule %d: %v", j.rule, err)
		}
		if count == 0 {
			continue
		}
		s.mu.Lock()
		for _, r := range s.st.Rules {
			if r.ID == j.rule {
				r.Hits += count
				r.Where = dest.Where
				if dest.Trash {
					r.Trash = true
				}
			}
		}
		// For the digest: a count, since the log drops mails older than MAILWATCH_LOG_DAYS (older
		// mails filed on request) before the evening.
		if s.st.RuleMoves[person] == nil {
			s.st.RuleMoves[person] = map[string]int{}
		}
		s.st.RuleMoves[person][dest.Name] += count
		s.save()
		s.mu.Unlock()
	}
}

// movedReport is for the digest: what Jarvis moved by rule since the last digest ("" if
// nothing). The digest clears the counts once it went out. Caller holds the lock.
func (s *Service) movedReport(person string) string {
	moves := s.st.RuleMoves[person]
	folders := make([]string, 0, len(moves))
	for f := range moves {
		folders = append(folders, f)
	}
	sort.Strings(folders)
	var parts []string
	for _, f := range folders {
		parts = append(parts, strconv.Itoa(moves[f])+" → "+f)
	}
	if len(parts) == 0 {
		return ""
	}
	return "Nach deinen Regeln einsortiert: " + strings.Join(parts, ", ") + "."
}

// ruleSuggestion suggests a rule when a kind of mail piles up ("" if none; noted, so it comes at
// most every MAILSORT_SUGGEST_DAYS). Caller holds the lock.
func (s *Service) ruleSuggestion(person string, now time.Time) string {
	kinds := []struct{ category, mails, folder string }{
		{"advertising", "Werbemails", "Werbung"},
		{"newsletter", "Newsletter", "Newsletter"},
		{"social", "Benachrichtigungen von sozialen Netzwerken", "Social"},
	}
	since := now.Add(-days(7)).Unix()
	for _, k := range kinds {
		covered := false
		for _, r := range s.st.Rules {
			covered = covered || (r.Person == person && r.Category == k.category)
		}
		last := s.st.Suggested[person][k.category]
		if covered || now.Unix()-last < int64(lim("MAILSORT_SUGGEST_DAYS", 14))*86400 {
			continue
		}
		count := 0
		for _, e := range s.st.Log {
			if e.Person == person && !e.Old && e.Where == "" && e.T >= since && e.Category == k.category {
				count++
			}
		}
		if count < lim("MAILSORT_SUGGEST_MIN", 10) {
			continue
		}
		if s.st.Suggested[person] == nil {
			s.st.Suggested[person] = map[string]int64{}
		}
		s.st.Suggested[person][k.category] = now.Unix()
		return "In den letzten 7 Tagen kamen " + strconv.Itoa(count) + " " + k.mails +
			". Soll ich so etwas künftig automatisch in einen Ordner „" + k.folder + "“ verschieben?"
	}
	return ""
}

// sortFolders are the folders besides the inbox where the assistant put mails of `box` (IMAP server
// names, no trash): the search looks there too. Caller holds the lock.
func (s *Service) sortFolders(box Mailbox, person string) []string {
	var folders []string
	add := func(where string) {
		if where != "" && !contains(folders, where) {
			folders = append(folders, where)
		}
	}
	for _, r := range s.st.Rules {
		if r.Person == person && !r.Trash && (r.Box == "" || strings.EqualFold(r.Box, box.Label)) {
			add(r.Where)
		}
	}
	for _, e := range s.st.Log {
		if e.Box == box.Label && e.Person == person && !e.Trash {
			add(e.Where)
		}
	}
	return folders
}

// movedTo says where an IMAP mail of the log is now: its folder (server name) and UID there.
// False if it was not moved (still in the inbox under `ref`). Caller holds the lock.
func (s *Service) movedTo(box Mailbox, person, ref string) (string, uint32, bool) {
	e := s.entryFor(box, person, ref)
	if e == nil || e.Where == "" {
		return "", 0, false
	}
	return e.Where, e.MUID, true
}
