package mail

import (
	"context"
	"log"
	"net/mail"
	"strings"
	"time"

	"echo-assistant/internal/prompts"
	"echo-assistant/internal/textutil"
)

// Commands by mail: the household members' mails to the assistant are carried out by the agent and
// answered. The inbox is the command stack: unread = waiting, read = taken (fetching marks it read
// before the agent starts), read + \Answered = done and replied. A mail taken but never answered
// (power loss mid-task) is not run again: printing or mailing twice is worse.

// Command is a mail from a verified household member.
type Command struct {
	UID     uint32
	Subject string
	Text    string // the body, at most MAIL_READ_LIMIT characters
	Sender  string // the sender's address, lower case
	Person  string // the person's id (ByEmail)
}

// Task is the task text for the agent, as the M5 builds it; name is the person's name.
func (c Command) Task(name string) string {
	return "Task by e-mail from " + name + " (" + c.Sender + ", sender verified).\nSubject: " + c.Subject + "\n\n" + c.Text
}

// verifiedSender accepts only mails that the provider itself verified: its Authentication-Results
// header (named authserv) is the topmost one (a sender can add fake ones, but only below it) and
// must report DMARC pass for the sender's domain. The From address must be one of `addresses`
// (lower case). Returns the sender's address or "". Without authserv no mail is trusted.
func verifiedSender(h mail.Header, addresses []string, authserv string) string {
	if authserv == "" {
		return ""
	}
	from := strings.ToLower(h.Get("From"))
	results := strings.ToLower(h.Get("Authentication-Results"))
	authserv = strings.ToLower(authserv)
	for _, address := range addresses {
		domain := address[strings.IndexByte(address, '@')+1:]
		if strings.Contains(from, address) && strings.HasPrefix(results, authserv+";") &&
			strings.Contains(results, "dmarc=pass") && strings.Contains(results, `header.from="`+domain+`"`) {
			return address
		}
	}
	return ""
}

// fromCriteria: IMAP search for mails from any of `addresses`; OR takes two keys, so it nests.
func fromCriteria(addresses []string) string {
	var criteria string
	for i, a := range addresses {
		key := `FROM "` + searchString(a) + `"`
		if i+1 < len(addresses) {
			criteria += "OR " + key + " "
		} else {
			criteria += key
		}
	}
	return criteria
}

// memberAddresses are the addresses of the people who may command the assistant, lower case.
func (s *Service) memberAddresses() []string {
	if s.Senders == nil {
		return nil
	}
	var out []string
	for _, a := range s.Senders() {
		if a = strings.ToLower(strings.TrimSpace(a)); strings.Index(a, "@") > 0 {
			out = append(out, a)
		}
	}
	return out
}

// fetchCommands takes the unread mails of verified household members (marking them read).
// `rejected` holds the UIDs of unverified mails from a person's address: checked once.
func (s *Service) fetchCommands(rejected map[uint32]bool) []Command {
	addresses := s.memberAddresses()
	if len(addresses) == 0 {
		return nil
	}
	unlock := s.lock()
	defer unlock()
	c, err := s.openFolder("INBOX", false)
	if err != nil {
		log.Printf("[Mail] check: %v", err)
		return nil
	}
	defer c.logout()
	uids, _, err := c.search("UNSEEN " + fromCriteria(addresses))
	if err != nil {
		log.Printf("[Mail] check: %v", err)
		return nil
	}
	var commands []Command
	for _, uid := range uids {
		if rejected[uid] {
			continue
		}
		headers, err := c.fetchHeaders([]uint32{uid}, "FROM SUBJECT AUTHENTICATION-RESULTS")
		if err != nil {
			log.Printf("[Mail] check: %v", err)
			return commands
		}
		h := parseHeaderBlock(headers[uid])
		sender := verifiedSender(h, addresses, s.authservID)
		if sender == "" {
			log.Printf("[Mail] mail %d from a person NOT verified, not carried out:\n%s", uid, headers[uid])
			rejected[uid] = true
			continue
		}
		p, err := c.fetchParsed(uid, parseOptions{textLimit: s.readLimit * 8}, bodyFetchLimit)
		if err != nil || p == nil {
			continue
		}
		c.markSeen(uid) // taken
		person := ""
		if s.ByEmail != nil {
			person = s.ByEmail(sender)
		}
		commands = append(commands, Command{UID: uid, Subject: decodeHeader(h.Get("Subject")),
			Text: textutil.Repair(textutil.Prefix(p.Text(), s.readLimit)), Sender: sender, Person: person})
	}
	return commands
}

func (s *Service) markAnswered(uid uint32) {
	unlock := s.lock()
	defer unlock()
	c, err := s.openFolder("INBOX", false)
	if err != nil {
		log.Printf("[Mail] marking %d answered: %v", uid, err)
		return
	}
	c.command("UID STORE " + uitoa(uid) + " +FLAGS (\\Answered)")
	c.logout()
}

// runCommands fetches the waiting commands and carries them out.
func (s *Service) runCommands(rejected map[uint32]bool) {
	for _, command := range s.fetchCommands(rejected) {
		log.Printf("[Mail task] %s\n%s", command.Subject, textutil.Prefix(command.Text, 300))
		started := time.Now()
		s.cmdMu.Lock()
		s.replyTo, s.replied = command.Sender, false
		s.cmdMu.Unlock()
		answer := s.OnCommand(command)
		log.Printf("[Mail task] done after %d s: %s", int(time.Since(started).Seconds()), textutil.Prefix(answer, 300))
		if !strings.Contains(answer, "<") { // plain text: keep the line breaks
			answer = strings.ReplaceAll(strings.ReplaceAll(answer, "&", "&amp;"), "\n", "<br>")
		}
		// The model sometimes answers with send_email itself: then no second reply.
		subject := command.Subject
		if !strings.HasPrefix(subject, "Re:") {
			subject = "Re: " + subject
		}
		s.cmdMu.Lock()
		replied := s.replied
		s.cmdMu.Unlock()
		sent := "already answered by send_email"
		if !replied {
			sent = s.Send(command.Sender, subject, answer, nil)
		}
		log.Printf("[Mail task] answer: %s", sent)
		if replied || sent == "sent" {
			s.markAnswered(command.UID)
		}
		s.cmdMu.Lock()
		s.replyTo = ""
		s.cmdMu.Unlock()
	}
}

// Run checks the inbox every MAIL_POLL_MS for mails from household members and carries them out
// until ctx ends. It does nothing (and returns) without a mailbox, MAIL_AUTHSERV_ID or OnCommand.
// Start it only when this device is the primary one: the M5 may still be answering the same
// mailbox, and both would run each command.
func (s *Service) Run(ctx context.Context) {
	if !s.TakesCommands() || s.OnCommand == nil {
		return
	}
	rejected := map[uint32]bool{}
	interval := time.Duration(prompts.Int("MAIL_POLL_MS", 60000)) * time.Millisecond
	timer := time.NewTimer(0)
	defer timer.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-timer.C:
		}
		if s.Idle != nil && !s.Idle() {
			timer.Reset(time.Second) // in a conversation: look again soon, as the M5's worker does
			continue
		}
		s.runCommands(rejected)
		timer.Reset(interval)
	}
}
