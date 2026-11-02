// Package telegram: The assistant as a Telegram bot, as devices/atoms3r/src/services/telegram.cpp. The
// people of the household (internal/people, the admin from TELEGRAM_OWNER_ID in the secrets)
// chat with him from their phones (TELEGRAM_BOT_TOKEN). Outgoing only, no server.
//
// Two halves:
//
//   - Sending (Send, SendPhoto, SendDocument, InviteLink, the tool send_telegram) works at any
//     time and needs no Run.
//   - Receiving is Run: it long-polls getUpdates, answers commands itself and hands every other
//     message to the Chat hook (the backend agent). Run must be started by the caller only when
//     the Echo is the household's primary device: the M5 may poll with the same bot token, and
//     two pollers steal each other's updates.
//
// Each message from a person runs through the agent with their tools in their own thread until
// the chat is quiet for TELEGRAM_THREAD_MS; then it goes to the memory like a conversation.
// Messages from accounts not in the register are reported to the admin, once, and not answered.
// Voice notes are transcribed and answered with a voice note as well (TTS as Ogg Opus); photos
// go to the model as images; text files and PDFs are read.
//
// Not ported: setIdle and the TLS gate (the M5 polled only between conversations to save
// internal RAM; the Echo has no such limit, so Run polls continuously).
package telegram

import (
	"context"
	"encoding/json"
	"log"
	"net/http"
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

func logf(format string, args ...any) { log.Printf("[Telegram] "+format, args...) }

// Memory is what the bot needs of the memory (memory.Memory has all of it).
type Memory interface {
	chatcore.Memory
	// LogEvent: a contact without a conversation for the activity log.
	LogEvent(person, channel, title, summary string)
	// Skills as "name: when" lines (/skills), "" without skills.
	Skills() string
}

// Bot is the Telegram service.
type Bot struct {
	// Chat answers one incoming message with the backend agent (agent::run on the M5). The
	// request and what the caller has to do with it are described at chatcore.Request. Nil
	// answers "Error: the chat is not connected".
	Chat func(chatcore.Request) string
	// StoreFile keeps a document a person sent (files::store on the M5: Drive, so it can be
	// printed or passed on later). source is "Telegram von <name>". Returns "" or an error text,
	// which is only logged. Nil = documents are not kept.
	StoreFile func(name, mime string, data []byte, source string) string
	// FromLine overrides the sender line of send_telegram (tools::fromLine); nil = the default
	// port, chatcore.FromLine.
	FromLine func(ctx tools.Context, recipient string) string
	// Button answers the admin's tap on an inline button a server sent with callback data
	// "mcp:<tool>:<data>" (agent-mcp's approval, #57): mcpservers.CallDevice. ok false = no
	// server has the tool. Nil = taps are only acknowledged.
	Button func(tool, data string) (result string, ok bool)

	token string
	owner int64
	base  string // https://api.telegram.org (a test server in tests)
	reg   *people.Register
	mem   Memory

	client *http.Client
	chats  *chatcore.Manager
	tools  *tools.Registry

	running atomic.Bool
	sendMu  sync.Mutex // sends from several goroutines: one at a time, like the M5

	nameMu  sync.Mutex
	botName string // from getMe, once

	offset        int64 // next update_id
	commandsSet   bool
	failures      int
	strangersSeen map[int64]bool // accounts the admin has heard of
	strangers     string         // the note for the admin's next message
}

// New creates the bot. TELEGRAM_BOT_TOKEN and TELEGRAM_OWNER_ID come from the secrets. mem may
// be nil (chats are then not handed to the memory).
func New(secrets config.Secrets, reg *people.Register, mem Memory) *Bot {
	b := &Bot{
		token:         secrets.String("TELEGRAM_BOT_TOKEN"),
		base:          "https://api.telegram.org",
		reg:           reg,
		mem:           mem,
		client:        netx.Client(0), // limits are set per request
		strangersSeen: map[int64]bool{},
	}
	if v, ok := secrets["TELEGRAM_OWNER_ID"].(float64); ok {
		b.owner = int64(v)
	}
	var chatMem chatcore.Memory
	if mem != nil {
		chatMem = mem
	}
	b.chats = &chatcore.Manager{
		Label: "Telegram", Channel: memory.Telegram, File: "telegram_chat.json", Mem: chatMem,
		Name:    reg.Name,
		Timeout: time.Duration(prompts.Int("TELEGRAM_THREAD_MS", 30*60*1000)) * time.Millisecond,
	}
	return b
}

// Configured: token and owner set.
func (b *Bot) Configured() bool { return b.token != "" && b.owner != 0 }

// Wire sets the functions of this bot in the people register's channels (Telegram, InviteLink),
// if it is configured. Call before people.AddTools / use of the register.
func (b *Bot) Wire(c *people.Channels) {
	if !b.Configured() {
		return
	}
	c.Telegram = b.Send
	c.InviteLink = b.InviteLink
}

// account of a person ("" = the admin), 0 if none.
func (b *Bot) account(person string) int64 {
	if person == "" {
		return b.owner
	}
	p, ok := b.reg.Find(person)
	if !ok {
		return b.owner
	}
	return p.Telegram
}

// target resolves the chat for the send functions: the account, or the error to return.
func (b *Bot) target(person string) (int64, string) {
	if !b.Configured() {
		return 0, "Error: Telegram is not set up"
	}
	account := b.account(person)
	if account == 0 {
		return 0, "Error: no Telegram account registered for " + b.reg.Name(person) + "."
	}
	return account, ""
}

// Send sends to a person's chat (people id, "" = the admin's) from any goroutine. html is
// Telegram HTML (b, i, a href, code, pre); falls back to plain text if Telegram rejects the
// markup. Long texts go as several messages. Blocks for seconds. Returns "sent" or an error.
// It is people.Channels.Telegram.
func (b *Bot) Send(html, person string) string {
	account, errText := b.target(person)
	if errText != "" {
		return errText
	}
	b.sendMu.Lock()
	defer b.sendMu.Unlock()
	return b.sendText(context.Background(), account, html)
}

// SendPhoto sends a JPEG with a caption to a person's chat. Returns "sent" or an error.
func (b *Bot) SendPhoto(jpeg []byte, caption, person string) string {
	account, errText := b.target(person)
	if errText != "" {
		return errText
	}
	b.sendMu.Lock()
	defer b.sendMu.Unlock()
	return b.sendFile(context.Background(), account, "sendPhoto", "photo", "assistant.jpg", "image/jpeg", jpeg, caption)
}

// SendDocument sends a file as a document to a person's chat. Returns "sent" or an error.
func (b *Bot) SendDocument(data []byte, filename, mime, caption, person string) string {
	account, errText := b.target(person)
	if errText != "" {
		return errText
	}
	b.sendMu.Lock()
	defer b.sendMu.Unlock()
	return b.sendFile(context.Background(), account, "sendDocument", "document", filename, mime, data, caption)
}

// InviteLink: "https://t.me/<bot>?start=<code>"; opening it in Telegram sends "/start <code>"
// to the bot, which links the account to the person with this invite code (people.ClaimInvite).
// "" without Telegram or when the bot's name cannot be fetched. Blocks for a second the first
// time. It is people.Channels.InviteLink.
func (b *Bot) InviteLink(code string) string {
	if !b.Configured() || code == "" {
		return ""
	}
	b.nameMu.Lock()
	defer b.nameMu.Unlock()
	if b.botName == "" {
		result, err := b.callJSON(context.Background(), "getMe", map[string]any{}, timeout())
		if err != nil {
			logf("getMe: %v", err)
		} else {
			var me struct {
				Username string `json:"username"`
			}
			json.Unmarshal(result, &me)
			b.botName = me.Username
		}
	}
	if b.botName == "" {
		return ""
	}
	return "https://t.me/" + b.botName + "?start=" + code
}

// AddTools registers send_telegram (if the bot is configured) and keeps the registry for the
// /aufgaben command. Polling is not started here, see Run.
func (b *Bot) AddTools(r *tools.Registry) {
	b.tools = r
	if !b.Configured() {
		return
	}
	r.Add(tools.Tool{
		Name: "send_telegram",
		Description: "Sends a message to the user's Telegram chat on their phone, or to another person of the household: " +
			"links, lists, recipes, addresses, anything to read later ('schick mir das aufs Handy', 'per Telegram', " +
			"'schick Anna den Link'). Format with Telegram HTML only: <b>, <i>, <a href=\"...\">text</a>, <code>; no " +
			"other tags; write <, > and & in text as &lt; &gt; &amp;. Images go by create_image / send_last_image " +
			"with telegram true.",
		Parameters: `{"type": "object", "additionalProperties": false,
            "properties": {"text": {"type": "string"},
                           "to": {"type": "string", "description": "name of a person of the household, '' = the user"}},
            "required": ["text", "to"]}`,
		Channels: tools.Everywhere,
		Run: func(args map[string]any, ctx tools.Context) string {
			person, errText := chatcore.Recipient(b.reg, tools.String(args, "to"), ctx)
			if errText != "" {
				return errText
			}
			from := b.fromLine(ctx, person)
			if from != "" {
				from = "<i>" + escape(from) + "</i>\n"
			}
			text, _ := args["text"].(string)
			return b.Send(from+text, person)
		},
	})
}

func (b *Bot) fromLine(ctx tools.Context, person string) string {
	if b.FromLine != nil {
		return b.FromLine(ctx, person)
	}
	return chatcore.FromLine(b.reg, ctx, person)
}

// helpText is the answer to /start, /hilfe and /help.
func helpText() string {
	return "Ich bin " + prompts.Name() + ". Schreib mir einfach, was du brauchst: Licht und Geräte schalten, Fragen und " +
		"Websuche, Mails, Bilder, Erinnerungen und Aufgaben, Skills. Ich weiß dasselbe wie zu Hause am " +
		"Lautsprecher.\n\n" +
		"/neu – neues Gespräch beginnen\n" +
		"/aufgaben – geplante Aufgaben\n" +
		"/skills – was du mir beigebracht hast\n" +
		"/freigabe – ob Aufträge an den Server-Agenten auf deinen Klick warten (an, aus; nur Admin)\n\n" +
		"Auf Sprachnachrichten antworte ich auch mit Sprache. Fotos schaue ich mir an (schreib dazu, was " +
		"ich damit tun soll). Textdateien und PDFs lese ich auch."
}

// memory.Memory provides everything the bot needs.
var _ Memory = (*memory.Memory)(nil)
