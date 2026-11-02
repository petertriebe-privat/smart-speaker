// Package files: files the assistant received or made (Telegram and WhatsApp documents, files the model
// wrote), kept in the assistant's Google Drive folder (The assistant/Dateien), as devices/atoms3r/src/assistant/
// files.cpp. They can be sent on later: "druck das PDF aus, das ich dir geschickt habe", "leite
// die Rechnung an Anna weiter". Up to FILE_MAX_BYTES each; the user tidies the folder in Drive.
// Drive is the index: name, type, size, date and appProperties.source ("WhatsApp von Anna").
// Tools: send_file (a stored or new file to a Telegram chat, an e-mail address or the printer),
// list_files. Store is what the chat services call for incoming documents.
// read_attachment (attachment.go) reads a mail attachment with the backend model and keeps it.
package files

import (
	"context"
	"fmt"
	"log"
	"strconv"
	"strings"
	"time"

	"echo-assistant/internal/chatcore"
	"echo-assistant/internal/config"
	"echo-assistant/internal/google"
	"echo-assistant/internal/mail"
	"echo-assistant/internal/people"
	"echo-assistant/internal/prompts"
	"echo-assistant/internal/store"
	"echo-assistant/internal/tools"
)

const (
	folder       = "Dateien"
	listLimit    = 100                // files looked through by name
	contentChars = 200 * 1024         // a file the model writes (it comes as one JSON string)
	oldIndex     = "files/index.json" // until 06.10. the M5 kept files in its flash
)

// Mailer is what send_file needs of the mailbox (*mail.Service).
type Mailer interface {
	Send(to, subject, html string, a *mail.Attachment) string
}

// Chat is what send_file needs of Telegram (*telegram.Bot).
type Chat interface {
	SendDocument(data []byte, filename, mime, caption, person string) string
}

// Service is the file store.
type Service struct {
	Drive    *google.Drive
	Mail     Mailer
	Telegram Chat
	// FromLine: the line "who it comes from" put in front of what goes to a household member
	// (tools::fromLine); nil = chatcore.FromLine.
	FromLine func(ctx tools.Context, person string) string
	// Busy: a conversation is open; Run waits (live::open). nil = never.
	Busy func() bool
	// read_attachment's downloads; nil = that mailbox is not set up.
	OwnAttachment     OwnAttachment
	WatchedAttachment WatchedAttachment

	people       *people.Register
	ownerEmail   string
	printerEmail string
}

// New creates the service. OWNER_EMAIL and PRINTER_EMAIL come from the secrets.
func New(secrets config.Secrets, reg *people.Register) *Service {
	return &Service{people: reg, ownerEmail: secrets.String("OWNER_EMAIL"), printerEmail: secrets.String("PRINTER_EMAIL")}
}

func maxBytes() int { return prompts.Int("FILE_MAX_BYTES", 1024*1024) }

func (s *Service) configured() bool { return s.Drive != nil && s.Drive.Configured() }

func (s *Service) fromLine(ctx tools.Context, person string) string {
	if s.FromLine != nil {
		return s.FromLine(ctx, person)
	}
	return chatcore.FromLine(s.people, ctx, person)
}

// cleanName: a plain file name: no directories, at most 60 bytes (the end is kept).
func cleanName(name string) string {
	name = strings.TrimSpace(name)
	name = strings.NewReplacer("/", "_", "\\", "_", ":", "_", "\"", "_", "\n", "_", "\r", "_").Replace(name)
	if len(name) > 60 {
		name = name[len(name)-60:]
		for name != "" && name[0]&0xC0 == 0x80 { // not in the middle of a character
			name = name[1:]
		}
	}
	if name == "" {
		return "datei.txt"
	}
	return name
}

// MimeOf: the MIME type for a file name, e.g. "text/csv" for .csv.
func MimeOf(name string) string {
	file := strings.ToLower(name)
	for _, t := range [][2]string{{".csv", "text/csv"}, {".html", "text/html"}, {".htm", "text/html"},
		{".txt", "text/plain"}, {".md", "text/markdown"}, {".json", "application/json"},
		{".xml", "application/xml"}, {".pdf", "application/pdf"}, {".jpg", "image/jpeg"},
		{".jpeg", "image/jpeg"}, {".png", "image/png"}, {".ics", "text/calendar"}} {
		if strings.HasSuffix(file, t[0]) {
			return t[1]
		}
	}
	return "application/octet-stream"
}

// Store keeps a copy of data under name (a same name is replaced; Drive keeps the old content as a
// revision). source: where it came from ("Telegram von Anna"). Returns "" or an error text. It is
// telegram.Bot.StoreFile (and the same for WhatsApp). Blocks for seconds.
func (s *Service) Store(rawName, mime string, data []byte, source string) string {
	if !s.configured() {
		return "Error: no Google Drive set up, the file is not kept."
	}
	if len(data) > maxBytes() {
		return fmt.Sprintf("Error: too large to keep (at most %d KB).", maxBytes()/1024)
	}
	name := cleanName(rawName)
	// Gmail labels a PDF "application/octet-stream": then the name says it (the printer needs it).
	if mime == "" || mime == "application/octet-stream" {
		mime = MimeOf(name)
	}
	found, err := s.Drive.List(folder, "name="+google.Quoted(name), 1)
	if err != nil {
		return err.Error()
	}
	if len(found) == 0 {
		err = s.Drive.Create(folder, name, mime, data, map[string]string{"source": source})
	} else {
		err = s.Drive.Replace(found[0].ID, mime, data) // the same name again: its content is replaced
	}
	if err != nil {
		return err.Error()
	}
	log.Printf("[Files] kept %s in Drive (%d bytes, %s)", name, len(data), source)
	return ""
}

// find picks a stored file by name: exact (ignoring case), else the newest whose name contains
// it. found is newest first. -1 if none.
func find(found []google.Entry, name string) int {
	wanted := strings.ToLower(name)
	partial := -1
	for i, f := range found {
		own := strings.ToLower(f.Name)
		if own == wanted {
			return i
		}
		if partial < 0 && wanted != "" && strings.Contains(own, wanted) {
			partial = i
		}
	}
	return partial
}

// load reads a stored file.
func (s *Service) load(name string) (data []byte, storedName, mime, errText string) {
	if !s.configured() {
		return nil, "", "", "Error: no Google Drive set up, so no files are kept."
	}
	found, err := s.Drive.List(folder, "", listLimit)
	if err != nil {
		return nil, "", "", err.Error()
	}
	i := find(found, name)
	if i < 0 {
		return nil, "", "", "Error: no stored file \"" + name + "\" (list_files shows them)."
	}
	if found[i].Size > maxBytes() {
		return nil, "", "", "Error: \"" + found[i].Name + "\" is too large to send."
	}
	if data, err = s.Drive.Download(found[i].ID); err != nil {
		return nil, "", "", err.Error()
	}
	return data, found[i].Name, found[i].Type, ""
}

func (s *Service) listFiles() string {
	if !s.configured() {
		return "No Google Drive set up, so no files are kept."
	}
	found, err := s.Drive.List(folder, "", 20)
	if err != nil {
		return err.Error()
	}
	if len(found) == 0 {
		return "No stored files."
	}
	text := "Stored files in the assistant's Google Drive, folder the assistant/" + folder + " (newest first):\n"
	for _, f := range found {
		text += "- " + f.Name + " (" + f.Type + ", " + strconv.Itoa(f.Size/1024+1) + " KB, " + prefix(f.Modified, 10)
		if f.Source != "" {
			text += ", " + f.Source
		}
		text += ")\n"
	}
	return text
}

func prefix(s string, n int) string { return s[:min(n, len(s))] }

func printable(mime string) bool {
	return mime == "application/pdf" || strings.HasPrefix(mime, "image/")
}

func (s *Service) sendFile(args map[string]any, ctx tools.Context) string {
	content, _ := args["content"].(string)
	via, to, text := tools.String(args, "via"), tools.String(args, "to"), tools.String(args, "text")
	var data []byte
	var name, mime string
	if content != "" { // a new file the model wrote
		if len(content) > contentChars {
			return fmt.Sprintf("Error: content too long (at most %d KB).", contentChars/1024)
		}
		name = cleanName(tools.String(args, "name"))
		mime = MimeOf(name)
		data = []byte(content)
		if kept := s.Store(name, mime, data, "von "+prompts.Name()+" erstellt"); kept != "" {
			log.Printf("[Files] %s not kept: %s", name, kept)
		}
	} else {
		var errText string
		if data, name, mime, errText = s.load(tools.String(args, "file")); errText != "" {
			return errText
		}
	}

	var result string
	switch via {
	case "telegram":
		person := ctx.Person
		if to != "" {
			if person = s.people.ByName(to); person == "" {
				return "Error: " + to + " is not registered as a person."
			}
		}
		caption := text
		if from := s.fromLine(ctx, person); from != "" {
			caption = from + " " + text
		}
		if s.Telegram == nil {
			return "Error: Telegram is not set up"
		}
		result = s.Telegram.SendDocument(data, name, mime, caption, person)
	case "email":
		address := to
		if !strings.Contains(address, "@") {
			person := ctx.Person
			if to != "" {
				person = s.people.ByName(to)
			} else if person == "" {
				person = s.people.Admin()
			}
			if to != "" && person == "" {
				return "Error: " + to + " is not registered as a person."
			}
			p, _ := s.people.Find(person)
			address = p.Email
			if address == "" && p.Admin() {
				address = s.ownerEmail
			}
			if address == "" {
				return "Error: no e-mail address registered for " + p.Name + "."
			}
		}
		from := ""
		if member := s.people.ByEmail(address); member != "" {
			from = s.fromLine(ctx, member)
		}
		subject := text
		if subject == "" {
			subject = name
		}
		html := "<p>Anbei: " + name + "</p>"
		if from != "" {
			html = "<p><i>" + from + "</i></p>" + html
		}
		if s.Mail == nil {
			return "Error: no mailbox"
		}
		result = s.Mail.Send(address, subject, html, &mail.Attachment{Filename: name, MIME: mime, Data: data})
	case "print":
		if !printable(mime) {
			return "Error: the printer takes PDFs and images; for text, tables or HTML use print_document."
		}
		if s.Mail == nil || s.printerEmail == "" {
			return "Error: no printer configured"
		}
		result = s.Mail.Send(s.printerEmail, name, "", &mail.Attachment{Filename: name, MIME: mime, Data: data})
	default:
		return "Error: via is telegram, email or print."
	}
	log.Printf("[Files] %s (%d bytes) via %s: %s", name, len(data), via, result)
	if result == "sent" {
		return fmt.Sprintf("Sent %s (%d KB) via %s.", name, len(data)/1024+1, via)
	}
	return result
}

// AddTools registers send_file and list_files.
func (s *Service) AddTools(r *tools.Registry) {
	r.Add(tools.Tool{
		Name: "send_file",
		Description: "Sends a file to a chat (via telegram: Telegram, or WhatsApp for whom has no Telegram; as a document), " +
			"by e-mail (as an attachment) or to the printer (PDFs and " +
			"images). Either a new file you write now (name with extension + content: CSV for tables, which open in " +
			"Excel or Sheets on the phone; HTML for formatted documents with tables; TXT, JSON, MD), or a stored one " +
			"(file: e.g. a PDF someone sent over Telegram or WhatsApp, see list_files). Telegram shows no tables and no " +
			"Markdown in messages: small tables go in the chat as <pre>, larger ones as a CSV or HTML file. New files " +
			"are kept, so they can be sent on later.",
		Parameters: `{"type": "object", "additionalProperties": false,
            "properties": {
              "file": {"type": "string", "description": "name of a stored file to send, '' when you write a new one"},
              "name": {"type": "string", "description": "file name with extension for new content, e.g. kosten-september.csv; '' for a stored file"},
              "content": {"type": "string", "description": "the complete content of a new file as text; '' for a stored file"},
              "via": {"type": "string", "enum": ["telegram", "email", "print"]},
              "to": {"type": "string", "description": "telegram/email: a person of the household by name or an e-mail address; '' = the user"},
              "text": {"type": "string", "description": "Telegram caption or e-mail subject in German, '' for none"}},
            "required": ["file", "name", "content", "via", "to", "text"]}`,
		Channels: tools.Everywhere,
		Run:      s.sendFile,
	})
	r.Add(tools.Tool{
		Name: "list_files",
		Description: "Lists the stored files (received over Telegram or WhatsApp, or written earlier with send_file), kept in " +
			"The assistant's Google Drive folder the assistant/Dateien: name, type, size, date, where from. Newest first.",
		Parameters: `{"type": "object", "additionalProperties": false, "properties": {}, "required": []}`,
		Channels:   tools.Everywhere,
		Run:        func(map[string]any, tools.Context) string { return s.listFiles() },
	})
	s.addAttachmentTool(r)
}

// --- Background work: the files of the M5's flash ------------------------------------------------

type oldFile struct {
	Path, Name, Type, Source string
}

// moveOld moves the files kept locally in the M5's way (state_dir/files, index.json) to Drive once,
// then deletes them. true: finished (nothing to move, or moved); false: try again later.
func (s *Service) moveOld() (finished bool, retryAfter time.Duration) {
	if !store.Exists(oldIndex) || !s.configured() {
		return true, 0
	}
	var index struct {
		Files []oldFile `json:"files"`
	}
	read := store.LoadJSON(oldIndex, &index)
	for _, entry := range index.Files {
		data, err := store.Read(entry.Path)
		if err != nil || len(data) == 0 {
			continue
		}
		if errText := s.Store(entry.Name, entry.Type, data, entry.Source); errText != "" {
			log.Printf("[Files] moving %s to Drive: %s", entry.Name, errText)
			return false, 10 * time.Minute // the rest next time; what moved is stored again (same name: replaced)
		}
	}
	if !read {
		log.Print("[Files] old file index unreadable: deleted")
	}
	for _, entry := range index.Files {
		store.Remove(entry.Path)
	}
	store.Remove(oldIndex)
	log.Printf("[Files] %d files moved to Drive", len(index.Files))
	return true, 0
}

// Run is the background work (moveOldFiles of the M5): moves old local files to Drive once, between
// conversations, and ends. Start it as a goroutine.
func (s *Service) Run(ctx context.Context) {
	var retryAt time.Time
	tick := time.NewTicker(10 * time.Second)
	defer tick.Stop()
	for {
		if time.Now().Year() >= 2024 && time.Now().After(retryAt) && (s.Busy == nil || !s.Busy()) {
			finished, after := s.moveOld()
			if finished {
				return
			}
			retryAt = time.Now().Add(after)
		}
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
		}
	}
}
