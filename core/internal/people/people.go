// Package people: the people of the household who use the assistant, who someone is and what it may
// do, as devices/atoms3r/src/assistant/people.cpp. Profiles (preferences, birthday) live in the
// memory, not here. The admin (OWNER_NAME with PHONE_OWNER, TELEGRAM_OWNER_ID and OWNER_EMAIL)
// comes from the secrets on every start, so nobody can make himself admin. Members are added by
// the admin (add_person, ...). Stored as people.json (same format as the M5's), backed up and
// restored from the backup when the file is missing.
//
// A person is known for sure on the phone (number), on Telegram (account) and by mail (address);
// at home only from the conversation (nobody = unknown, "", and the house is trusted).
package people

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log"
	"slices"
	"strings"
	"sync"
	"time"

	"echo-assistant/internal/backup"
	"echo-assistant/internal/config"
	"echo-assistant/internal/prompts"
	"echo-assistant/internal/store"
	"echo-assistant/internal/textutil"
	"echo-assistant/internal/tools"
)

const (
	filePath   = "people.json"
	backupFile = "people.json" // in Drive
	maxPeople  = 10
)

type Person struct {
	ID       string   `json:"id"`
	Name     string   `json:"name"`
	Role     string   `json:"role"` // "admin" or "member"
	Phones   []string `json:"phones"`
	Telegram int64    `json:"telegram"`
	Email    string   `json:"email"`
	Blocked  []string `json:"blocked"`
	Humor    *int     `json:"humor,omitempty"` // nil = PERSONA_LEVEL
	Child    bool     `json:"child,omitempty"`
	Invite   string   `json:"invite,omitempty"` // code of the Telegram link in the welcome mail
	Notify   string   `json:"notify,omitempty"` // "telegram", "whatsapp", "" = automatic
}

func (p Person) Admin() bool { return p.Role == "admin" }

// Channels to the people; set by the services that exist (nil = not configured).
type Channels struct {
	Telegram   func(html, person string) string // "sent" or an error
	WhatsApp   func(text, person string) string
	InviteLink func(code string) string // Telegram deep link for a welcome mail
	// WhatsAppLink: the household's WhatsApp number as shown and its wa.me link, for a welcome
	// mail; "" when unknown.
	WhatsAppLink func() (number, link string)
	MailSend     func(to, subject, html string) string
	MailAddress  func() string // The assistant's own address, "" without mailbox
	MailCommands func() bool   // the mailbox takes commands by mail
}

type Register struct {
	owner          string
	ownerPhone     string
	ownerTG        int64
	ownerMail      string
	assistantPhone string
	Channels       Channels
	// OnMessage, if set, sees every message to a person (in its own goroutine), before it goes
	// to the phone: e.g. a hint on the TVs that are on (#108).
	OnMessage func(name, text string)

	mu             sync.Mutex
	list           []Person
	backupPending  bool
	restorePending bool
}

func New(s config.Secrets) *Register {
	r := &Register{owner: s.String("OWNER_NAME"), ownerPhone: s.String("PHONE_OWNER"),
		ownerMail: s.String("OWNER_EMAIL"), assistantPhone: s.String("ASSISTANT_PHONE_NUMBER")}
	if v, ok := s["TELEGRAM_OWNER_ID"].(float64); ok {
		r.ownerTG = int64(v)
	}
	if r.owner == "" {
		r.owner = "Admin"
	}
	var doc struct {
		People []Person `json:"people"`
	}
	loaded := store.LoadJSON(filePath, &doc)
	r.mu.Lock()
	r.list = valid(doc.People)
	r.ensureAdminLocked()
	// Only the admin: a new device. It looks for the backup even if a start without it already
	// wrote the file (16.10.2026 the move to the Echo lost the household that way).
	r.restorePending = !loaded || len(r.list) == 1
	if !loaded {
		r.saveLocked()
	}
	n := len(r.list)
	r.mu.Unlock()
	log.Printf("[People] %d registered%s", n, map[bool]string{true: ", looking for the backup"}[!loaded])
	return r
}

func valid(in []Person) []Person {
	var out []Person
	for _, p := range in {
		if p.ID != "" && p.Name != "" {
			if p.Role == "" {
				p.Role = "member"
			}
			out = append(out, p)
		}
	}
	return out
}

func slug(name string) string { return textutil.Slug(name, 24) }
func lower(s string) string   { return strings.ToLower(strings.TrimSpace(s)) }

// --- Storage (caller holds the lock) ---------------------------------------------------

func (r *Register) findLocked(id string) *Person {
	for i := range r.list {
		if r.list[i].ID == id {
			return &r.list[i]
		}
	}
	return nil
}

func (r *Register) findByNameLocked(name string) *Person {
	wanted, wantedID := lower(name), slug(name)
	if wanted == "" {
		return nil
	}
	for i := range r.list {
		p := &r.list[i]
		if lower(p.Name) == wanted || p.ID == wanted || p.ID == wantedID {
			return p
		}
	}
	return nil
}

// ensureAdminLocked: the admin from the secrets: nobody else is admin, his accounts are the
// secrets'.
func (r *Register) ensureAdminLocked() {
	id := slug(r.owner)
	admin := r.findLocked(id)
	if admin == nil {
		r.list = append([]Person{{ID: id, Name: r.owner, Role: "admin"}}, r.list...)
		admin = &r.list[0]
	}
	for i := range r.list {
		r.list[i].Role = "member"
	}
	admin.Role = "admin"
	if phone := textutil.PhoneNormalized(r.ownerPhone); phone != "" && !slices.Contains(admin.Phones, phone) {
		admin.Phones = append([]string{phone}, admin.Phones...)
	}
	if r.ownerTG != 0 {
		admin.Telegram = r.ownerTG
	}
	admin.Email = r.ownerMail
	admin.Blocked, admin.Child = nil, false
}

func (r *Register) docLocked() []byte {
	list := make([]Person, len(r.list))
	copy(list, r.list)
	for i := range list {
		if list[i].Phones == nil {
			list[i].Phones = []string{}
		}
		if list[i].Blocked == nil {
			list[i].Blocked = []string{}
		}
	}
	data, _ := json.MarshalIndent(map[string]any{"people": list}, "", " ")
	return data
}

func (r *Register) saveLocked() {
	if err := store.Write(filePath, r.docLocked()); err != nil {
		log.Printf("[People] saving failed: %v", err)
	}
	r.backupPending = backup.Available() && !r.restorePending // an incomplete list must not replace the backup
}

// Background: restore from the backup, or back up after changes. Call between conversations.
func (r *Register) Background() {
	r.mu.Lock()
	restore, pending := r.restorePending, r.backupPending
	r.mu.Unlock()
	if !backup.Available() {
		return
	}
	if restore {
		r.restore()
	} else if pending {
		r.backup()
	}
}

func (r *Register) backup() {
	r.mu.Lock()
	text := "Personenregister von " + prompts.Name() + ", Stand " + time.Now().Format("02.01.2006 15:04") +
		". Stellt es nach einem Gerätewechsel wieder her; bitte nicht bearbeiten.\n\n"
	for _, p := range r.list {
		text += "- " + r.describe(p) + "\n"
	}
	doc := r.docLocked()
	r.backupPending = false
	r.mu.Unlock()
	if err := backup.Store(backupFile, backupSubject(), text, doc); err != nil {
		log.Printf("[People] backup: %v", err)
		r.mu.Lock()
		r.backupPending = true
		r.mu.Unlock()
	}
}

func (r *Register) restore() {
	data, err := backup.Load(backupFile, backupSubject())
	if err != nil {
		log.Printf("[People] restore: %v", err)
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.restorePending = false
	var doc struct {
		People []Person `json:"people"`
	}
	if data == nil || json.Unmarshal(data, &doc) != nil || len(r.list) > 1 {
		r.saveLocked() // no backup yet, or added meanwhile: the current list becomes it
		return
	}
	r.list = valid(doc.People)
	r.ensureAdminLocked()
	r.saveLocked()
	log.Printf("[People] restored from the backup: %d", len(r.list))
}

// --- Lookups ---------------------------------------------------------------------------

func (r *Register) humorOf(p Person) int {
	maxLevel := prompts.Int("PERSONA_MAX_LEVEL", 3)
	if p.Humor != nil && *p.Humor >= 0 {
		return min(*p.Humor, maxLevel)
	}
	return min(max(prompts.Int("PERSONA_LEVEL", 2), 0), maxLevel)
}

// Humor: The assistant's cheekiness with a person (0..PERSONA_MAX_LEVEL).
func (r *Register) Humor(p Person) int { return r.humorOf(p) }

func (r *Register) describe(p Person) string {
	join := func(items []string) string { return strings.Join(items, ", ") }
	phones := "-"
	if len(p.Phones) > 0 {
		phones = join(p.Phones)
	}
	tg := "- (unknown)"
	if p.Telegram != 0 {
		tg = fmt.Sprint(p.Telegram)
	}
	mail := "-"
	if p.Email != "" {
		mail = p.Email
	}
	admin := ""
	if p.Admin() {
		admin = ", admin"
	}
	text := fmt.Sprintf("%s (id %s%s): phone %s, Telegram %s, mail %s", p.Name, p.ID, admin, phones, tg, mail)
	if len(p.Blocked) > 0 {
		text += ", blocked: " + join(p.Blocked)
	}
	if p.Child {
		text += ", child (child-friendly)"
	} else {
		text += fmt.Sprintf(", humour %d", r.humorOf(p))
		if p.Humor == nil || *p.Humor < 0 {
			text += " (default)"
		}
	}
	notify := p.Notify
	if notify == "" {
		notify = "auto"
	}
	return text + ", notices via " + notify
}

// Find by id; false if unknown ("" = nobody).
func (r *Register) Find(id string) (Person, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if p := r.findLocked(id); id != "" && p != nil {
		return *p, true
	}
	return Person{}, false
}

func (r *Register) All() []Person {
	r.mu.Lock()
	defer r.mu.Unlock()
	return slices.Clone(r.list)
}

// Admin: the admin's id.
func (r *Register) Admin() string { return slug(r.owner) }

// Name of a person ("" if unknown).
func (r *Register) Name(id string) string {
	p, _ := r.Find(id)
	return p.Name
}

// ByName: id by name or id (ignoring case), "" if none.
func (r *Register) ByName(name string) string {
	r.mu.Lock()
	defer r.mu.Unlock()
	if p := r.findByNameLocked(name); p != nil {
		return p.ID
	}
	return ""
}

func (r *Register) ByPhone(number string) string {
	wanted := textutil.PhoneNormalized(number)
	if wanted == "" {
		return ""
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, p := range r.list {
		if slices.Contains(p.Phones, wanted) {
			return p.ID
		}
	}
	return ""
}

func (r *Register) ByTelegram(account int64) string {
	if account == 0 {
		return ""
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, p := range r.list {
		if p.Telegram == account {
			return p.ID
		}
	}
	return ""
}

func (r *Register) ByEmail(address string) string {
	wanted := lower(address)
	if wanted == "" {
		return ""
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, p := range r.list {
		if p.Email != "" && lower(p.Email) == wanted {
			return p.ID
		}
	}
	return ""
}

// ClaimInvite: a person opened the Telegram link of their welcome mail ("/start <code>"): their
// account is linked and the code used up. Returns the id, "" if no person has this code.
func (r *Register) ClaimInvite(code string, telegram int64) string {
	key := strings.TrimSpace(code)
	if len(key) < 8 || telegram == 0 {
		return ""
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, p := range r.list {
		if p.Telegram == telegram {
			return "" // the account belongs to someone already
		}
	}
	for i := range r.list {
		if r.list[i].Invite == key {
			r.list[i].Telegram, r.list[i].Invite = telegram, ""
			r.saveLocked()
			log.Printf("[People] %s linked Telegram through the welcome mail", r.list[i].Name)
			return r.list[i].ID
		}
	}
	return ""
}

// Denied: whether person may run tool (private: the admin's own accounts). Unknown people ("",
// at home) may run everything. Returns "" or the error for the model.
func (r *Register) Denied(person, tool string, private bool) string {
	if person == "" {
		return "" // at home, nobody named: the house is trusted
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	p := r.findLocked(person)
	if p != nil && p.Admin() {
		return ""
	}
	if p != nil && slices.Contains(p.Blocked, tool) {
		return "Error: " + tool + " is blocked for " + p.Name + "."
	}
	if !private {
		return ""
	}
	name := "the admin"
	if owner := r.findLocked(slug(r.owner)); owner != nil {
		name = owner.Name
	}
	return "Error: only " + name + " may do that (their own accounts and the administration of the assistant)."
}

// chatOrder: the chat channels that reach p, the preferred one first: the one set in notify,
// else Telegram when their account is known, else WhatsApp. Empty if neither reaches them.
func (r *Register) chatOrder(p Person) []string {
	viaTelegram := r.Channels.Telegram != nil && p.Telegram != 0
	viaWhatsApp := r.Channels.WhatsApp != nil && len(p.Phones) > 0
	order := []string{"telegram", "whatsapp"}
	if p.Notify == "whatsapp" || (p.Notify == "" && !viaTelegram) {
		order = []string{"whatsapp", "telegram"}
	}
	var out []string
	for _, c := range order {
		if c == "telegram" && viaTelegram || c == "whatsapp" && viaWhatsApp {
			out = append(out, c)
		}
	}
	return out
}

// ChatOrder: the chat channels ("telegram", "whatsapp") that reach a person ("" = the admin),
// preferred first; nil if none does or the person is unknown.
func (r *Register) ChatOrder(person string) []string {
	if person == "" {
		person = r.Admin()
	}
	p, ok := r.Find(person)
	if !ok {
		return nil
	}
	return r.chatOrder(p)
}

// ChatChannel: the preferred chat channel of a person ("" = the admin), "" if none reaches them.
func (r *Register) ChatChannel(person string) string {
	if order := r.ChatOrder(person); len(order) > 0 {
		return order[0]
	}
	return ""
}

// Message sends text (plain, no markup) to a person ("" = the admin) on their channel for
// notices (chatOrder), the other one if that fails. Returns "telegram" / "whatsapp" or an error.
func (r *Register) Message(person, text string) string {
	if person == "" {
		person = r.Admin()
	}
	p, ok := r.Find(person)
	if !ok {
		return "Error: this person is not registered."
	}
	if r.OnMessage != nil {
		go r.OnMessage(p.Name, text)
	}
	order := r.chatOrder(p)
	var errs []string
	for _, channel := range order {
		var result string
		if channel == "telegram" {
			result = r.Channels.Telegram(textutil.HTML(text), p.ID)
		} else {
			result = r.Channels.WhatsApp(text, p.ID)
		}
		if result == "sent" {
			return channel
		}
		errs = append(errs, channel+": "+result)
	}
	if len(errs) > 0 {
		return "Error: " + strings.Join(errs, "; ")
	}
	return "Error: " + p.Name + " has neither Telegram nor WhatsApp."
}

// Household: who lives here, for the prompts; "" without members.
func (r *Register) Household() string {
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(r.list) < 2 {
		return ""
	}
	var names []string
	for _, p := range r.list {
		n := p.Name
		if p.Admin() {
			n += " (admin)"
		}
		names = append(names, n)
	}
	return "People of the household who use the assistant: " + strings.Join(names, ", ") + ".\n"
}

// --- Tools -----------------------------------------------------------------------------

func stringsArg(args map[string]any, name string) []string {
	items, _ := args[name].([]any)
	var out []string
	for _, item := range items {
		if s, ok := item.(string); ok {
			out = append(out, s)
		}
	}
	return out
}

func int64Arg(args map[string]any, name string) int64 {
	v, _ := args[name].(float64)
	return int64(v)
}

// checkPhones normalizes the numbers; "" or the error.
func (r *Register) checkPhones(given []string, except string) ([]string, string) {
	var phones []string
	for _, value := range given {
		number := textutil.PhoneNormalized(value)
		if number == "" {
			continue
		}
		if !strings.HasPrefix(number, "0") || len(number) < 6 {
			return nil, "Error: " + value + " is not a complete phone number (with area code)."
		}
		for _, p := range r.list {
			if p.ID != except && slices.Contains(p.Phones, number) {
				return nil, "Error: the number " + number + " already belongs to " + p.Name + "."
			}
		}
		if !slices.Contains(phones, number) {
			phones = append(phones, number)
		}
	}
	return phones, ""
}

func (r *Register) checkTelegram(account int64, except string) string {
	if account == 0 {
		return ""
	}
	for _, p := range r.list {
		if p.ID != except && p.Telegram == account {
			return "Error: this Telegram account already belongs to " + p.Name + "."
		}
	}
	return ""
}

func (r *Register) checkEmail(email *string, except string) string {
	*email = lower(*email)
	if *email == "" {
		return ""
	}
	if strings.Index(*email, "@") < 1 || strings.Contains(*email, " ") {
		return "Error: " + *email + " is not an e-mail address."
	}
	for _, p := range r.list {
		if p.ID != except && lower(p.Email) == *email {
			return "Error: the e-mail address already belongs to " + p.Name + "."
		}
	}
	return ""
}

func newInvite() string {
	b := make([]byte, 6)
	rand.Read(b)
	return hex.EncodeToString(b)
}

// welcome: the mail to a person on how to reach the assistant (German: the person reads it). Returns
// "sent", "" without mailbox or address, or an error.
func (r *Register) welcome(p Person) string {
	c := r.Channels
	if c.MailSend == nil || p.Email == "" {
		return ""
	}
	h := textutil.HTML
	var ways string
	if p.Telegram == 0 && c.InviteLink != nil {
		if link := c.InviteLink(p.Invite); link != "" {
			ways += "<li><b>Telegram:</b> <a href=\"" + link + "\">diesen Link öffnen</a> und auf „Starten“ tippen. " +
				"Danach erkennt " + prompts.Name() + " Sie dort; Sie können ihm schreiben und Sprachnachrichten, Fotos oder Dateien " +
				"schicken.</li>"
		}
	}
	if c.WhatsAppLink != nil && len(p.Phones) > 0 {
		if number, link := c.WhatsAppLink(); link != "" {
			ways += "<li><b>WhatsApp:</b> <a href=\"" + link + "\">diesen Link öffnen</a> oder " + h(number) +
				" als Kontakt speichern und " + prompts.Name() + " schreiben. Von Ihrer Nummer " + h(p.Phones[0]) + " aus erkennt er " +
				"Sie; Sprachnachrichten, Fotos und Dateien gehen auch.</li>"
		}
	}
	if r.assistantPhone != "" && len(p.Phones) > 0 {
		ways += "<li><b>Telefon:</b> " + h(r.assistantPhone) + " anrufen. Von Ihrer Nummer " + h(p.Phones[0]) +
			" aus erkennt er Sie.</li>"
	}
	if c.MailCommands != nil && c.MailCommands() && c.MailAddress != nil {
		ways += "<li><b>E-Mail:</b> an " + h(c.MailAddress()) + " schreiben, was er erledigen soll; er antwortet " +
			"per Mail. Das gilt nur für Mails von " + h(p.Email) + ".</li>"
	}
	ways += "<li><b>Zu Hause:</b> „Hey Jarvis“ sagen.</li>"
	body := "<p>Hallo " + h(p.Name) + ",</p><p>" + h(r.owner) +
		" hat Sie bei " + prompts.Name() + " freigeschaltet, dem Sprachassistenten Ihres Haushalts. So erreichen Sie " +
		"ihn:</p><ul>" + ways +
		"</ul><p>" + prompts.Name() + " beantwortet Fragen, erinnert an Termine, schickt Nachrichten, steuert das Smart " +
		"Home und merkt sich, was Sie ihm erzählen. Auf die persönlichen Konten von " +
		h(r.owner) + " (Kalender, Kontakte, Postfach) hat er in Gesprächen mit Ihnen keinen " +
		"Zugriff.</p><p>Viele Grüße<br>" + prompts.Name() + "</p>"
	return c.MailSend(p.Email, "Willkommen bei "+prompts.Name(), body)
}

func (r *Register) welcomeResult(p Person) string {
	sent := r.welcome(p)
	if sent == "" {
		if p.Email == "" {
			return " No welcome mail (no e-mail address)."
		}
		return " No welcome mail (no mailbox)."
	}
	if sent == "sent" {
		if p.Telegram != 0 {
			return " Welcome mail sent to " + p.Email + "."
		}
		return " Welcome mail sent to " + p.Email + " (with the Telegram link)."
	}
	return " Welcome mail failed: " + sent
}

func (r *Register) addPerson(args map[string]any) string {
	name := tools.String(args, "name")
	id := slug(name)
	if id == "" {
		return "Error: name missing"
	}
	r.mu.Lock()
	if len(r.list) >= maxPeople {
		r.mu.Unlock()
		return fmt.Sprintf("Error: already %d people registered", maxPeople)
	}
	if r.findLocked(id) != nil || r.findByNameLocked(name) != nil {
		r.mu.Unlock()
		return "Error: " + name + " is already registered."
	}
	email := tools.String(args, "email")
	phones, errText := r.checkPhones(stringsArg(args, "phones"), id)
	account := int64Arg(args, "telegram")
	if errText == "" {
		errText = r.checkTelegram(account, id)
	}
	if errText == "" {
		errText = r.checkEmail(&email, id)
	}
	if errText != "" {
		r.mu.Unlock()
		return errText
	}
	p := Person{ID: id, Name: name, Role: "member", Phones: phones, Telegram: account, Email: email}
	if p.Telegram == 0 && r.Channels.Telegram != nil {
		p.Invite = newInvite()
	}
	r.list = append(r.list, p)
	r.saveLocked()
	r.mu.Unlock()
	log.Printf("[People] added: %s", r.describe(p))
	return "Registered: " + r.describe(p) + "." + r.welcomeResult(p)
}

func (r *Register) sendWelcome(who string) string {
	r.mu.Lock()
	found := r.findByNameLocked(who)
	if found == nil {
		r.mu.Unlock()
		return "Error: this person is not registered."
	}
	if found.Admin() {
		r.mu.Unlock()
		return "Error: the admin needs no welcome mail."
	}
	if found.Telegram == 0 && r.Channels.Telegram != nil {
		found.Invite = newInvite() // a new link; an old one stops working
		r.saveLocked()
	}
	p := *found
	r.mu.Unlock()
	result := r.welcomeResult(p)
	if strings.Contains(result, "sent to") {
		return result[1:]
	}
	return "Error:" + result
}

func (r *Register) updatePerson(args map[string]any) string {
	r.mu.Lock()
	defer r.mu.Unlock()
	p := r.findByNameLocked(tools.String(args, "person"))
	if p == nil {
		return "Error: this person is not registered."
	}
	changed := *p
	if name := tools.String(args, "name"); name != "" && name != p.Name {
		if p.Admin() {
			return "Error: the admin's name is set by the firmware."
		}
		if other := r.findByNameLocked(name); other != nil && other != p {
			return "Error: " + name + " is already registered."
		}
		changed.Name = name // the id stays: the memory's profile hangs on it
	}
	email := tools.String(args, "email")
	phones, errText := r.checkPhones(stringsArg(args, "phones"), p.ID)
	account := int64Arg(args, "telegram")
	if errText == "" {
		errText = r.checkTelegram(account, p.ID)
	}
	if errText == "" {
		errText = r.checkEmail(&email, p.ID)
	}
	if errText != "" {
		return errText
	}
	changed.Phones, changed.Telegram, changed.Email = phones, account, email
	changed.Child = tools.Bool(args, "child", false)
	switch notify := tools.String(args, "notify"); notify {
	case "telegram", "whatsapp":
		changed.Notify = notify
	default:
		changed.Notify = ""
	}
	changed.Blocked = nil
	for _, tool := range stringsArg(args, "blocked") {
		if name := lower(tool); name != "" {
			changed.Blocked = append(changed.Blocked, name)
		}
	}
	*p = changed
	r.ensureAdminLocked() // the admin's own accounts stay the secrets'
	r.saveLocked()
	log.Printf("[People] changed: %s", r.describe(*p))
	return "Changed: " + r.describe(*p)
}

// setHumor: "sei frecher zu mir", "sei bei Max netter": the admin for anyone, others only for
// themselves, children not at all.
func (r *Register) setHumor(who string, level int, ctx tools.Context) string {
	r.mu.Lock()
	defer r.mu.Unlock()
	var target *Person
	if who != "" {
		target = r.findByNameLocked(who)
	} else {
		target = r.findLocked(ctx.Person)
	}
	if target == nil {
		if who != "" {
			return "Error: " + who + " is not registered."
		}
		return "Error: for whom? Give person."
	}
	asking := r.findLocked(ctx.Person)
	if asking != nil && !asking.Admin() && (asking != target || asking.Child) {
		return "Error: only " + r.owner + " may do that."
	}
	if target.Child {
		return "Error: The assistant talks child-friendly with " + target.Name + " (a child in the register)."
	}
	if level < 0 {
		target.Humor = nil
	} else {
		l := min(level, prompts.Int("PERSONA_MAX_LEVEL", 3))
		target.Humor = &l
	}
	r.saveLocked()
	names := []string{"polite", "dry", "cheeky", "biting"}
	effective := r.humorOf(*target)
	log.Printf("[People] humour for %s: %d", target.ID, effective)
	text := fmt.Sprintf("Humour for %s: level %d (%s)", target.Name, effective, names[min(effective, 3)])
	if target.Humor == nil {
		text += ", the default"
	}
	return text + ". Applies from the next conversation on."
}

func (r *Register) removePerson(who string) string {
	r.mu.Lock()
	defer r.mu.Unlock()
	p := r.findByNameLocked(who)
	if p == nil {
		return "Error: this person is not registered."
	}
	if p.Admin() {
		return "Error: nobody can remove the admin."
	}
	name := p.Name
	r.list = slices.DeleteFunc(r.list, func(q Person) bool { return q.ID == p.ID })
	r.saveLocked()
	log.Printf("[People] removed: %s", name)
	return name + " is removed (the memory profile stays)."
}

func (r *Register) list_() string {
	r.mu.Lock()
	defer r.mu.Unlock()
	var b strings.Builder
	for _, p := range r.list {
		b.WriteString("- " + r.describe(p) + "\n")
	}
	return b.String()
}

// AddTools registers list_people, add_person, send_welcome, update_person, set_humor and
// remove_person.
func (r *Register) AddTools(reg *tools.Registry) {
	tools.Denied = r.Denied
	reg.Add(tools.Tool{
		Name: "list_people",
		Description: "Lists the people registered with the assistant (the household): name, id, phone numbers, Telegram account, " +
			"e-mail, blocked tools.",
		Parameters: `{"type": "object", "additionalProperties": false, "properties": {}, "required": []}`,
		Run:        func(map[string]any, tools.Context) string { return r.list_() },
	})
	reg.Add(tools.Tool{
		Name: "add_person",
		Description: "Registers a new member of the household so the assistant recognizes them by phone number, Telegram account or " +
			"e-mail, e.g. 'gib Kontakt X frei': take name, numbers and e-mail address from find_contact. Only the " +
			"admin may. First read all details out and get its yes. The person then gets a welcome e-mail with the " +
			"ways to reach the assistant and a Telegram link that connects their account (telegram 0 until then).",
		Parameters: `{"type": "object", "additionalProperties": false,
            "properties": {"name": {"type": "string", "description": "first name as the household uses it"},
                           "phones": {"type": "array", "items": {"type": "string"}, "description": "mobile and other numbers with area code"},
                           "telegram": {"type": "integer", "description": "numeric Telegram user id, 0 = unknown"},
                           "email": {"type": "string", "description": "'' = none"}},
            "required": ["name", "phones", "telegram", "email"]}`,
		Private: true,
		Run:     func(args map[string]any, _ tools.Context) string { return r.addPerson(args) },
	})
	reg.Add(tools.Tool{
		Name: "send_welcome",
		Description: "Sends a registered person the welcome e-mail again (ways to reach the assistant, a new Telegram link while their " +
			"account is not connected), e.g. for people registered before welcome mails existed. Only the admin may.",
		Parameters: `{"type": "object", "additionalProperties": false,
            "properties": {"person": {"type": "string", "description": "name or id of the person"}},
            "required": ["person"]}`,
		Private: true,
		Run:     func(args map[string]any, _ tools.Context) string { return r.sendWelcome(tools.String(args, "person")) },
	})
	reg.Add(tools.Tool{
		Name: "update_person",
		Description: "Changes a registered person. Gives the complete new entry: take the current values from list_people " +
			"for everything that stays. Only the admin may; read the change out and get his yes first.",
		Parameters: `{"type": "object", "additionalProperties": false,
            "properties": {"person": {"type": "string", "description": "name or id of the person"},
                           "name": {"type": "string"},
                           "phones": {"type": "array", "items": {"type": "string"}},
                           "telegram": {"type": "integer", "description": "numeric Telegram user id, 0 = unknown"},
                           "email": {"type": "string", "description": "'' = none"},
                           "blocked": {"type": "array", "items": {"type": "string"}, "description": "tool names this person may not use, e.g. phone_call; [] = none"},
                           "child": {"type": "boolean", "description": "a child: The assistant talks child-friendly"},
                           "notify": {"type": "string", "enum": ["auto", "telegram", "whatsapp"], "description": "channel for notices the assistant sends on its own (e.g. about new e-mails); auto = Telegram if connected, else WhatsApp"}},
            "required": ["person", "name", "phones", "telegram", "email", "blocked", "child", "notify"]}`,
		Private: true,
		Run:     func(args map[string]any, _ tools.Context) string { return r.updatePerson(args) },
	})
	reg.Add(tools.Tool{
		Name: "set_humor",
		Description: "Sets how cheeky and sarcastic the assistant is with a person: 0 polite, 1 dry wit, 2 cheeky and sarcastic, 3 " +
			"biting; -1 back to the default. For 'sei frecher/netter zu mir', 'sei bei Anna höflicher'. Everyone for " +
			"themselves, the admin for anyone; children are always talked to child-friendly.",
		Parameters: `{"type": "object", "additionalProperties": false,
            "properties": {"person": {"type": "string", "description": "name of the person, '' = the one you are talking with"},
                           "level": {"type": "integer", "minimum": -1, "maximum": 3}},
            "required": ["person", "level"]}`,
		Run: func(args map[string]any, ctx tools.Context) string {
			level, ok := tools.Int(args, "level")
			if !ok {
				level = -1
			}
			return r.setHumor(tools.String(args, "person"), level, ctx)
		},
	})
	reg.Add(tools.Tool{
		Name:        "remove_person",
		Description: "Removes a person from the register (their memory profile stays). Only the admin may; confirm first.",
		Parameters: `{"type": "object", "additionalProperties": false,
            "properties": {"person": {"type": "string", "description": "name or id of the person"}},
            "required": ["person"]}`,
		Private: true,
		Run:     func(args map[string]any, _ tools.Context) string { return r.removePerson(tools.String(args, "person")) },
	})
}

// backupSubject: the mail with the register in the mailbox, named after the assistant.
func backupSubject() string { return prompts.Name() + "-Personen" }
