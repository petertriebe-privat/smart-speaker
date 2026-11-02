// Package whatsapp: The assistant on WhatsApp (Cloud API), as devices/atoms3r/src/services/whatsapp.cpp.
// The people of the household write to the household's number from their phones; the assistant answers
// as that number. Meta delivers incoming messages only by webhook, so a small relay
// (services/whatsapp-relay/, a Cloudflare Worker) takes them and Run long-polls it
// (WHATSAPP_RELAY_URL / WHATSAPP_RELAY_TOKEN); answers go straight to the Graph API
// (WHATSAPP_PHONE_NUMBER_ID, WHATSAPP_ACCESS_TOKEN).
//
// Two halves:
//
//   - Sending (Send, the tool send_whatsapp) works at any time and needs no Run.
//   - Receiving is Run: it polls the relay's event queue. The relay deletes every event up to
//     the "after" id of a poll, so two pollers (the M5 and the Echo) would take messages from
//     each other: the caller must start Run only when the Echo is the household's primary
//     device.
//
// A sender is a person by their phone number (internal/people); others are reported to the
// admin once and not answered. Like Telegram: one Responses thread per person until the chat is
// quiet for TELEGRAM_THREAD_MS, then it goes to the memory; voice notes are transcribed and
// answered with a voice note too; photos go to the model; text files and PDFs are read.
// Free-form answers are only possible within 24 hours of the person's last message (WhatsApp's
// service window). The relay also queues announcements from a household server and the Google
// login callback; Run passes them on to the Announce/GoogleLogin hooks.
//
// Not ported: setIdle and the TLS gate (see the Telegram package).
package whatsapp

import (
	"context"
	"log"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"echo-assistant/internal/chatcore"
	"echo-assistant/internal/config"
	"echo-assistant/internal/memory"
	"echo-assistant/internal/netx"
	"echo-assistant/internal/people"
	"echo-assistant/internal/prompts"
	"echo-assistant/internal/tools"
)

func logf(format string, args ...any) { log.Printf("[WhatsApp] "+format, args...) }

// Memory is what the service needs of the memory (memory.Memory has all of it).
type Memory interface {
	chatcore.Memory
	// LogEvent: a contact without a conversation for the activity log.
	LogEvent(person, channel, title, summary string)
}

// Presence tells whether somebody is at home, for announcements.
type Presence struct {
	Sensor   bool // a presence sensor exists (occupancy.available)
	Someone  bool // somebody is in the room (occupancy.someoneThere)
	LiveOpen bool // a voice conversation is open (live.open)
}

// Service is the WhatsApp service.
type Service struct {
	// Chat answers one incoming message with the backend agent (agent::run on the M5); see
	// chatcore.Request for what the caller has to do. Nil answers "Error: the chat is not
	// connected".
	Chat func(chatcore.Request) string
	// StoreFile keeps a document a person sent (files::store: Drive); source is "WhatsApp von
	// <name>". Returns "" or an error text (logged only). Nil = documents are not kept.
	StoreFile func(name, mime string, data []byte, source string) string
	// Telegram sends the admin a report as Telegram HTML (telegram.Bot.Send; nil = not set up).
	// Used for messages from unknown numbers, with WhatsApp itself as the fallback.
	Telegram func(html, person string) string
	// Speak says a household server's announcement at home (tasks::runNow("reminder", source,
	// text, "speak") on the M5: queued and told by the live session as a notice), on the assistant
	// speaker in room ('' = this device). Nil = logged only.
	Speak func(source, text, room string)
	// Presence reports who is at home; announcements wait for somebody (Sensor && !Someone &&
	// !LiveOpen) for at most ANNOUNCE_HOLD_MS, at most ANNOUNCE_HOLD_MAX of them. Nil = speak at
	// once.
	Presence func() Presence
	// GoogleLogin gets the code and state of a Google login the admin did through the relay's
	// /google redirect (google::loginCallback). Nil = ignored.
	GoogleLogin func(code, state string)
	// FromLine overrides the sender line of send_whatsapp (tools::fromLine); nil =
	// chatcore.FromLine.
	FromLine func(ctx tools.Context, recipient string) string

	relayHost     string
	relayToken    string
	phoneNumberID string
	accessToken   string
	graphBase     string // https://graph.facebook.com (a test server in tests)
	reg           *people.Register
	mem           Memory

	client *http.Client
	chats  *chatcore.Manager
	tools  *tools.Registry

	running  atomic.Bool
	sendMu   sync.Mutex       // one send at a time; also guards lastSeen and the outbox file
	lastSeen map[string]int64 // WhatsApp id -> unix time of their last message (outbox.go)
	numberMu sync.Mutex
	number   string // the household's number as Meta shows it (Number)

	after         int64 // the relay's last event id handled
	failures      int
	ready         bool
	strangersSeen map[string]bool
	held          []announcement
}

// New creates the service from the secrets WHATSAPP_RELAY_URL, WHATSAPP_RELAY_TOKEN,
// WHATSAPP_PHONE_NUMBER_ID and WHATSAPP_ACCESS_TOKEN. mem may be nil.
func New(secrets config.Secrets, reg *people.Register, mem Memory) *Service {
	s := &Service{
		relayToken:    secrets.String("WHATSAPP_RELAY_TOKEN"),
		phoneNumberID: secrets.String("WHATSAPP_PHONE_NUMBER_ID"),
		accessToken:   secrets.String("WHATSAPP_ACCESS_TOKEN"),
		graphBase:     "https://graph.facebook.com",
		reg:           reg,
		mem:           mem,
		client:        netx.Client(0), // limits are set per request
		strangersSeen: map[string]bool{},
	}
	s.relayHost = relayHostOf(secrets.String("WHATSAPP_RELAY_URL"))
	s.chats = &chatcore.Manager{
		Label: "WhatsApp", Channel: memory.WhatsApp, File: "whatsapp_chat.json", Mem: mem,
		Name:    reg.Name,
		Timeout: time.Duration(prompts.Int("TELEGRAM_THREAD_MS", 30*60*1000)) * time.Millisecond,
	}
	return s
}

// relayHostOf: the relay's host from WHATSAPP_RELAY_URL ("https://name.account.workers.dev").
func relayHostOf(url string) string {
	url = strings.TrimPrefix(url, "https://")
	if i := strings.IndexByte(url, '/'); i >= 0 {
		return url[:i]
	}
	return url
}

// Configured: relay and Graph API set in the secrets.
func (s *Service) Configured() bool {
	return s.relayHost != "" && s.relayToken != "" && s.phoneNumberID != "" && s.accessToken != ""
}

// Wire sets this service's Send in the people register's channels, if it is configured.
func (s *Service) Wire(c *people.Channels) {
	if s.Configured() {
		c.WhatsApp = s.Send
		c.WhatsAppLink = s.Number
	}
}

// recipient: the WhatsApp id of a person ("" = the admin), or an error text.
func (s *Service) recipient(person string) (to, errText string) {
	if !s.Configured() {
		return "", "Error: WhatsApp is not set up"
	}
	if person == "" {
		person = s.reg.Admin()
	}
	p, ok := s.reg.Find(person)
	if !ok {
		return "", "Error: unknown person."
	}
	if to = waID(p.Phones); to == "" {
		return "", "Error: no phone number registered for " + p.Name + "."
	}
	return to, ""
}

// Send sends text (WhatsApp formatting: *bold*, _italic_, bare links) to a person's WhatsApp
// ("" = the admin) from any goroutine. Blocks for a second or two. Returns "sent" or an error.
// Outside the 24-hour window it goes as the template (outbox.go). It is people.Channels.WhatsApp.
func (s *Service) Send(text, person string) string {
	to, errText := s.recipient(person)
	if errText != "" {
		return errText
	}
	ctx := context.Background()
	s.sendMu.Lock()
	defer s.sendMu.Unlock()
	if !s.inWindow(to, time.Now()) {
		return s.outside(ctx, queued{To: to, Kind: "text", Text: text}, text, true)
	}
	result := s.sendText(ctx, to, text)
	if strings.Contains(result, "(131047)") { // the window closed after all: the template then
		return s.outside(ctx, queued{To: to, Kind: "text", Text: text}, text, true)
	}
	return result
}

// AddTools registers send_whatsapp (if configured) and keeps the registry for call backs
// (phone_call). Polling is not started here, see Run.
func (s *Service) AddTools(r *tools.Registry) {
	s.tools = r
	if !s.Configured() {
		return
	}
	r.Add(tools.Tool{
		Name: "send_whatsapp",
		Description: "Sends a WhatsApp message to the user or another person of the household, from the household's number: " +
			"links, lists, addresses, anything to read later ('schick mir das per WhatsApp'). If that person has not " +
			"written on WhatsApp for 24 hours, it goes as a short notice template and the rest follows when they " +
			"answer. Format: *bold*, _italic_, bare links; no HTML.",
		Parameters: `{"type": "object", "additionalProperties": false,
            "properties": {"text": {"type": "string"},
                           "to": {"type": "string", "description": "name of a person of the household, '' = the user"}},
            "required": ["text", "to"]}`,
		Channels: tools.Everywhere,
		Run: func(args map[string]any, ctx tools.Context) string {
			person, errText := chatcore.Recipient(s.reg, tools.String(args, "to"), ctx)
			if errText != "" {
				return errText
			}
			from := s.fromLine(ctx, person)
			if from != "" {
				from = "_" + from + "_\n"
			}
			text, _ := args["text"].(string)
			return s.Send(from+text, person)
		},
	})
}

func (s *Service) fromLine(ctx tools.Context, person string) string {
	if s.FromLine != nil {
		return s.FromLine(ctx, person)
	}
	return chatcore.FromLine(s.reg, ctx, person)
}

// helpText is the answer to /start, /hilfe and /help.
func helpText() string {
	return "Ich bin " + prompts.Name() + ". Schreib mir einfach, was du brauchst: Licht und Geräte schalten, Fragen und " +
		"Websuche, Mails, Erinnerungen und Aufgaben, Skills. Ich weiß dasselbe wie zu Hause am " +
		"Lautsprecher.\n\n" +
		"/neu – neues Gespräch beginnen\n\n" +
		"Auf Sprachnachrichten antworte ich auch mit Sprache. Fotos schaue ich mir an (schreib dazu, was " +
		"ich damit tun soll)."
}

// memory.Memory provides everything the service needs.
var _ Memory = (*memory.Memory)(nil)
