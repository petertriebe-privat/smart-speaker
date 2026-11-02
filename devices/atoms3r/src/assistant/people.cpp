#include "assistant/people.h"

#include <ArduinoJson.h>
#include <LittleFS.h>

#include "assistant/tools.h"
#include "config.h"
#include "persona.h"
#include "secrets.h"
#include "assistant/backup.h"
#include "services/mail.h"
#include "services/psram_json.h"
#include "services/telegram.h"
#include "services/whatsapp.h"
#include "system/file_util.h"
#include "system/phone_number.h"
#include "system/scoped_lock.h"
#include "system/text_util.h"
#include "system/time_util.h"
#include "system/tls_gate.h"

// Optional in secrets.h (older copies do not have them).
#ifndef PHONE_OWNER
#define PHONE_OWNER ""
#endif
#ifndef TELEGRAM_OWNER_ID
#define TELEGRAM_OWNER_ID 0
#endif

namespace people {
namespace {

constexpr const char* FILE_PATH = "/people.json";
constexpr const char* BACKUP_FILE = "people.json";       // in Drive
constexpr const char* BACKUP_SUBJECT = ASSISTANT_NAME "-Personen";  // in the mailbox (MEMORY_FOLDER_CORE)
constexpr size_t MAX_PEOPLE = 10;

// {"people": [{"id", "name", "role", "phones": [], "telegram", "email", "blocked": [], "humor", "child"}]}
std::vector<Person> list;  // guarded by lock
SemaphoreHandle_t lock = xSemaphoreCreateMutex();  // main loop, tool worker, agent runs
volatile bool idle = false, backupPending = false, restorePending = false;
uint32_t retryAt = 0;

struct Locked : ScopedLock {
    Locked() : ScopedLock(lock) {}
};

// "Anna-Lena" -> "anna-lena": a person's id.
String slug(const String& name) { return textutil::slug(name, 24); }

String lower(String text)
{
    text.trim();
    text.toLowerCase();
    return text;
}

String joined(const std::vector<String>& items)
{
    String text;
    for (const String& item : items) text += (text.length() ? ", " : "") + item;
    return text;
}

String accountText(int64_t account)
{
    char text[24];
    snprintf(text, sizeof(text), "%lld", static_cast<long long>(account));
    return text;
}

String describe(const Person& p)
{
    String text = p.name + " (id " + p.id + (p.admin() ? ", admin" : "") + "): phone " +
                  (p.phones.empty() ? String("-") : joined(p.phones)) + ", Telegram " +
                  (p.telegram ? accountText(p.telegram) : String("- (unknown)")) + ", mail " +
                  (p.email.length() ? p.email : String("-"));
    if (!p.blocked.empty()) text += ", blocked: " + joined(p.blocked);
    if (p.child) text += ", child (child-friendly)";
    else text += ", humour " + String(humor(p)) + (p.humor < 0 ? " (default)" : "");
    text += ", notices via " + (p.notify.length() ? p.notify : String("auto"));
    return text;
}

// --- Storage (caller holds the lock) ---------------------------------------------------

Person* findLocked(const String& id)
{
    for (Person& p : list)
        if (p.id == id) return &p;
    return nullptr;
}

Person* findByNameLocked(const String& name)
{
    const String wanted = lower(name), wantedId = slug(name);
    if (wanted.isEmpty()) return nullptr;
    for (Person& p : list)
        if (lower(p.name) == wanted || p.id == wanted || p.id == wantedId) return &p;
    return nullptr;
}

void toJson(JsonDocument& doc)
{
    JsonArray array = doc["people"].to<JsonArray>();
    for (const Person& p : list) {
        JsonObject entry = array.add<JsonObject>();
        entry["id"] = p.id;
        entry["name"] = p.name;
        entry["role"] = p.role;
        JsonArray phones = entry["phones"].to<JsonArray>();
        for (const String& phone : p.phones) phones.add(phone);
        entry["telegram"] = p.telegram;
        entry["email"] = p.email;
        JsonArray blocked = entry["blocked"].to<JsonArray>();
        for (const String& tool : p.blocked) blocked.add(tool);
        if (p.humor >= 0) entry["humor"] = p.humor;
        if (p.child) entry["child"] = true;
        if (p.invite.length()) entry["invite"] = p.invite;
        if (p.notify.length()) entry["notify"] = p.notify;
    }
}

std::vector<Person> fromJson(const JsonDocument& doc)
{
    std::vector<Person> people;
    for (JsonObjectConst entry : doc["people"].as<JsonArrayConst>()) {
        Person p;
        p.id = entry["id"] | "";
        p.name = entry["name"] | "";
        p.role = entry["role"] | "member";
        for (JsonVariantConst phone : entry["phones"].as<JsonArrayConst>()) p.phones.push_back(phone | "");
        p.telegram = entry["telegram"] | int64_t(0);
        p.email = entry["email"] | "";
        for (JsonVariantConst tool : entry["blocked"].as<JsonArrayConst>()) p.blocked.push_back(tool | "");
        p.humor = entry["humor"] | -1;
        p.child = entry["child"] | false;
        p.invite = entry["invite"] | "";
        p.notify = entry["notify"] | "";
        if (p.id.length() && p.name.length()) people.push_back(p);
    }
    return people;
}

// The admin from the firmware: nobody else is admin, his accounts are the firmware's.
void ensureAdmin()
{
    const String id = slug(OWNER_NAME);
    Person* admin = findLocked(id);
    if (!admin) {
        Person owner;
        owner.id = id;
        owner.name = OWNER_NAME;
        owner.role = "admin";
        list.insert(list.begin(), owner);
        admin = &list.front();
    }
    for (Person& p : list)
        if (&p != admin) p.role = "member";
    admin->role = "admin";
    const String phone = phonenumber::normalized(PHONE_OWNER);
    if (phone.length() && std::find(admin->phones.begin(), admin->phones.end(), phone) == admin->phones.end())
        admin->phones.insert(admin->phones.begin(), phone);
    if (TELEGRAM_OWNER_ID) admin->telegram = TELEGRAM_OWNER_ID;
    admin->email = OWNER_EMAIL;
    admin->blocked.clear();
    admin->child = false;
}

void save()
{
    JsonDocument doc(PsramJson::instance());
    toJson(doc);
    if (!fileutil::saveJson(FILE_PATH, doc)) Serial.println("[People] saving failed");
    backupPending = ::backup::available() && !restorePending;  // an incomplete list must not replace the backup
}

// --- Backup in the mailbox (tool worker, between conversations) ---------------------------

void backup()
{
    String text;
    JsonDocument doc(PsramJson::instance());
    {
        Locked guard;
        text = "Personenregister von " ASSISTANT_NAME ", Stand " + timeutil::format(time(nullptr), "%d.%m.%Y %H:%M") +
               ". Stellt es nach einem Gerätewechsel wieder her; bitte nicht bearbeiten.\n\n";
        for (const Person& p : list) text += "- " + describe(p) + "\n";
        toJson(doc);
        backupPending = false;
    }
    const String result = ::backup::store(BACKUP_FILE, BACKUP_SUBJECT, text, doc);
    Serial.printf("[People] backup: %s\n", result.c_str());
    if (result != "stored") {
        backupPending = true;
        retryAt = millis() + MEMORY_OUTBOX_RETRY_MS;
    }
}

void restore()
{
    String error;
    JsonDocument doc(PsramJson::instance());
    if (!::backup::read(BACKUP_FILE, BACKUP_SUBJECT, doc, error)) {
        Serial.printf("[People] restore: %s\n", error.c_str());
        retryAt = millis() + MEMORY_OUTBOX_RETRY_MS;
        return;
    }
    restorePending = false;
    Locked guard;
    if (doc.isNull()) {
        save();  // no backup yet: the current list becomes it
        return;
    }
    if (list.size() > 1) {  // added meanwhile: those count, the list becomes the backup
        save();
        return;
    }
    list = fromJson(doc);
    ensureAdmin();
    save();
    Serial.printf("[People] restored from the mailbox: %u\n", unsigned(list.size()));
}

void background()
{
    if (!idle || !(backupPending || restorePending) || int32_t(millis() - retryAt) < 0) return;
    tls_gate::TryHold gate;  // one background TLS job at a time; else again in a second
    if (!gate) return;
    if (restorePending) restore();
    else backup();
}

// --- Tools ---------------------------------------------------------------------------------

// Checks and normalizes the numbers; "" or the error.
String checkPhones(JsonArrayConst given, std::vector<String>& phones, const String& except)
{
    phones.clear();
    for (JsonVariantConst value : given) {
        const String number = phonenumber::normalized(value | "");
        if (number.isEmpty()) continue;
        if (!number.startsWith("0") || number.length() < 6)
            return "Error: " + String(value | "") + " is not a complete phone number (with area code).";
        for (const Person& p : list)
            if (p.id != except && std::find(p.phones.begin(), p.phones.end(), number) != p.phones.end())
                return "Error: the number " + number + " already belongs to " + p.name + ".";
        if (std::find(phones.begin(), phones.end(), number) == phones.end()) phones.push_back(number);
    }
    return "";
}

String checkTelegram(int64_t account, const String& except)
{
    if (!account) return "";
    for (const Person& p : list)
        if (p.id != except && p.telegram == account) return "Error: this Telegram account already belongs to " + p.name + ".";
    return "";
}

String checkEmail(String& email, const String& except)
{
    email = lower(email);
    if (email.isEmpty()) return "";
    if (email.indexOf('@') < 1 || email.indexOf(' ') >= 0) return "Error: " + email + " is not an e-mail address.";
    for (const Person& p : list)
        if (p.id != except && lower(p.email) == email) return "Error: the e-mail address already belongs to " + p.name + ".";
    return "";
}

String listPeople()
{
    Locked guard;
    String text;
    for (const Person& p : list) text += "- " + describe(p) + "\n";
    return text;
}

String html(String text)
{
    text.replace("&", "&amp;");
    text.replace("<", "&lt;");
    text.replace(">", "&gt;");
    return text;
}

// A fresh code for the Telegram link of a welcome mail (only while the account is unknown).
String newInvite() {
    char code[16];
    snprintf(code, sizeof(code), "%08lx%04x", (unsigned long)esp_random(), unsigned(esp_random() & 0xffff));
    return code;
}

// The welcome mail to a person: how to reach the assistant (German: the person reads it). Returns
// "sent", "" without mailbox or address, or an error. Blocks for seconds.
String welcome(const Person& p)
{
    if (!mail::configured() || p.email.isEmpty()) return "";
    const String link = p.telegram ? String() : telegram::inviteLink(p.invite);
    String ways;
    if (link.length())
        ways += "<li><b>Telegram:</b> <a href=\"" + link + "\">diesen Link öffnen</a> und auf „Starten“ tippen. "
                "Danach erkennt " ASSISTANT_NAME " Sie dort; Sie können ihm schreiben und Sprachnachrichten, Fotos oder Dateien "
                "schicken.</li>";
    if (*ASSISTANT_PHONE_NUMBER && p.phones.size())
        ways += "<li><b>Telefon:</b> " + html(ASSISTANT_PHONE_NUMBER) + " anrufen. Von Ihrer Nummer " + html(p.phones[0]) +
                " aus erkennt er Sie.</li>";
    if (mail::takesCommands())
        ways += "<li><b>E-Mail:</b> an " + html(mail::address()) + " schreiben, was er erledigen soll; er antwortet "
                "per Mail. Das gilt nur für Mails von " + html(p.email) + ".</li>";
    ways += "<li><b>Zu Hause:</b> „Hey Jarvis“ sagen.</li>";
    const String body = "<p>Hallo " + html(p.name) + ",</p><p>" + html(OWNER_NAME) +
                        " hat Sie bei " ASSISTANT_NAME " freigeschaltet, dem Sprachassistenten Ihres Haushalts. So erreichen Sie "
                        "ihn:</p><ul>" + ways +
                        "</ul><p>" ASSISTANT_NAME " beantwortet Fragen, erinnert an Termine, schickt Nachrichten, steuert das Smart "
                        "Home und merkt sich, was Sie ihm erzählen. Auf die persönlichen Konten von " +
                        html(OWNER_NAME) + " (Kalender, Kontakte, Postfach) hat er in Gesprächen mit Ihnen keinen "
                        "Zugriff.</p><p>Viele Grüße<br>" ASSISTANT_NAME "</p>";
    return mail::send(p.email, "Willkommen bei " ASSISTANT_NAME, body);
}

String welcomeResult(const Person& p)
{
    const String sent = welcome(p);
    if (sent.isEmpty()) return p.email.isEmpty() ? " No welcome mail (no e-mail address)." : " No welcome mail (no mailbox).";
    return sent == "sent" ? " Welcome mail sent to " + p.email + (p.telegram ? "." : " (with the Telegram link).")
                          : " Welcome mail failed: " + sent;
}

String addPerson(JsonObjectConst arguments)
{
    String name = arguments["name"] | "";
    name.trim();
    const String id = slug(name);
    if (id.isEmpty()) return "Error: name missing";
    Person p;
    {
        Locked guard;
        if (list.size() >= MAX_PEOPLE) return "Error: already " + String(MAX_PEOPLE) + " people registered";
        if (findLocked(id) || findByNameLocked(name)) return "Error: " + name + " is already registered.";
        p.id = id;
        p.name = name;
        p.role = "member";
        String email = arguments["email"] | "";
        String error = checkPhones(arguments["phones"], p.phones, id);
        if (error.isEmpty()) error = checkTelegram(arguments["telegram"] | int64_t(0), id);
        if (error.isEmpty()) error = checkEmail(email, id);
        if (error.length()) return error;
        p.telegram = arguments["telegram"] | int64_t(0);
        p.email = email;
        if (!p.telegram && telegram::configured()) p.invite = newInvite();
        list.push_back(p);
        save();
    }
    Serial.printf("[People] added: %s\n", describe(p).c_str());
    return "Registered: " + describe(p) + "." + welcomeResult(p);
}

String sendWelcome(const String& who)
{
    Person p;
    {
        Locked guard;
        Person* found = findByNameLocked(who);
        if (!found) return "Error: this person is not registered.";
        if (found->admin()) return "Error: the admin needs no welcome mail.";
        if (!found->telegram && telegram::configured()) {
            found->invite = newInvite();  // a new link; an old one stops working
            save();
        }
        p = *found;
    }
    const String result = welcomeResult(p);
    return result.indexOf("sent to") >= 0 ? result.substring(1) : "Error:" + result;
}

String updatePerson(JsonObjectConst arguments)
{
    Locked guard;
    Person* p = findByNameLocked(arguments["person"] | "");
    if (!p) return "Error: this person is not registered.";
    Person changed = *p;
    String name = arguments["name"] | "";
    name.trim();
    if (name.length() && name != p->name) {
        if (p->admin()) return "Error: the admin's name is set by the firmware.";
        Person* other = findByNameLocked(name);
        if (other && other != p) return "Error: " + name + " is already registered.";
        changed.name = name;  // the id stays: the memory's profile hangs on it
    }
    String email = arguments["email"] | "";
    String error = checkPhones(arguments["phones"], changed.phones, p->id);
    if (error.isEmpty()) error = checkTelegram(arguments["telegram"] | int64_t(0), p->id);
    if (error.isEmpty()) error = checkEmail(email, p->id);
    if (error.length()) return error;
    changed.telegram = arguments["telegram"] | int64_t(0);
    changed.email = email;
    changed.child = arguments["child"] | false;
    const String notify = arguments["notify"] | "auto";
    changed.notify = notify == "telegram" || notify == "whatsapp" ? notify : String();
    changed.blocked.clear();
    for (JsonVariantConst tool : arguments["blocked"].as<JsonArrayConst>()) {
        const String name = lower(tool | "");
        if (name.length()) changed.blocked.push_back(name);
    }
    *p = changed;
    ensureAdmin();  // the admin's own accounts stay the firmware's
    save();
    Serial.printf("[People] changed: %s\n", describe(*p).c_str());
    return "Changed: " + describe(*p);
}

// "sei frecher zu mir", "sei bei Max netter": the admin for anyone, others only for themselves,
// children not at all.
String setHumor(const String& who, int level, const tools::Context& context)
{
    Locked guard;
    Person* target = who.length() ? findByNameLocked(who) : findLocked(context.person);
    if (!target) return who.length() ? "Error: " + who + " is not registered." : String("Error: for whom? Give person.");
    const Person* asking = findLocked(context.person);
    if (asking && !asking->admin() && (asking != target || asking->child))
        return "Error: only " + String(OWNER_NAME) + " may do that.";
    if (target->child) return "Error: the assistant talks child-friendly with " + target->name + " (a child in the register).";
    target->humor = level < 0 ? -1 : min(level, PERSONA_MAX_LEVEL);
    save();
    static const char* const names[] = {"polite", "dry", "cheeky", "biting"};
    const int effective = humor(*target);
    Serial.printf("[People] humour for %s: %d\n", target->id.c_str(), effective);
    return "Humour for " + target->name + ": level " + String(effective) + " (" + names[effective] + ")" +
           (target->humor < 0 ? ", the default" : "") + ". Applies from the next conversation on.";
}

String removePerson(const String& who)
{
    Locked guard;
    Person* p = findByNameLocked(who);
    if (!p) return "Error: this person is not registered.";
    if (p->admin()) return "Error: nobody can remove the admin.";
    const String name = p->name;
    list.erase(list.begin() + (p - list.data()));
    save();
    Serial.printf("[People] removed: %s\n", name.c_str());
    return name + " is removed (the memory profile stays).";
}

}  // namespace

void begin()
{
    Locked guard;
    File file = LittleFS.exists(FILE_PATH) ? LittleFS.open(FILE_PATH, "r") : File();
    JsonDocument doc(PsramJson::instance());
    const bool loaded = file && !deserializeJson(doc, file);
    file.close();
    list = loaded ? fromJson(doc) : std::vector<Person>();
    ensureAdmin();
    restorePending = !loaded && ::backup::available();
    if (!loaded) save();
    else if (::backup::due(BACKUP_FILE)) backupPending = true;  // the first backup in Drive
    Serial.printf("[People] %u registered%s\n", unsigned(list.size()),
                  restorePending ? ", looking for the backup" : "");
}

void setIdle(bool on) { idle = on; }

bool find(const String& id, Person& person)
{
    Locked guard;
    const Person* p = id.length() ? findLocked(id) : nullptr;
    if (p) person = *p;
    return p;
}

std::vector<Person> all()
{
    Locked guard;
    return list;
}

String admin() { return slug(OWNER_NAME); }

String name(const String& id)
{
    Locked guard;
    const Person* p = id.length() ? findLocked(id) : nullptr;
    return p ? p->name : String();
}

String byName(const String& name)
{
    Locked guard;
    const Person* p = findByNameLocked(name);
    return p ? p->id : String();
}

String byPhone(const String& number)
{
    const String wanted = phonenumber::normalized(number);
    if (wanted.isEmpty()) return "";
    Locked guard;
    for (const Person& p : list)
        if (std::find(p.phones.begin(), p.phones.end(), wanted) != p.phones.end()) return p.id;
    return "";
}

String byTelegram(int64_t account)
{
    if (!account) return "";
    Locked guard;
    for (const Person& p : list)
        if (p.telegram == account) return p.id;
    return "";
}

String claimInvite(const String& code, int64_t telegram)
{
    String key = code;
    key.trim();
    if (key.length() < 8 || !telegram) return "";
    Locked guard;
    for (const Person& p : list)
        if (p.telegram == telegram) return "";  // the account belongs to someone already
    for (Person& p : list)
        if (p.invite == key) {
            p.telegram = telegram;
            p.invite = "";
            save();
            Serial.printf("[People] %s linked Telegram through the welcome mail\n", p.name.c_str());
            return p.id;
        }
    return "";
}

String byEmail(const String& address)
{
    const String wanted = lower(address);
    if (wanted.isEmpty()) return "";
    Locked guard;
    for (const Person& p : list)
        if (p.email.length() && lower(p.email) == wanted) return p.id;
    return "";
}

String denied(const String& person, const char* tool, bool privateTool)
{
    if (person.isEmpty()) return "";  // at home, nobody named: the house is trusted
    Locked guard;
    const Person* p = findLocked(person);
    if (p && p->admin()) return "";
    if (p && std::find(p->blocked.begin(), p->blocked.end(), String(tool)) != p->blocked.end())
        return "Error: " + String(tool) + " is blocked for " + p->name + ".";
    if (!privateTool) return "";
    const Person* owner = findLocked(slug(OWNER_NAME));
    return "Error: only " + (owner ? owner->name : String("the admin")) +
           " may do that (their own accounts and the administration of the assistant).";
}

String message(const String& person, const String& text)
{
    Person p;
    if (!find(person.length() ? person : admin(), p)) return "Error: this person is not registered.";
    const bool viaTelegram = telegram::configured() && p.telegram;
    const bool viaWhatsapp = whatsapp::configured() && p.phones.size();
    std::vector<String> order;
    if (p.notify == "whatsapp" || (p.notify.isEmpty() && !viaTelegram)) order = {"whatsapp", "telegram"};
    else order = {"telegram", "whatsapp"};
    String errors;
    for (const String& channel : order) {
        if (channel == "telegram" ? !viaTelegram : !viaWhatsapp) continue;
        const String result = channel == "telegram" ? telegram::send(html(text), p.id) : whatsapp::send(text, p.id);
        if (result == "sent") return channel;
        errors += (errors.length() ? "; " : "") + channel + ": " + result;
    }
    return errors.length() ? "Error: " + errors : "Error: " + p.name + " has neither Telegram nor WhatsApp.";
}

int humor(const Person& person)
{
    return person.humor >= 0 ? min(person.humor, PERSONA_MAX_LEVEL) : min(max(PERSONA_LEVEL, 0), PERSONA_MAX_LEVEL);
}

String household()
{
    Locked guard;
    if (list.size() < 2) return "";
    String text;
    for (const Person& p : list) text += (text.length() ? ", " : "") + p.name + (p.admin() ? " (admin)" : "");
    return "People of the household who use " ASSISTANT_NAME ": " + text + ".\n";
}

void addTools()
{
    tools::addBackgroundWork(background);
    using tools::Tool;
    tools::add(Tool{
        "list_people",
        "Lists the people registered with the assistant (the household): name, id, phone numbers, Telegram account, "
        "e-mail, blocked tools.",
        R"json({"type": "object", "additionalProperties": false, "properties": {}, "required": []})json",
        [](JsonObjectConst, const tools::Context&) { return listPeople(); }, 0, tools::Everywhere});
    tools::add(Tool{
        "add_person",
        "Registers a new member of the household so the assistant recognizes them by phone number, Telegram account or "
        "e-mail, e.g. 'gib Kontakt X frei': take name, numbers and e-mail address from find_contact. Only the "
        "admin may. First read all details out and get his yes. The person then gets a welcome e-mail with the "
        "ways to reach the assistant and a Telegram link that connects their account (telegram 0 until then).",
        R"json({"type": "object", "additionalProperties": false,
            "properties": {"name": {"type": "string", "description": "first name as the household uses it"},
                           "phones": {"type": "array", "items": {"type": "string"}, "description": "mobile and other numbers with area code"},
                           "telegram": {"type": "integer", "description": "numeric Telegram user id, 0 = unknown"},
                           "email": {"type": "string", "description": "'' = none"}},
            "required": ["name", "phones", "telegram", "email"]})json",
        [](JsonObjectConst arguments, const tools::Context&) { return addPerson(arguments); },
        tools::Private | tools::Slow, tools::Everywhere});  // slow: the welcome mail
    tools::add(Tool{
        "send_welcome",
        "Sends a registered person the welcome e-mail again (ways to reach " ASSISTANT_NAME ", a new Telegram link while their "
        "account is not connected), e.g. for people registered before welcome mails existed. Only the admin may.",
        R"json({"type": "object", "additionalProperties": false,
            "properties": {"person": {"type": "string", "description": "name or id of the person"}},
            "required": ["person"]})json",
        [](JsonObjectConst arguments, const tools::Context&) { return sendWelcome(arguments["person"] | ""); },
        tools::Private | tools::Slow, tools::Everywhere});
    tools::add(Tool{
        "update_person",
        "Changes a registered person. Gives the complete new entry: take the current values from list_people "
        "for everything that stays. Only the admin may; read the change out and get his yes first.",
        R"json({"type": "object", "additionalProperties": false,
            "properties": {"person": {"type": "string", "description": "name or id of the person"},
                           "name": {"type": "string"},
                           "phones": {"type": "array", "items": {"type": "string"}},
                           "telegram": {"type": "integer", "description": "numeric Telegram user id, 0 = unknown"},
                           "email": {"type": "string", "description": "'' = none"},
                           "blocked": {"type": "array", "items": {"type": "string"}, "description": "tool names this person may not use, e.g. phone_call; [] = none"},
                           "child": {"type": "boolean", "description": "a child: the assistant talks child-friendly"},
                           "notify": {"type": "string", "enum": ["auto", "telegram", "whatsapp"], "description": "channel for notices the assistant sends on its own (e.g. about new e-mails); auto = Telegram if connected, else WhatsApp"}},
            "required": ["person", "name", "phones", "telegram", "email", "blocked", "child", "notify"]})json",
        [](JsonObjectConst arguments, const tools::Context&) { return updatePerson(arguments); }, tools::Private,
        tools::Everywhere});
    tools::add(Tool{
        "set_humor",
        "Sets how cheeky and sarcastic the assistant is with a person: 0 polite, 1 dry wit, 2 cheeky and sarcastic, 3 "
        "biting; -1 back to the default. For 'sei frecher/netter zu mir', 'sei bei Anna höflicher'. Everyone for "
        "themselves, the admin for anyone; children are always talked to child-friendly.",
        R"json({"type": "object", "additionalProperties": false,
            "properties": {"person": {"type": "string", "description": "name of the person, '' = the one you are talking with"},
                           "level": {"type": "integer", "minimum": -1, "maximum": 3}},
            "required": ["person", "level"]})json",
        [](JsonObjectConst arguments, const tools::Context& context) {
            return setHumor(arguments["person"] | "", arguments["level"] | -1, context);
        },
        0, tools::Everywhere});
    tools::add(Tool{
        "remove_person",
        "Removes a person from the register (their memory profile stays). Only the admin may; confirm first.",
        R"json({"type": "object", "additionalProperties": false,
            "properties": {"person": {"type": "string", "description": "name or id of the person"}},
            "required": ["person"]})json",
        [](JsonObjectConst arguments, const tools::Context&) { return removePerson(arguments["person"] | ""); },
        tools::Private, tools::Everywhere});
}

void print()
{
    Locked guard;
    for (const Person& p : list) Serial.printf("[People] %s\n", describe(p).c_str());
}

}  // namespace people
