#include "assistant/shared.h"

#include <ArduinoJson.h>
#include <LittleFS.h>
#include <esp_random.h>

#include "assistant/memory.h"
#include "assistant/tasks.h"
#include "config.h"
#include "services/psram_json.h"
#include "system/file_util.h"
#include "system/peer.h"
#include "system/time_util.h"
#include "voice/live_session.h"

namespace shared {
namespace {

// As core/cmd/assistant/shared.go: what changes the leader's state, and what only reads it.
const char* const WRITE_TOOLS[] = {"remember", "forget", "save_rule", "delete_rule", "save_skill", "delete_skill",
                                   "update_profile", "dream", "create_task", "delete_task", "pause_task", "run_task",
                                   "add_person", "update_person", "remove_person", "send_welcome", "set_humor",
                                   "speaker_access", "mail_rule", "update_affair", "pause_hints", "affair_trust",
                                   "answer_suggestion", "confirm_rule"};
const char* const READ_TOOLS[] = {"recall", "list_activities", "open_conversation", "open_topic", "use_skill",
                                  "list_tasks", "list_people", "list_affairs"};
// Task tools with an id: on one of the emergency tasks here they act here (assistant/tasks).
const char* const LOCAL_TASK_TOOLS[] = {"delete_task", "pause_task", "run_task"};

constexpr const char* JOURNAL_FILE = "/shared_journal.json";
constexpr uint32_t TOOL_TIMEOUT_MS = 30000;      // dream or a welcome mail on the leader take a while
constexpr uint32_t CONTEXT_TIMEOUT_MS = 3000;
constexpr uint32_t REFRESH_EVERY_MS = 5 * 60000;  // the leader's context, between conversations
constexpr uint32_t REFRESH_AFTER_HANDOVER_MS = 20000;  // the leader consolidated the conversation
constexpr uint32_t REPLAY_RETRY_MS = 10000;
constexpr size_t JOURNAL_MAX = 50;
constexpr size_t PENDING_TEXT_MAX = 2000;

struct Cache {
    String notes, notesWithIds, rules, rulesWithIds, planned;
    bool valid = false;
    uint32_t at = 0;  // millis()
};
Cache cache;
SemaphoreHandle_t lock = nullptr;  // cache and journal: the worker writes, the main loop reads
volatile uint32_t refreshAt = 0;   // 0 = by REFRESH_EVERY_MS
volatile bool replaySoon = true;
uint32_t lastReplay = 0;
bool replaying = false;  // the worker replays: tools run here, nothing forwarded or journaled
// Entries in the journal file; kept here, since every LittleFS.exists() on a missing file logs
// an error line.
volatile int journalSize = 0;

struct Guard {
    Guard() { xSemaphoreTake(lock, portMAX_DELAY); }
    ~Guard() { xSemaphoreGive(lock); }
};

bool listed(const char* name, const char* const* list, size_t count)
{
    for (size_t i = 0; i < count; ++i)
        if (!strcmp(list[i], name)) return true;
    return false;
}
template <size_t N>
bool listed(const char* name, const char* const (&list)[N])
{
    return listed(name, list, N);
}

String newId()
{
    char id[17];
    snprintf(id, sizeof(id), "%08lx%08lx", (unsigned long)esp_random(), (unsigned long)esp_random());
    return id;
}

// Go's time.Time in JSON: RFC 3339, here in UTC.
String rfc3339(time_t t)
{
    tm utc;
    gmtime_r(&t, &utc);
    char text[24];
    strftime(text, sizeof(text), "%Y-%m-%dT%H:%M:%SZ", &utc);
    return text;
}

// --- The journal -----------------------------------------------------------------------------

void loadJournal(JsonDocument& doc)
{
    File file = journalSize ? LittleFS.open(JOURNAL_FILE, "r") : File();
    if (!file || deserializeJson(doc, file) || !doc["entries"].is<JsonArray>()) {
        doc.clear();
        doc["entries"].to<JsonArray>();
    }
}

// Appends what the leader must still get: `path` with `body` (the id is added), a line for the
// instructions, and the emergency task it made here (0 = none).
void journal(const char* path, JsonDocument& body, const String& summary, int localTask = 0)
{
    Guard guard;
    JsonDocument doc(PsramJson::instance());
    loadJournal(doc);
    JsonArray entries = doc["entries"];
    while (entries.size() >= JOURNAL_MAX) entries.remove(0);  // the oldest: the flash is small
    JsonObject entry = entries.add<JsonObject>();
    entry["id"] = newId();
    entry["path"] = path;
    entry["at"] = (long long)time(nullptr);
    entry["summary"] = summary;
    if (localTask) entry["local_task"] = localTask;
    entry["body"] = body;
    if (!fileutil::saveJson(JOURNAL_FILE, doc)) Serial.println("[Shared] journal not written");
    journalSize = entries.size();
    replaySoon = true;
}

// What the leader has not got yet, for the instructions ("" when nothing).
String pendingText()
{
    if (!journalSize) return "";
    Guard guard;
    JsonDocument doc(PsramJson::instance());
    loadJournal(doc);
    String text;
    for (JsonObjectConst entry : doc["entries"].as<JsonArrayConst>()) {
        if (text.length() > PENDING_TEXT_MAX) break;
        text += "- " + timeutil::format(entry["at"] | 0LL, "%d.%m. %H:%M") + " " + (entry["summary"] | "") + "\n";
    }
    if (text.isEmpty()) return "";
    return "\nNot yet passed on to the main device, which keeps memory, tasks and people (it was not "
           "reachable; it gets this as soon as it is back):\n" + text;
}

// Sends the journal in order while the leader answers. Worker, between conversations.
void replay()
{
    if (!replaySoon && millis() - lastReplay < REPLAY_RETRY_MS) return;
    if (!journalSize || !peer::leaderPresent() || live::open()) return;
    replaySoon = false;
    lastReplay = millis();
    for (int round = 0; round < 10; ++round) {
        JsonDocument doc(PsramJson::instance());
        String path, id, body, summary;
        int localTask = 0;
        {
            Guard guard;
            loadJournal(doc);
            JsonArray entries = doc["entries"];
            if (entries.size() == 0) {
                LittleFS.remove(JOURNAL_FILE);
                journalSize = 0;
                return;
            }
            JsonObject entry = entries[0];
            path = entry["path"] | "";
            id = entry["id"] | "";
            summary = entry["summary"] | "";
            localTask = entry["local_task"] | 0;
            entry["body"]["id"] = id;  // the leader runs it once, also after a retry
            serializeJson(entry["body"], body);
        }
        // A reminder made here that already rang (and is gone) is done: not again on the leader.
        const bool done = localTask && !tasks::exists(localTask);
        if (!done) {
            String answer;
            if (!peer::callLeader(path.c_str(), body, answer, TOOL_TIMEOUT_MS)) {
                Serial.printf("[Shared] replay of %s: %s (later)\n", summary.c_str(), answer.c_str());
                return;
            }
            Serial.printf("[Shared] passed on to the leader: %s\n", summary.c_str());
            if (localTask) {  // the leader has it now: not twice
                replaying = true;
                tools::run("delete_task", ("{\"id\": " + String(localTask) + "}").c_str(), tools::Console);
                replaying = false;
            }
            if (path == "/conversation") refreshAt = millis() + REFRESH_AFTER_HANDOVER_MS;
        } else {
            Serial.printf("[Shared] %s already ran here, not passed on\n", summary.c_str());
        }
        Guard guard;
        loadJournal(doc);
        JsonArray entries = doc["entries"];
        if (entries.size() && id == (entries[0]["id"] | "")) entries.remove(0);
        if (entries.size()) fileutil::saveJson(JOURNAL_FILE, doc);
        else LittleFS.remove(JOURNAL_FILE);
        journalSize = entries.size();
        if (!journalSize) return;
    }
    replaySoon = true;  // more left: on in the next round
}

// --- The leader's context ----------------------------------------------------------------------

bool ask(JsonDocument& query, String& text)
{
    String body, answer;
    serializeJson(query, body);
    if (!peer::callLeader("/context", body, answer, CONTEXT_TIMEOUT_MS)) {
        Serial.printf("[Shared] context %s: %s\n", (const char*)(query["fn"] | ""), answer.c_str());
        return false;
    }
    JsonDocument doc(PsramJson::instance());
    if (deserializeJson(doc, answer)) return false;
    text = doc["text"] | "";
    return true;
}

bool askText(const char* fn, bool withIds, String& text)
{
    JsonDocument query;
    query["fn"] = fn;
    query["person"] = "";  // at the speaker: nobody named, all profiles
    query["with_ids"] = withIds;
    return ask(query, text);
}

void refresh()
{
    const bool due = refreshAt ? int32_t(millis() - refreshAt) >= 0
                               : !cache.valid || millis() - cache.at >= REFRESH_EVERY_MS;
    if (!due || live::open() || !peer::leaderPresent()) return;
    refreshAt = 0;
    Cache fresh;
    JsonDocument planned;
    planned["fn"] = "planned";
    planned["person"] = "";
    planned["voice"] = true;
    fresh.valid = askText("notes", false, fresh.notes) && askText("notes", true, fresh.notesWithIds) &&
                  askText("rules", false, fresh.rules) && askText("rules", true, fresh.rulesWithIds) &&
                  ask(planned, fresh.planned);
    if (!fresh.valid) {
        refreshAt = millis() + REPLAY_RETRY_MS;
        return;
    }
    fresh.at = millis();
    Guard guard;
    const bool first = !cache.valid;
    cache = fresh;
    if (first)
        Serial.printf("[Shared] context from the leader: notes %u, rules %u, planned %u characters\n",
                      cache.notes.length(), cache.rules.length(), cache.planned.length());
}

void background()
{
    replay();
    refresh();
}

}  // namespace

void begin()
{
    if (!MEMBER) return;
    lock = xSemaphoreCreateMutex();
    if (LittleFS.exists(JOURNAL_FILE)) {  // left from before a restart
        journalSize = 1;
        JsonDocument doc(PsramJson::instance());
        loadJournal(doc);
        journalSize = doc["entries"].size();
        Serial.printf("[Shared] %d entries for the leader in the journal\n", journalSize);
    }
    tools::addBackgroundWork(background);
    Serial.println("[Shared] memory, tasks and people are the leader's");
}

bool remote(const char* name) { return MEMBER && (listed(name, WRITE_TOOLS) || listed(name, READ_TOOLS)); }

void addTools()
{
    if (!MEMBER) return;
    using tools::Tool;
    // The schemas as the leader's (core/internal/affairs/tools.go): the arguments go there
    // unchanged. Run here only when the leader is away (list_affairs; update_affair is journaled).
    tools::add(Tool{
        "list_affairs",
        "The person's affairs: the open matters the assistant keeps (appointments, deadlines, to-dos, promises, things "
        "someone owes them, deliveries, trips) with id; the agenda shows the most relevant ones. For 'was ist noch "
        "offen?' and to find an id for update_affair.",
        R"json({"type": "object", "additionalProperties": false,
            "properties": {"state": {"type": "string", "enum": ["open", "all", "waiting", "snoozed", "done", "obsolete"]},
                           "kind": {"type": "string", "enum": ["", "appointment", "deadline", "todo", "promise", "waiting", "delivery", "trip", "other"]},
                           "days": {"type": "integer", "minimum": 0, "maximum": 365, "description": "due within that many days; 0 = any"}},
            "required": ["state", "kind", "days"]})json",
        [](JsonObjectConst, const tools::Context&) {
            return String("Error: the open matters are on the main device, which is not reachable now.");
        },
        tools::Slow, tools::Everywhere});
    tools::add(Tool{
        "update_affair",
        "Changes one of the person's affairs (id from the agenda or list_affairs): done (settled), obsolete (will "
        "not happen or no longer matters: it never comes up again), snoozed (again from remind_at), open; with the "
        "reason in the user's words; or a new due date, title, or parent (the affair it belongs to). Only on what "
        "the user says. Does not touch calendar events or to-do lists.",
        R"json({"type": "object", "additionalProperties": false,
            "properties": {"id": {"type": "string"},
                           "state": {"type": "string", "enum": ["", "open", "waiting", "done", "obsolete", "snoozed"]},
                           "reason": {"type": "string"},
                           "due": {"type": "string", "description": "YYYY-MM-DD or YYYY-MM-DD HH:MM; 'none' clears; '' = unchanged"},
                           "remind_at": {"type": "string", "description": "snoozed: YYYY-MM-DD"},
                           "title": {"type": "string"},
                           "parent": {"type": "string"}},
            "required": ["id", "state", "reason", "due", "remind_at", "title", "parent"]})json",
        [](JsonObjectConst, const tools::Context&) {
            return String("Error: the open matters are on the main device, which is not reachable now.");
        },
        tools::Slow, tools::Everywhere});
    tools::add(Tool{
        "pause_hints",
        "The user wants no unasked hints from the agenda for now ('Heute nicht, " ASSISTANT_NAME "', 'bis Montag nichts davon'): "
        "until the end of today or of a day, or 'off' to have them again. Lasting wishes about hints are behaviour "
        "rules (save_rule), not this.",
        R"json({"type": "object", "additionalProperties": false,
            "properties": {"until": {"type": "string", "description": "YYYY-MM-DD; '' = today; 'off' = hints again"}},
            "required": ["until"]})json",
        [](JsonObjectConst, const tools::Context&) {
            return String("Error: the agenda is on the main device, which is not reachable now.");
        },
        tools::Slow, tools::Everywhere});
    // The initiative's suggestions and the learned rules (#82) are the leader's: as its
    // internal/initiative/deliver.go and internal/memory/tools.go.
    tools::add(Tool{
        "answer_suggestion",
        "Records the user's answer to one of your own suggestions (the ids in 'Your own suggestions' of your "
        "instructions): accepted when they want it or liked it, declined when they do not. Call it right after they "
        "answer, also for 'ja, mach das' before you do it. A lasting wish about such suggestions ('sowas musst du mir "
        "nicht sagen', 'erinnere mich da immer dran') is a rule too: save_rule.",
        R"json({"type": "object", "additionalProperties": false,
            "properties": {"id": {"type": "string", "description": "e.g. s12"},
                           "answer": {"type": "string", "enum": ["accepted", "declined"]},
                           "reason": {"type": "string", "description": "the user's words in short, '' if none"}},
            "required": ["id", "answer", "reason"]})json",
        [](JsonObjectConst, const tools::Context&) {
            return String("Error: the suggestions are on the main device, which is not reachable now.");
        },
        tools::Slow | tools::Private, tools::Everywhere});
    tools::add(Tool{
        "confirm_rule",
        "Makes one of the rules you learned yourself (the 'not confirmed yet' list in your instructions) binding, "
        "when the user says yes to it ('ja, so machen', 'passt'). When they do not want it: delete_rule; when they "
        "want it different: save_rule with replaces.",
        R"json({"type": "object", "additionalProperties": false,
            "properties": {"id": {"type": "integer"}}, "required": ["id"]})json",
        [](JsonObjectConst, const tools::Context&) {
            return String("Error: the rules are on the main device, which is not reachable now.");
        },
        tools::Slow, tools::Everywhere});
}

bool forward(const char* name, const char* argumentsJson, const tools::Context& context, String& output)
{
    if (!remote(name) || replaying) return false;
    JsonDocument body(PsramJson::instance());
    body["name"] = name;
    body["arguments"] = argumentsJson;
    body["channel"] = uint8_t(context.channel);
    body["person"] = context.person;
    body["origin"] = context.origin;
    String text;
    serializeJson(body, text);
    String answer;
    if (peer::callLeader("/tool", text, answer, TOOL_TIMEOUT_MS)) {
        JsonDocument doc(PsramJson::instance());
        if (!deserializeJson(doc, answer)) {
            output = doc["text"] | "";
            Serial.printf("[Shared] %s ran on the leader\n", name);
            return true;
        }
        answer = "answer not readable";
    }
    Serial.printf("[Shared] %s on the leader: %s\n", name, answer.c_str());
    if (listed(name, READ_TOOLS)) return false;  // the local copy answers
    // On one of the emergency tasks made here: here, and the leader never hears of it.
    if (listed(name, LOCAL_TASK_TOOLS)) {
        JsonDocument arguments;
        deserializeJson(arguments, argumentsJson);
        if (tasks::exists(arguments["id"] | 0)) return false;
    }
    String summary = String(name) + " " + argumentsJson;
    if (summary.length() > 300) summary = summary.substring(0, 300) + "...";
    if (!strcmp(name, "create_task")) {
        // A reminder must ring even if the leader stays away: made here as well (a file of its own).
        replaying = true;
        const String local = tools::run(name, argumentsJson, context.channel, context.person, context.origin);
        replaying = false;
        const int at = local.indexOf('#');
        const int id = local.startsWith("Created") && at >= 0 ? local.substring(at + 1).toInt() : 0;
        if (!id) {
            output = local;  // refused here too (a bad schedule): the model hears why
            return true;
        }
        journal("/tool", body, summary, id);
        output = local + " (The main device is not reachable: the task is kept on this speaker and moves to "
                         "the main device when it is back.)";
        return true;
    }
    journal("/tool", body, summary);
    output = "Noted on this speaker: the main device, which keeps memory, tasks and people, is not reachable (" +
             answer + "). It gets this as soon as it is back. Tell the user briefly.";
    return true;
}

String notes(bool withIds, const String& person)
{
    if (MEMBER && lock) {
        Guard guard;
        if (cache.valid) return withIds ? cache.notesWithIds : cache.notes;
    }
    return memory::context(withIds, person);
}

String rules(const String& person, bool withIds)
{
    if (MEMBER && lock) {
        Guard guard;
        if (cache.valid) return withIds ? cache.rulesWithIds : cache.rules;
    }
    return memory::rules(person, withIds);
}

String planned(const String& local)
{
    if (!MEMBER || !lock) return local;
    String leader;
    {
        Guard guard;
        if (!cache.valid) return local;
        leader = cache.planned;
    }
    return leader + tasks::context() + pendingText();
}

bool conversationOver(const String& text, time_t started, const String& person, const char* channel,
                      const String& transcript)
{
    if (!MEMBER || !lock) return false;
    if (transcript.isEmpty()) return true;
    JsonDocument body(PsramJson::instance());
    body["text"] = text;
    body["started"] = rfc3339(started);
    body["person"] = person;
    body["channel"] = channel;
    body["conv"] = "m5-" + String((long long)started);
    body["transcript"] = transcript;
    journal("/conversation", body, "conversation at the speaker (" + timeutil::format(started, "%H:%M") + ")");
    return true;
}

void print()
{
    if (!MEMBER) {
        Serial.println("\n[Shared] standalone: memory, tasks and people are this device's");
        return;
    }
    {
        Guard guard;
        Serial.printf("\n[Shared] leader %s; context %s (notes %u, rules %u, planned %u characters, %lu s old)\n",
                      peer::leaderName().length() ? peer::leaderName().c_str() : "none",
                      cache.valid ? "from the leader" : "not fetched yet", cache.notes.length(), cache.rules.length(),
                      cache.planned.length(), cache.valid ? (unsigned long)((millis() - cache.at) / 1000) : 0UL);
    }
    const String pending = pendingText();
    Serial.println(pending.length() ? pending : "[Shared] journal empty");
}

}  // namespace shared
