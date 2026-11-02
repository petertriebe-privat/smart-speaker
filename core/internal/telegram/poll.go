package telegram

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"time"

	"echo-assistant/internal/chatcore"
	"echo-assistant/internal/memory"
	"echo-assistant/internal/netx"
	"echo-assistant/internal/openai"
	"echo-assistant/internal/prompts"
	"echo-assistant/internal/store"
	"echo-assistant/internal/textutil"
	"echo-assistant/internal/tools"
)

const offsetFile = "telegram_offset.json"

// Wire format of the updates we read.
type update struct {
	UpdateID int64     `json:"update_id"`
	Message  *message  `json:"message"`
	Callback *callback `json:"callback_query"`
}

// callback: a tap on an inline button.
type callback struct {
	ID   string `json:"id"`
	From struct {
		ID int64 `json:"id"`
	} `json:"from"`
	Message *struct {
		MessageID int64 `json:"message_id"`
		Chat      struct {
			ID int64 `json:"id"`
		} `json:"chat"`
	} `json:"message"`
	Data string `json:"data"`
}

type message struct {
	From struct {
		ID        int64  `json:"id"`
		FirstName string `json:"first_name"`
		LastName  string `json:"last_name"`
		Username  string `json:"username"`
	} `json:"from"`
	Chat struct {
		ID int64 `json:"id"`
	} `json:"chat"`
	Date    int64  `json:"date"`
	Text    string `json:"text"`
	Caption string `json:"caption"`
	Voice   *struct {
		FileID   string `json:"file_id"`
		Duration int    `json:"duration"`
	} `json:"voice"`
	Photo    []photoSize `json:"photo"`
	Document *struct {
		FileID   string `json:"file_id"`
		FileName string `json:"file_name"`
		MIME     string `json:"mime_type"`
		Size     int    `json:"file_size"`
	} `json:"document"`
}

type photoSize struct {
	FileID string `json:"file_id"`
	Size   int    `json:"file_size"`
}

// parseUpdates reads the "result" of getUpdates.
func parseUpdates(result json.RawMessage) ([]update, error) {
	var updates []update
	err := json.Unmarshal(result, &updates)
	return updates, err
}

func (b *Bot) loadOffset() {
	var doc struct {
		Offset int64 `json:"offset"`
	}
	store.LoadJSON(offsetFile, &doc)
	b.offset = doc.Offset
}

// saveOffset: before acting on an update, so a crash does not run it twice.
func (b *Bot) saveOffset(next int64) {
	b.offset = next
	if err := store.SaveJSON(offsetFile, map[string]int64{"offset": next}); err != nil {
		logf("offset not saved: %v", err)
	}
}

// Run polls Telegram until ctx ends and handles the messages one after the other. Start it only
// when this device is the household's primary one: a second poller with the same bot token (the
// M5) would take updates away from the first. A second call while one runs returns at once.
// Needs the Chat hook for anything but commands.
func (b *Bot) Run(ctx context.Context) {
	if b.token == "" || !b.running.CompareAndSwap(false, true) {
		return
	}
	defer b.running.Store(false)
	b.chats.Restore()
	b.loadOffset()
	pollSeconds := prompts.Int("TELEGRAM_POLL_S", 2)
	retry := time.Duration(prompts.Int("TELEGRAM_RETRY_MS", 30000)) * time.Millisecond
	for ctx.Err() == nil {
		b.chats.Sweep()
		result, err := b.callJSON(ctx, "getUpdates", map[string]any{
			"offset": b.offset, "timeout": pollSeconds, "allowed_updates": []string{"message", "callback_query"},
		}, time.Duration(pollSeconds+10)*time.Second)
		var updates []update
		if err == nil {
			updates, err = parseUpdates(result)
		}
		if err != nil {
			if ctx.Err() != nil {
				return
			}
			b.failures++
			if b.failures == 1 || b.failures%20 == 0 {
				logf("%v (%d. time)", err, b.failures)
			}
			sleep(ctx, retry)
			continue
		}
		if b.failures > 0 {
			logf("reachable again")
		}
		b.failures = 0
		if !b.commandsSet {
			b.commandsSet = true
			b.setCommands(ctx)
			if b.owner != 0 {
				logf("ready")
			} else {
				logf("ready, TELEGRAM_OWNER_ID still missing")
			}
		}
		for _, u := range updates {
			b.handle(ctx, u)
		}
		sleep(ctx, 200*time.Millisecond)
	}
}

func sleep(ctx context.Context, d time.Duration) {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
	case <-t.C:
	}
}

func (b *Bot) setCommands(ctx context.Context) {
	commands := [][2]string{{"neu", "Neues Gespräch beginnen"}, {"aufgaben", "Geplante Aufgaben"},
		{"skills", "Was du mir beigebracht hast"}, {"freigabe", "Aufträge an den Server-Agenten: an, aus"},
		{"hilfe", "Was ich per Telegram kann"}}
	var list []map[string]string
	for _, c := range commands {
		list = append(list, map[string]string{"command": c[0], "description": c[1]})
	}
	if _, err := b.callJSON(ctx, "setMyCommands", map[string]any{"commands": list}, timeout()); err != nil {
		logf("command menu: %v", err)
	}
}

// reply sends HTML to the chat of the message being handled.
func (b *Bot) reply(ctx context.Context, account int64, html string) {
	if sent := b.sendText(ctx, account, html); sent != "sent" {
		logf("answer not delivered: %s", sent)
	}
}

// commandName: "/neu", "/neu@AssistantBot arg" -> "neu".
func commandName(text string) string {
	name := strings.TrimPrefix(text, "/")
	if i := strings.IndexAny(name, " @\n"); i >= 0 {
		name = name[:i]
	}
	return strings.ToLower(name)
}

// command handles "/neu" and the like without the model; false if it is none of ours.
func (b *Bot) command(ctx context.Context, text string, account int64, person string) bool {
	name := commandName(text)
	chat := b.chats.Of(person)
	switch name {
	case "start", "hilfe", "help":
		b.reply(ctx, account, helpText())
	case "neu":
		had := chat.Chatting
		b.chats.End(chat)
		if had {
			b.reply(ctx, account, "Neues Gespräch. Das bisherige habe ich mir gemerkt.")
		} else {
			b.reply(ctx, account, "Neues Gespräch.")
		}
	case "aufgaben":
		out := "Error: tools are not connected"
		if b.tools != nil {
			out = b.tools.Call("list_tasks", "{}", tools.Context{Channel: tools.Agent, Person: person})
		}
		b.reply(ctx, account, "<b>Geplante Aufgaben</b>\n"+escape(out))
	case "skills":
		skills := ""
		if b.mem != nil {
			skills = b.mem.Skills()
		}
		var list strings.Builder
		for _, line := range strings.Split(strings.TrimSuffix(skills, "\n"), "\n") {
			if skills != "" {
				list.WriteString("• " + escape(line) + "\n")
			}
		}
		if skills == "" {
			b.reply(ctx, account, "Noch keine Skills. Bring mir einen bei, etwa: „Wenn ich Guten Morgen schreibe, dann …“")
		} else {
			b.reply(ctx, account, "<b>Skills</b>\n"+list.String())
		}
	case "freigabe": // the server agent's approval mode (#57), the admin's only
		_, arg, _ := strings.Cut(strings.TrimSpace(text), " ")
		out := "Das darf nur der Admin."
		if person == b.reg.Admin() {
			out = "Error: the server agent is not connected"
			if b.Button != nil {
				if result, ok := b.Button("set_approval", strings.TrimSpace(arg)); ok {
					out = result
				}
			}
		}
		b.reply(ctx, account, escape(out))
	default:
		return false
	}
	logf("command /%s", name)
	return true
}

func (b *Bot) typing(ctx context.Context, account int64) {
	b.callJSON(ctx, "sendChatAction", map[string]any{"chat_id": account, "action": "typing"}, timeout())
}

// reportStranger: someone not in the register wrote: the admin hears of it once (with the id he
// needs to register them); the message stays unanswered.
func (b *Bot) reportStranger(ctx context.Context, m *message) {
	from := m.From.ID
	who := strings.TrimSpace(m.From.FirstName + " " + m.From.LastName)
	if m.From.Username != "" {
		who += " (@" + m.From.Username + ")"
	}
	logf("message from unknown id %d (%s)", from, who)
	if b.strangersSeen[from] {
		return
	}
	b.strangersSeen[from] = true
	text := m.Text
	if text == "" {
		text = m.Caption
	}
	if text == "" {
		text = "(ohne Text)"
	}
	text = textutil.Prefix(text, 300)
	id := strconv.FormatInt(from, 10)
	b.strangers += "- " + who + ", Telegram-ID " + id + ", schrieb: „" + text + "“\n"
	if b.mem != nil {
		b.mem.LogEvent("", memory.Telegram, "Unbekanntes Telegram-Konto: "+who, "ID "+id+", schrieb: „"+text+"“")
	}
	sent := b.sendText(ctx, b.owner, "<b>Unbekanntes Telegram-Konto</b>\n"+escape(who)+" (ID <code>"+id+
		"</code>) hat mir geschrieben: „"+escape(text)+
		"“\nIch antworte erst, wenn du die Person eingetragen hast. Sag mir einfach, wer "+
		"das ist (z. B. „Das ist Anna“).")
	if sent != "sent" {
		logf("report to the admin: %s", sent)
	}
}

// parseButton: "mcp:<tool>:<data>" -> tool, data; ok false for anything else.
func parseButton(data string) (tool, rest string, ok bool) {
	after, found := strings.CutPrefix(data, "mcp:")
	if !found {
		return "", "", false
	}
	tool, rest, ok = strings.Cut(after, ":")
	return tool, rest, ok && tool != ""
}

// button handles a tap: only the admin's taps in his own chat count (the approval of a server
// task, #57); the buttons go away and the outcome is the answer to that message.
func (b *Bot) button(ctx context.Context, c *callback) {
	answer := func(text string) {
		b.callJSON(ctx, "answerCallbackQuery", map[string]any{"callback_query_id": c.ID, "text": text}, timeout())
	}
	tool, data, ok := parseButton(c.Data)
	if c.From.ID != b.owner || c.Message == nil || c.Message.Chat.ID != b.owner || !ok {
		logf("button from %d ignored", c.From.ID)
		answer("")
		return
	}
	result := "Error: the buttons are not connected"
	if b.Button != nil {
		if out, found := b.Button(tool, data); found {
			result = out
		} else {
			result = "Error: no connected server knows this button"
		}
	}
	logf("button %s: %.200s", tool, result)
	answer(textutil.Prefix(result, 190))
	b.callJSON(ctx, "editMessageReplyMarkup", map[string]any{"chat_id": c.Message.Chat.ID,
		"message_id": c.Message.MessageID, "reply_markup": map[string]any{"inline_keyboard": [][]any{}}}, timeout())
	b.callJSON(ctx, "sendMessage", map[string]any{"chat_id": c.Message.Chat.ID, "text": escape(result),
		"parse_mode": "HTML", "reply_parameters": map[string]any{"message_id": c.Message.MessageID}}, timeout())
}

// handle runs one update.
func (b *Bot) handle(ctx context.Context, u update) {
	b.saveOffset(u.UpdateID + 1) // before acting: a crash must not run it twice
	if u.Callback != nil {
		b.button(ctx, u.Callback)
		return
	}
	m := u.Message
	if m == nil {
		return
	}
	from, chatID := m.From.ID, m.Chat.ID
	if b.owner == 0 { // setup: tell the sender his id
		logf("message from id %d (TELEGRAM_OWNER_ID not set)", from)
		b.sendText(ctx, chatID, fmt.Sprintf("Hallo! Deine Telegram-ID ist <code>%d</code>. Trag sie in secrets.h als "+
			"TELEGRAM_OWNER_ID ein und spiel die Firmware neu auf; danach höre ich nur noch auf dich.", from))
		return
	}
	person := b.reg.ByTelegram(from)
	if chatID != from { // a group: nobody's own chat
		logf("message from group %d ignored", chatID)
		return
	}
	if person == "" && strings.HasPrefix(m.Text, "/start ") { // the link of a welcome mail
		if person = b.reg.ClaimInvite(m.Text[7:], from); person != "" {
			name := b.reg.Name(person)
			b.reply(ctx, from, "Hallo "+escape(name)+"! Ab jetzt erkenne ich Sie hier. Schreiben Sie mir "+
				"einfach, schicken Sie Sprachnachrichten, Fotos oder Dateien.")
			b.sendText(ctx, b.owner, escape(name)+" hat Telegram mit "+prompts.Name()+" verbunden.")
			return
		}
	}
	if person == "" {
		b.reportStranger(ctx, m)
		return
	}
	sent := time.Unix(m.Date, 0)
	if netx.TimeSynced && time.Since(sent) > time.Duration(prompts.Int("TELEGRAM_MAX_AGE_S", 7200))*time.Second {
		b.reply(ctx, from, "Deine Nachricht von "+sent.Format("02.01. 15:04")+
			" kam an, als ich nicht erreichbar war. Schick sie bitte noch einmal, wenn sie noch gilt.")
		return
	}
	text := m.Text
	if strings.HasPrefix(text, "/") && b.command(ctx, text, from, person) {
		return
	}
	voice, photo, document := m.Voice != nil, len(m.Photo) > 0, m.Document != nil
	if !voice && !photo && !document && text == "" {
		b.reply(ctx, from, "Das kann ich noch nicht lesen. Schick mir Text, eine Sprachnachricht, ein Foto oder eine Datei.")
		return
	}
	b.typing(ctx, from)
	var image string
	var file *chatcore.Attachment
	var err error
	switch {
	case voice:
		text, err = b.transcribeVoice(ctx, m.Voice.FileID, m.Voice.Duration)
	case photo:
		image, err = b.loadPhoto(ctx, m.Photo)
		text = m.Caption
		if text == "" {
			text = "Was ist auf diesem Bild?"
		}
	}
	if document {
		file, err = b.loadDocument(ctx, m)
		if err == nil {
			// Kept in Drive, so it can be printed or passed on later ("druck das PDF aus").
			if b.StoreFile != nil {
				if kept := b.StoreFile(file.Name, file.MIME, file.Data, "Telegram von "+b.reg.Name(person)); kept != "" {
					logf("%s not kept: %s", file.Name, kept)
				}
			}
		}
		text = m.Caption
		if text == "" {
			text = "Schau dir diese Datei an und fasse zusammen, was drinsteht."
		}
	}
	if err != nil {
		b.reply(ctx, from, escape("Fehler: "+err.Error()))
		return
	}
	b.converse(ctx, person, from, text, voice, image, file)
}

// converse is chat() of the M5: a message of a person of the household. text is typed,
// transcribed or a photo's caption; voice: it came as a voice note (the answer comes as one,
// too); image: a photo as data URL.
func (b *Bot) converse(ctx context.Context, person string, account int64, text string, voice bool, image string, file *chatcore.Attachment) {
	chat := b.chats.Of(person)
	b.chats.Start(chat)
	tag := ""
	if voice {
		tag = " (voice)"
	}
	if image != "" {
		tag += " (photo)"
	}
	logf("%s%s: %.300s", person, tag, text)
	started := time.Now()
	task := text
	if voice {
		task = "[Sprachnachricht] " + text
	}
	if b.strangers != "" && person == b.reg.Admin() { // he can have them registered now
		task = "(System note, not from " + b.reg.Name(person) + ": these Telegram accounts are not in the " +
			"people register and wrote to the bot; not answered. Register or assign them only when asked, with " +
			"add_person or update_person:\n" + b.strangers + ")\n\n" + task
		b.strangers = ""
	}
	answer := "Error: the chat is not connected"
	if b.Chat != nil {
		answer = b.Chat(chatcore.Request{Ctx: ctx, Channel: "Telegram", Person: person, Task: task, Text: text,
			Voice: voice, Image: image, File: file, Instructions: prompts.Text("CHAT_INSTRUCTIONS"), Thread: &chat.Thread})
	}
	logf("answer after %d s: %.300s", int(time.Since(started).Seconds()), answer)
	failed := strings.HasPrefix(answer, "Error")
	var audio []byte
	var audioErr error
	if voice && !failed {
		audio, audioErr = openai.Speech(ctx, spoken(answer, prompts.Int("TELEGRAM_SPOKEN_CHARS", 1500)), "opus")
		if audioErr != nil {
			logf("%v", audioErr)
		}
	}
	b.chats.Record(chat, chatcore.How("Telegram", voice, image, file), text, answer)
	// Voice: what the assistant understood first, so a misheard word shows.
	html := answer
	if failed {
		html = escape(answer)
	}
	if voice {
		html = "<i>„" + escape(text) + "“</i>\n\n" + html
	}
	b.reply(ctx, account, html)
	if voice && !failed && audioErr == nil {
		if sent := b.sendFile(ctx, account, "sendVoice", "voice", "assistant.ogg", "audio/ogg", audio, ""); sent != "sent" {
			logf("voice answer: %s", sent)
		}
	}
	b.chats.Finish(chat)
}

// transcribeVoice: a voice note as text.
func (b *Bot) transcribeVoice(ctx context.Context, fileID string, duration int) (string, error) {
	if limit := prompts.Int("TELEGRAM_VOICE_MAX_S", 300); duration > limit {
		return "", fmt.Errorf("Die Sprachnachricht ist zu lang (höchstens %d Minuten)", limit/60)
	}
	audio, err := b.download(ctx, fileID, 3*1024*1024)
	if err != nil {
		return "", err
	}
	started := time.Now()
	text, err := openai.Transcribe(ctx, audio, "voice.ogg")
	logf("voice note %d s, %d bytes, recognized after %d ms", duration, len(audio), time.Since(started).Milliseconds())
	return text, err
}

// pickPhoto: the largest size of a photo (Telegram sends several, smallest first) that fits
// the limit; "" if none does.
func pickPhoto(sizes []photoSize, limit int) string {
	id := ""
	for _, s := range sizes {
		if s.Size <= limit {
			id = s.FileID
		}
	}
	return id
}

// loadPhoto: the photo as data URL for the model.
func (b *Bot) loadPhoto(ctx context.Context, sizes []photoSize) (string, error) {
	limit := prompts.Int("TELEGRAM_PHOTO_BYTES", 1024*1024)
	id := pickPhoto(sizes, limit)
	if id == "" {
		return "", fmt.Errorf("Das Foto ist zu groß")
	}
	jpeg, err := b.download(ctx, id, limit)
	if err != nil {
		return "", err
	}
	logf("photo %d bytes", len(jpeg))
	return chatcore.DataURL("image/jpeg", jpeg), nil
}

// loadDocument: a document (text file, PDF or image) a person sent.
func (b *Bot) loadDocument(ctx context.Context, m *message) (*chatcore.Attachment, error) {
	d := m.Document
	name := d.FileName
	if name == "" {
		name = "Datei"
	}
	if !chatcore.Readable(d.MIME, name) {
		return nil, fmt.Errorf("Mit dieser Datei (%s) kann ich nichts anfangen. Ich lese Textdateien (JSON, CSV, TXT, …) und PDFs.", name)
	}
	limit := prompts.Int("TELEGRAM_DOCUMENT_BYTES", 1024*1024)
	if d.Size > limit {
		return nil, fmt.Errorf("Die Datei ist zu groß (höchstens %d KB)", limit/1024)
	}
	data, err := b.download(ctx, d.FileID, limit)
	if err != nil {
		return nil, err
	}
	logf("file %s (%s), %d bytes", name, d.MIME, len(data))
	return &chatcore.Attachment{Name: name, MIME: d.MIME, Data: data}, nil
}
