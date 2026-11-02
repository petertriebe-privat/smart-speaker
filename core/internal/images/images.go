// Package images: image jobs, as devices/atoms3r/src/services/images.cpp. Generate an image with
// the OpenAI Images API (JPEG, ~30-60 s), then print it, mail it as an attachment and/or send it
// to a person's Telegram chat. Tools: create_image, send_last_image (deliver the last image again
// without generating it).
//
// Jobs wait in a queue (IMAGE_QUEUE_LENGTH) and run one after the other in their own goroutine:
// the tool answers right away. The image is generated during a conversation, but delivered only
// after it (on the M5 the upload dropped the GPT Live connection; kept here through Busy). The
// last image stays in memory. The outcome of every job is left for the main loop (TakeResult).
package images

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"log"
	"net"
	"strconv"
	"strings"
	"sync"
	"time"

	"echo-assistant/internal/config"
	"echo-assistant/internal/mail"
	"echo-assistant/internal/openai"
	"echo-assistant/internal/people"
	"echo-assistant/internal/prompts"
	"echo-assistant/internal/textutil"
	"echo-assistant/internal/tools"
)

// Mailer is what the jobs need of the mailbox (*mail.Service).
type Mailer interface {
	Configured() bool
	Send(to, subject, html string, a *mail.Attachment) string
}

// Chat is what the jobs need of Telegram (*telegram.Bot).
type Chat interface {
	Configured() bool
	Send(html, person string) string
	SendPhoto(jpeg []byte, caption, person string) string
}

// Result is the outcome of a finished job. Spoken: a failure was already announced on the
// speaker (no conversation was open).
type Result struct {
	Message string
	OK      bool
	Spoken  bool
}

type job struct {
	prompt, title, emailTo string
	person                 string // who asked (people id): Telegram goes to their chat
	print, color           bool
	landscape              bool // wider than high (1536x1024); printed turned onto the A4 page
	telegram, byMail       bool
	reuseLast              bool // send_last_image: deliver the last image, generate nothing
}

// Service is the image service. Set the hooks before AddTools.
type Service struct {
	// Mail and Telegram: nil = not available.
	Mail     Mailer
	Telegram Chat
	// PrintJPEG prints a JPEG as its own job (the M5's printer::printJpeg over IPPS). It returns
	// the job id (-1 = accepted, id unknown) or 0 with an error text. nil: if PRINTER_EMAIL is
	// set, the image goes to the print service by mail (the same way as send_file's "print");
	// otherwise there is no printer.
	PrintJPEG func(jpeg []byte, jobName string, color bool) (id int, errText string)
	// Announce says a text on the speaker (agent.Announcer.Speak); used for failures when no
	// conversation is open. nil = failures are not announced.
	Announce func(text string) string
	// Busy: a conversation is open (live::open on the M5): failures are told in it, not announced.
	// nil = never.
	Busy func() bool
	// HoldDelivery: delivery waits for the end of the conversation (the M5: its one TLS connection
	// dropped GPT Live during an upload). Off on the Echos and TVs: an image asked for by Telegram
	// came only after "tschüss" (29.10.2026).
	HoldDelivery bool

	people       *people.Register
	ownerEmail   string
	printerEmail string

	mu      sync.Mutex
	queued  []*job
	running bool
	pending int // queued or running

	results chan Result

	// Only the worker uses these.
	last        []byte
	lastTitle   string
	lastPrompt  string
	retryPause  func(attempt int) // tests shorten it
	generateJPG func(prompt, size string) ([]byte, int, error)
}

// New creates the service. OWNER_EMAIL (the admin's address, the default for "me") and
// PRINTER_EMAIL come from the secrets.
func New(secrets config.Secrets, reg *people.Register) *Service {
	s := &Service{people: reg, ownerEmail: secrets.String("OWNER_EMAIL"), printerEmail: secrets.String("PRINTER_EMAIL"),
		results: make(chan Result, prompts.Int("IMAGE_QUEUE_LENGTH", 3)+1)}
	s.retryPause = func(attempt int) { time.Sleep(time.Duration(5*attempt) * time.Second) }
	s.generateJPG = s.requestImage
	return s
}

func (s *Service) mailOK() bool     { return s.Mail != nil && s.Mail.Configured() }
func (s *Service) telegramOK() bool { return s.Telegram != nil && s.Telegram.Configured() }
func (s *Service) printerOK() bool {
	return s.PrintJPEG != nil || (s.printerEmail != "" && s.mailOK())
}
func (s *Service) busy() bool { return s.Busy != nil && s.Busy() }

// filenameFor: "Katze im Weltall" -> "katze-im-weltall.jpg"
func filenameFor(title string) string {
	name := textutil.Slug(title, 40)
	if name == "" {
		name = "assistant-bild"
	}
	return name + ".jpg"
}

// --- Generation -------------------------------------------------------------------------------

// sizeFor: IMAGE_SIZE (portrait, like A4) or, for a landscape image, its sides swapped.
func sizeFor(landscape bool) string {
	size := prompts.Text("IMAGE_SIZE")
	if w, h, ok := strings.Cut(size, "x"); ok && landscape {
		return h + "x" + w
	}
	return size
}

// requestImage: one request to the Images API. Returns the JPEG, or an error with the HTTP status
// (0 = none: no connection, timeout). size "": IMAGE_SIZE.
func (s *Service) requestImage(prompt, size string) ([]byte, int, error) {
	if size == "" {
		size = sizeFor(false)
	}
	var answer struct {
		Data []struct {
			B64 string `json:"b64_json"`
		} `json:"data"`
	}
	err := openai.Post(context.Background(), "/v1/images/generations", map[string]any{
		"model":              prompts.Text("IMAGE_MODEL"),
		"prompt":             prompt,
		"size":               size,
		"quality":            prompts.Text("IMAGE_QUALITY"),
		"output_format":      "jpeg",
		"output_compression": 85,
	}, &answer)
	if err != nil {
		status, text := classify(err)
		return nil, status, errors.New(text)
	}
	if len(answer.Data) == 0 || answer.Data[0].B64 == "" {
		return nil, 200, errors.New("no image data in the answer")
	}
	jpeg, err := base64.StdEncoding.DecodeString(answer.Data[0].B64)
	if err != nil {
		return nil, 200, errors.New("image data not decodable")
	}
	return jpeg, 200, nil
}

// classify splits an openai.Post error into the HTTP status (0 if there was none) and the text
// for the user (the M5's openai::errorMessage).
func classify(err error) (int, string) {
	var netErr net.Error
	if errors.Is(err, context.DeadlineExceeded) || (errors.As(err, &netErr) && netErr.Timeout()) {
		return 0, "Zeitueberschreitung beim Generieren"
	}
	text := err.Error()
	status := 0
	if rest, ok := strings.CutPrefix(text, "HTTP "); ok {
		digits, message, _ := strings.Cut(rest, ":")
		status, _ = strconv.Atoi(digits)
		text = strings.TrimSpace(message)
		if text == "" {
			text = "HTTP " + digits
		}
	}
	// The safety system's message is long English text: short instead (the Go openai package does
	// not map the error code moderation_blocked).
	if lower := strings.ToLower(text); strings.Contains(lower, "safety system") || strings.Contains(lower, "moderation") {
		text = "rejected by the content filter"
	}
	return status, text
}

// retryable: retries only where no image was made (and billed): no connection (status 0 and no
// timeout), rate limit, server errors. A timeout is not retried: the server may have finished the
// image anyway.
func retryable(status int, text string) bool {
	if status == 0 {
		return text != "Zeitueberschreitung beim Generieren"
	}
	return status == 429 || status >= 500
}

func (s *Service) generate(prompt, size string) ([]byte, error) {
	attempts := prompts.Int("IMAGE_ATTEMPTS", 3)
	for attempt := 1; ; attempt++ {
		jpeg, status, err := s.generateJPG(prompt, size)
		if err == nil {
			return jpeg, nil
		}
		log.Printf("[Image] attempt %d: %v", attempt, err)
		if !retryable(status, err.Error()) || attempt >= attempts {
			return nil, err
		}
		s.retryPause(attempt)
	}
}

// --- Jobs -------------------------------------------------------------------------------------

// waitUntilNoConversation: mailing or printing an image during a conversation dropped the GPT Live
// WebSocket on the M5 every time: the image is generated during the conversation but delivered
// after it.
func (s *Service) waitUntilNoConversation() {
	if !s.HoldDelivery || !s.busy() {
		return
	}
	log.Print("[Image] done, delivery waits for the end of the conversation")
	limit := time.Duration(prompts.Int("MAX_SESSION_MS", 1200000)+prompts.Int("THINKING_TIMEOUT_MS", 180000)) * time.Millisecond
	since := time.Now()
	for s.busy() && time.Since(since) < limit {
		time.Sleep(500 * time.Millisecond)
	}
	time.Sleep(time.Second) // let the WebSocket close
}

func (s *Service) report(j *job, ok bool, message string) {
	// Nobody may be looking at the display. After the conversation the assistant says it out loud,
	// during one the main loop tells the model, which tells the user.
	spoken := false
	if !ok && !s.busy() && s.Announce != nil {
		speech := "Sir, das Bild " + j.title + " hat leider nicht geklappt. " + message + "."
		// "Inhaltsfilter" as the M5 tests it; the Go side says "content filter".
		if strings.Contains(message, "Inhaltsfilter") || strings.Contains(message, "content filter") {
			speech += " Geschützte Figuren, Marken oder echte Personen lehnt der Filter oft ab."
		}
		spoken = !strings.HasPrefix(s.Announce(speech), "Error")
	}
	select {
	case s.results <- Result{message, ok, spoken}:
	default:
	}
	// After a mail command or a chat nobody sees the display: the owner learns of failures where
	// he waits for the image.
	if !ok && j.telegram && s.telegramOK() {
		s.Telegram.Send("Das Bild „"+j.title+"“ hat nicht geklappt: "+message, j.person)
		return
	}
	ownerWaits := j.byMail || strings.EqualFold(j.emailTo, s.ownerEmail)
	if !ok && ownerWaits && s.mailOK() {
		s.Mail.Send(s.ownerEmail, "Bild: "+j.title,
			"<p>Das Bild &bdquo;"+j.title+"&ldquo; hat nicht geklappt: "+message+"</p>", nil)
	}
}

// printImage prints through the hook, else by mail to the print service. A landscape image is
// turned a quarter first: the print service put pumpkin faces asked for in landscape on a
// portrait page (30.10.2026, then still generated in portrait); turned, it fills the A4 page
// held sideways.
func (s *Service) printImage(jpeg []byte, jobName string, color bool) (int, string) {
	jpeg = portraitForPrint(jpeg)
	if s.PrintJPEG != nil {
		return s.PrintJPEG(jpeg, jobName, color)
	}
	result := s.Mail.Send(s.printerEmail, jobName, "", &mail.Attachment{Filename: filenameFor(jobName), MIME: "image/jpeg", Data: jpeg})
	if result != "sent" {
		return 0, result
	}
	return -1, "" // accepted, job id unknown
}

func (s *Service) run(j *job) {
	started := time.Now()
	if j.reuseLast {
		if s.last == nil {
			s.report(j, false, "Seit dem letzten Neustart wurde noch kein Bild generiert")
			return
		}
		j.title, j.prompt = s.lastTitle, s.lastPrompt
	} else {
		jpeg, err := s.generate(j.prompt, sizeFor(j.landscape))
		if err != nil {
			s.report(j, false, "image not generated: "+err.Error())
			return
		}
		log.Printf("[Image] %d bytes after %d s", len(jpeg), int(time.Since(started).Seconds()))
		s.last, s.lastTitle, s.lastPrompt = jpeg, j.title, j.prompt
	}
	s.waitUntilNoConversation()

	ok := true
	done := ""
	join := func(text string) {
		if done != "" {
			done += ", "
		}
		done += text
	}
	if j.print {
		id, errText := s.printImage(s.last, prompts.Name()+": "+j.title, j.color)
		ok = id != 0
		if ok {
			join("printed (job " + strconv.Itoa(id) + ")")
		} else {
			join("printing failed: " + errText)
		}
	}
	if j.emailTo != "" {
		html := "<p>Hier ist das Bild &bdquo;" + j.title + "&ldquo;.</p><p style=\"color:#888\">" +
			"Beschreibung: " + j.prompt + "</p>"
		mailStarted := time.Now()
		sent := s.Mail.Send(j.emailTo, j.title, html, &mail.Attachment{Filename: filenameFor(j.title), MIME: "image/jpeg", Data: s.last})
		log.Printf("[Image] mail: %s after %d s", sent, int(time.Since(mailStarted).Seconds()))
		if sent != "sent" {
			ok = false
			join("mail failed: " + sent)
		} else {
			join("mailed to " + j.emailTo)
		}
	}
	if j.telegram {
		sendStarted := time.Now()
		sent := s.Telegram.SendPhoto(s.last, j.title, j.person)
		log.Printf("[Image] chat: %s after %d s", sent, int(time.Since(sendStarted).Seconds()))
		if sent != "sent" {
			ok = false
			join("chat failed: " + sent)
		} else {
			join("sent to the chat")
		}
	}
	s.report(j, ok, fmt.Sprintf("image \"%s\" %s, %d s", j.title, done, int(time.Since(started).Seconds())))
}

// worker runs the queue until it is empty; queue() starts it again when needed.
func (s *Service) worker() {
	for {
		s.mu.Lock()
		if len(s.queued) == 0 {
			s.running = false
			s.mu.Unlock()
			return
		}
		j := s.queued[0]
		s.queued = s.queued[1:]
		s.mu.Unlock()
		s.run(j)
		s.mu.Lock()
		s.pending--
		s.mu.Unlock()
	}
}

func (s *Service) queue(j *job) string {
	errText := ""
	switch {
	case !j.print && j.emailTo == "" && !j.telegram:
		errText = "give print, email_to or telegram"
	case j.print && !s.printerOK():
		errText = "no printer configured"
	case j.telegram && !s.telegramOK():
		errText = "Telegram is not set up"
	case j.emailTo != "" && (!s.mailOK() || strings.Index(j.emailTo, "@") < 1):
		errText = "e-mail not possible (address invalid or no mailbox)"
	}
	if errText != "" {
		return "Error: " + errText
	}
	var ways []string
	if j.print {
		ways = append(ways, "printed")
	}
	if j.emailTo != "" {
		ways = append(ways, "mailed to "+j.emailTo)
	}
	if j.telegram {
		ways = append(ways, "sent to the chat")
	}
	what := strings.Join(ways, " and ")

	s.mu.Lock()
	ahead := s.pending
	if len(s.queued) >= prompts.Int("IMAGE_QUEUE_LENGTH", 3) {
		s.mu.Unlock()
		return fmt.Sprintf("Error: %d image jobs are already waiting, please try again later.", prompts.Int("IMAGE_QUEUE_LENGTH", 3))
	}
	s.pending++
	s.queued = append(s.queued, j)
	start := !s.running
	s.running = true
	s.mu.Unlock()
	if start {
		go s.worker()
	}

	answer := "The last image is being " + what + "."
	if !j.reuseLast {
		answer = "The image is being generated now (about a minute) and then " + what + "."
	}
	if ahead > 0 {
		answer += " Vorher ist noch ein anderer Bildauftrag dran."
	}
	if s.HoldDelivery && s.busy() {
		answer += " Gedruckt bzw. verschickt wird erst nach dem Gespraech, damit die Verbindung stabil " +
			"bleibt: sag dem Nutzer, dass das Bild kommt, sobald ihr fertig seid."
	}
	return answer
}

// call queues the job and returns what the model is told right away. byMail: nobody is at the
// speaker (agent), so failures are reported to the owner by mail.
func (s *Service) call(name string, args map[string]any, byMail bool, person string) string {
	j := &job{person: person, reuseLast: name == "send_last_image", byMail: byMail}
	j.prompt, _ = args["prompt"].(string)
	if title, ok := args["title"].(string); ok {
		j.title = title
	} else {
		j.title = "Bild"
	}
	j.print = tools.Bool(args, "print", false)
	j.color = tools.Bool(args, "color", false)
	j.landscape = tools.Bool(args, "landscape", false)
	j.emailTo = tools.String(args, "email_to")
	if strings.EqualFold(j.emailTo, "me") { // the asking person's address, the admin's by default
		asking, found := s.people.Find(person)
		j.emailTo = s.ownerEmail
		if found && asking.Email != "" {
			j.emailTo = asking.Email
		}
	}
	j.telegram = tools.Bool(args, "telegram", false)
	if !j.reuseLast && j.prompt == "" {
		return "Error: no image description"
	}
	return s.queue(j)
}

// TakeResult returns the outcome of a finished job, once each (false = none). The main loop shows
// it, plays a tone and tells the conversation (live notice); Spoken: a failure was already
// announced on the speaker.
func (s *Service) TakeResult() (Result, bool) {
	select {
	case r := <-s.results:
		return r, true
	default:
		return Result{}, false
	}
}

// AddTools registers create_image and send_last_image if a printer, a mailbox or Telegram is set.
func (s *Service) AddTools(r *tools.Registry) {
	if !s.printerOK() && !s.mailOK() && !s.telegramOK() {
		return
	}
	// The delivery parameters both tools share.
	delivery := `"print": {"type": "boolean", "description": "Print the image"},
              "color": {"type": "boolean", "description": "Print in colour (true) or black and white (false)"},
              "email_to": {"type": "string", "description": "E-mail the image to this address, \"me\" = the user's own, \"\" = no mail"}`
	required := `"print", "color", "email_to"`
	chat := s.telegramOK()
	if chat {
		delivery += `,
              "telegram": {"type": "boolean", "description": "Send the image to the user's chat on the phone (Telegram, or WhatsApp for whom has no Telegram)"}`
		required += `, "telegram"`
	}
	pick := func(withChat, without string) string {
		if chat {
			return withChat
		}
		return without
	}
	// Both queue a background job (~30-60 s); the model gets told so. Outside a conversation
	// nobody is at the speaker: failures are then reported by mail (or Telegram).
	r.Add(tools.Tool{
		Name: "create_image",
		Description: pick("Generates an image with AI, then prints it on the A4 printer at home, e-mails it as a JPEG "+
			"attachment and/or sends it to the user's chat (Telegram or WhatsApp). Takes about a minute; returns right away "+
			"and keeps running in the background. For colouring pages ask for black line art on white and set "+
			"color to false. Never name protected characters, brands or real people in the prompt (the image "+
			"model rejects them): describe the look instead. If the user did not say where it should go, ask.",
			"Generates an image with AI, then prints it on the A4 printer at home and/or e-mails it as a JPEG "+
				"attachment. Takes about a minute; returns right away and keeps running in the background. For "+
				"colouring pages ask for black line art on white and set color to false. Never name protected "+
				"characters, brands or real people in the prompt (the image model rejects them): describe the look "+
				"instead. If the user did not say where it should go, ask."),
		Parameters: `{"type": "object", "additionalProperties": false, "properties": {` + delivery + `,
              "landscape": {"type": "boolean", "description": "Landscape format (Querformat: wider than high, printed sideways on A4); false = portrait"},
              "prompt": {"type": "string", "description": "Detailed image description, in the chosen format"},
              "title": {"type": "string", "description": "Short name (print job, mail subject, file name)"}},
            "required": [` + required + `, "landscape", "prompt", "title"]}`,
		Channels: tools.Everywhere,
		Run: func(args map[string]any, ctx tools.Context) string {
			return s.call("create_image", args, ctx.Channel != tools.Voice, ctx.Person)
		},
	})
	r.Add(tools.Tool{
		Name: "send_last_image",
		Description: pick("Prints, e-mails and/or sends to the chat (Telegram or WhatsApp) the image generated last with create_image again, "+
			"without generating a new one (e.g. 'druck das Bild auch noch aus', 'schick es auch an ...', "+
			"'schick es mir aufs Handy'). If an image is still being generated, it takes that one.",
			"Prints and/or e-mails the image generated last with create_image again, without generating a new "+
				"one (e.g. 'druck das Bild auch noch aus', 'schick es auch an ...'). If an image is still being "+
				"generated, it takes that one."),
		Parameters: `{"type": "object", "additionalProperties": false, "properties": {` + delivery +
			`}, "required": [` + required + `]}`,
		Channels: tools.Everywhere,
		Run: func(args map[string]any, ctx tools.Context) string {
			return s.call("send_last_image", args, ctx.Channel != tools.Voice, ctx.Person)
		},
	})
}
