// Dreaming: once a night (DREAM_HOUR) the memory task looks over the whole memory and the last
// DREAM_DAYS days of conversations at once, like OpenClaw's dreaming (issue #36). Each
// conversation is consolidated right after it ends (memory.cpp); this sweep is the view across
// them: it removes facts about events that are over, merges doubles, closes stale topics
// (light), condenses what fills the prompt (long topic statuses and profile fields, duplicate
// topics, overlapping behaviour rules; a rule goes only when a newer one replaced it), finds what
// recurs on several days (rem) and keeps only what has evidence (deep).
// Suggestions for the household go into the next day's prompts; a diary entry with what was
// changed goes to the archive. The core before the dream is kept in the flash
// (core-before-dream.json) and in Drive's revisions.

#include "assistant/memory.h"
#include "assistant/memory_internal.h"

#include <LittleFS.h>
#include <set>
#include <vector>

#include "assistant/people.h"
#include "assistant/prompt.h"
#include "config.h"
#include "prompts.h"
#include "services/openai.h"
#include "services/psram_json.h"
#include "system/file_util.h"
#include "system/time_util.h"
#include "system/utf8.h"

namespace memory {
namespace internal {
namespace {

constexpr const char* BEFORE_FILE = "/memory/core-before-dream.json";
constexpr uint32_t RETRY_MS = 15 * 60 * 1000;  // the API failed: again within the hour

// Output of the dream request (Structured Outputs).
constexpr const char* SCHEMA = R"({
  "type": "object", "additionalProperties": false,
  "required": ["remove", "merge", "rewrite", "topics", "topic_merge", "profiles", "rules", "rules_remove", "patterns",
               "add", "suggestions", "diary"],
  "properties": {
    "remove": {"type": "array", "items": {"type": "object", "additionalProperties": false,
      "required": ["id", "reason"], "properties": {"id": {"type": "integer"}, "reason": {"type": "string"}}}},
    "merge": {"type": "array", "items": {"type": "object", "additionalProperties": false,
      "required": ["ids", "category", "text"],
      "properties": {"ids": {"type": "array", "items": {"type": "integer"}}, "category": {"$ref": "#/$defs/category"},
                     "text": {"type": "string"}}}},
    "rewrite": {"type": "array", "items": {"type": "object", "additionalProperties": false,
      "required": ["id", "category", "text"],
      "properties": {"id": {"type": "integer"}, "category": {"$ref": "#/$defs/category"}, "text": {"type": "string"}}}},
    "topics": {"type": "array", "items": {"type": "object", "additionalProperties": false,
      "required": ["title", "status", "done", "reason"],
      "properties": {"title": {"type": "string"}, "status": {"type": "string", "description": "new status, '' = unchanged"},
                     "done": {"type": "boolean"}, "reason": {"type": "string"}}}},
    "topic_merge": {"type": "array", "items": {"type": "object", "additionalProperties": false,
      "required": ["titles", "status", "reason"],
      "properties": {"titles": {"type": "array", "items": {"type": "string"}}, "status": {"type": "string"},
                     "reason": {"type": "string"}}}},
    "profiles": {"type": "array", "items": {"type": "object", "additionalProperties": false,
      "required": ["person", "field", "value", "reason"],
      "properties": {"person": {"type": "string", "enum": [PEOPLE]},
                     "field": {"type": "string", "enum": ["name", "home", "birthday", "family", "pets", "work", "interests", "wishes"]},
                     "value": {"type": "string"}, "reason": {"type": "string"}}}},
    "rules": {"type": "array", "items": {"type": "object", "additionalProperties": false,
      "required": ["ids", "text", "reason"],
      "properties": {"ids": {"type": "array", "items": {"type": "integer"}}, "text": {"type": "string"},
                     "reason": {"type": "string"}}}},
    "rules_remove": {"type": "array", "items": {"type": "object", "additionalProperties": false,
      "required": ["id", "newer", "reason"],
      "properties": {"id": {"type": "integer"}, "newer": {"type": "integer"}, "reason": {"type": "string"}}}},
    "patterns": {"type": "array", "items": {"$ref": "#/$defs/evidenced"}},
    "add": {"type": "array", "items": {"type": "object", "additionalProperties": false,
      "required": ["category", "text", "evidence"],
      "properties": {"category": {"$ref": "#/$defs/category"}, "text": {"type": "string"},
                     "evidence": {"type": "array", "items": {"type": "string"}}}}},
    "suggestions": {"type": "array", "items": {"type": "object", "additionalProperties": false,
      "required": ["person", "text"],
      "properties": {"person": {"type": "string", "enum": [PEOPLE, ""]}, "text": {"type": "string"}}}},
    "diary": {"type": "string"}
  },
  "$defs": {"category": {"type": "string", "enum": ["Person", "Vorliebe", "Haushalt", "Termin", "Sonstiges"]},
    "evidenced": {"type": "object", "additionalProperties": false, "required": ["text", "evidence"],
      "properties": {"text": {"type": "string"}, "evidence": {"type": "array", "items": {"type": "string"}}}}}
})";

volatile bool requested = false;
volatile bool running = false;  // for the prompt: The assistant must not claim it is still going on
volatile int lastDreamDay = -1;  // YYYYMMDD of the last dream, -1 = not read from the core yet
uint32_t retryAt = 0;

int dayNumber(time_t t)
{
    struct tm local;
    localtime_r(&t, &local);
    return (local.tm_year + 1900) * 10000 + (local.tm_mon + 1) * 100 + local.tm_mday;
}

// The memory as the prompts get it, measured: what the dream is for.
struct Size {
    size_t prompt = 0, backend = 0;  // characters of the notes for GPT Live / for the backend model
    int facts = 0, openTopics = 0, topics = 0;
};

Size measure()  // caller holds the lock
{
    Size size;
    size.prompt = contextLocked(false, "", true).length();
    size.backend = contextLocked(true, "", true).length();
    size.facts = core["facts"].size();
    for (JsonObjectConst topic : core["topics"].as<JsonArrayConst>()) {
        ++size.topics;
        if (!(topic["done"] | false)) ++size.openTopics;
    }
    return size;
}

String change(size_t before, size_t after)
{
    const long delta = long(after) - long(before);
    return String(before) + " → " + String(after) + (before ? " (" + String(delta * 100 / long(before)) + " %)" : String());
}

// The last DREAM_DAYS days' conversations with people of the household (no guests, no calls for
// someone else: what strangers said must not become a memory), oldest first, at most
// DREAM_CONVERSATION_CHARS. `times`: the conversations' "at", the only valid evidence.
String recentConversations(std::set<String>& times)  // caller holds the lock
{
    std::vector<String> lines;
    const time_t since = time(nullptr) - DREAM_DAYS * 86400L;
    File index = LittleFS.open(INDEX_FILE, "r");
    while (index && index.available()) {
        const String line = index.readStringUntil('\n');
        JsonDocument entry;
        if (deserializeJson(entry, line) || (entry["t"] | 0LL) < since) continue;
        const String channel = entry["channel"] | "", at = entry["at"] | "", person = entry["person"] | "";
        // Mail notices summarize mails from anyone: no evidence for facts about the household.
        if (outsider(channel) || channel == MAIL_NOTICE || at.isEmpty()) continue;
        const String with = person.length() ? people::name(person) : String();
        const String label = channelLabel(channel.c_str());
        const String how = with.length() && label.length() ? with + ", " + label : with.length() ? with : label;
        lines.push_back("[" + at + "]" + (how.length() ? " (" + how + ")" : String()) + " " + (entry["title"] | "") + ": " +
                        (entry["summary"] | "") + "\n");
    }
    index.close();
    String text;
    size_t used = 0, first = lines.size();
    while (first > 0 && used + lines[first - 1].length() <= DREAM_CONVERSATION_CHARS) used += lines[--first].length();
    for (size_t i = first; i < lines.size(); ++i) {
        text += lines[i];
        times.insert(lines[i].substring(1, lines[i].indexOf(']')));
    }
    return text;
}

// Days with conversations among `evidence` (times not given to the model do not count).
int evidenceDays(JsonArrayConst evidence, const std::set<String>& times)
{
    std::set<String> days;
    for (JsonVariantConst at : evidence) {
        String time = at | "";
        time.replace("[", "");  // as written in the input: "[2026-10-02 16:28]"
        time.replace("]", "");
        time.trim();
        if (times.count(time.substring(0, 16))) days.insert(time.substring(0, 10));
    }
    return days.size();
}

JsonObject topicByExactTitle(const String& title)
{
    const String wanted = folded(title);
    for (JsonObject topic : core["topics"].as<JsonArray>())
        if (folded(topic["title"] | "") == wanted) return topic;
    return JsonObject();
}

// A topic's key words ("a, b, c") into `out`, without doubles.
void addKeywords(JsonArray out, const String& words)
{
    for (int start = 0; start < int(words.length());) {
        int end = words.indexOf(", ", start);
        if (end < 0) end = words.length();
        const String word = words.substring(start, end);
        bool known = false;
        for (JsonVariantConst w : out) known |= word == (w | "");
        if (word.length() && !known) out.add(word);
        start = end + 2;
    }
}

String factText(int id)
{
    JsonObject fact = findFact(id);
    return fact ? "[" + String(id) + "] " + String(fact["text"] | "") : "[" + String(id) + "] (unknown)";
}

// Applies the dream to the core (caller holds the lock). Returns the report for the diary, ""
// if it was not applied.
String apply(const JsonDocument& dream, const std::set<String>& times)
{
    const String diary = dream["diary"] | "";
    // Sanity first: a dream that would wipe most facts is not applied.
    std::set<int> gone;
    for (JsonObjectConst entry : dream["remove"].as<JsonArrayConst>())
        if (findFact(entry["id"] | 0)) gone.insert(entry["id"] | 0);
    for (JsonObjectConst entry : dream["merge"].as<JsonArrayConst>()) {
        JsonArrayConst ids = entry["ids"];
        for (size_t i = 1; i < ids.size(); ++i)
            if (findFact(ids[i] | 0)) gone.insert(ids[i] | 0);
    }
    const int facts = core["facts"].size();
    if (facts >= 5 && int(gone.size()) * 100 > facts * DREAM_MAX_REMOVED_PERCENT) {
        Serial.printf("[Dream] would remove %u of %d facts: not applied\n", unsigned(gone.size()), facts);
        return "";
    }
    fileutil::saveJson(BEFORE_FILE, core);  // to compare or roll back by hand
    const Size before = measure();
    const String today = timeutil::today(), at = timeutil::format(time(nullptr), "%Y-%m-%d %H:%M");
    String report;

    for (JsonObjectConst entry : dream["rewrite"].as<JsonArrayConst>()) {
        JsonObject fact = findFact(entry["id"] | 0);
        const String text = entry["text"] | "";
        if (!fact || text.isEmpty()) continue;
        report += "- umformuliert " + factText(entry["id"] | 0) + "\n  → " + text + "\n";
        fact["text"] = text;
        fact["category"] = category(entry["category"] | "");
    }
    for (JsonObjectConst entry : dream["merge"].as<JsonArrayConst>()) {
        JsonArrayConst ids = entry["ids"];
        const String text = entry["text"] | "";
        JsonObject kept = ids.size() > 1 ? findFact(ids[0] | 0) : JsonObject();
        if (!kept || text.isEmpty()) continue;
        report += "- zusammengeführt:";
        for (JsonVariantConst id : ids) report += " " + factText(id | 0) + ";";
        report += "\n  → " + text + "\n";
        kept["text"] = text;
        kept["category"] = category(entry["category"] | "");
        for (size_t i = 1; i < ids.size(); ++i) removeFact(ids[i] | 0);
    }
    for (JsonObjectConst entry : dream["remove"].as<JsonArrayConst>()) {
        const int id = entry["id"] | 0;
        if (!findFact(id)) continue;
        report += "- entfernt " + factText(id) + " (" + (entry["reason"] | "") + ")\n";
        removeFact(id);
    }
    for (JsonObjectConst entry : dream["topics"].as<JsonArrayConst>()) {
        JsonObject topic = topicByExactTitle(entry["title"] | "");
        if (!topic) continue;
        const String status = entry["status"] | "";
        const bool done = entry["done"] | false;
        if ((status.isEmpty() || status == String(topic["status"] | "")) && done == (topic["done"] | false))
            continue;  // listed but unchanged
        JsonDocument update;
        update["title"] = topic["title"] | "";
        update["status"] = status.length() ? status : String(topic["status"] | "");
        addKeywords(update["keywords"].to<JsonArray>(), topic["keywords"] | "");
        update["notes"] = String("Nächtliche Durchsicht: ") + (entry["reason"] | "");
        update["done"] = done;
        report += "- Thema „" + String(topic["title"] | "") + "“" + (done ? " erledigt" : " aktualisiert") + " (" +
                  (entry["reason"] | "") + ")\n";
        applyTopic(update.as<JsonObjectConst>(), at, "Nächtliche Durchsicht", false);
    }
    // Topics that are one: the first keeps going with the merged status, the others are closed.
    for (JsonObjectConst entry : dream["topic_merge"].as<JsonArrayConst>()) {
        JsonArrayConst titles = entry["titles"];
        JsonObject kept = titles.size() > 1 ? topicByExactTitle(titles[0] | "") : JsonObject();
        const String status = entry["status"] | "";
        if (!kept || status.isEmpty()) continue;
        const String keptTitle = kept["title"] | "";
        JsonDocument update;
        JsonArray keywords = update["keywords"].to<JsonArray>();
        addKeywords(keywords, kept["keywords"] | "");
        report += "- Themen zusammengeführt in „" + keptTitle + "“:";
        for (size_t i = 1; i < titles.size(); ++i) {
            JsonObject other = topicByExactTitle(titles[i] | "");
            if (!other || other == kept) continue;
            report += " „" + String(other["title"] | "") + "“";
            addKeywords(keywords, other["keywords"] | "");
            JsonDocument close;
            close["title"] = other["title"] | "";
            close["status"] = "Zusammengeführt in „" + keptTitle + "“.";
            addKeywords(close["keywords"].to<JsonArray>(), other["keywords"] | "");
            close["notes"] = "Nächtliche Durchsicht: zusammengeführt in „" + keptTitle + "“ (" + (entry["reason"] | "") + ")";
            close["done"] = true;
            applyTopic(close.as<JsonObjectConst>(), at, "Nächtliche Durchsicht", false);
        }
        report += " (" + String(entry["reason"] | "") + ")\n";
        update["title"] = keptTitle;
        update["status"] = status;
        update["notes"] = String("Nächtliche Durchsicht: andere Themen hier zusammengeführt");
        update["done"] = kept["done"] | false;
        applyTopic(update.as<JsonObjectConst>(), at, "Nächtliche Durchsicht", false);
    }
    // Profile fields said shorter (the content stays; the model must not drop what still holds).
    for (JsonObjectConst entry : dream["profiles"].as<JsonArrayConst>()) {
        const String person = entry["person"] | "", field = entry["field"] | "", value = entry["value"] | "";
        const String old = core["profiles"][person][field] | "";
        if (value.isEmpty() || old.isEmpty() || value == old) continue;  // never cleared or filled by a dream
        report += "- Profil " + people::name(person) + " / " + field + " verdichtet: " + old + "\n  → " + value + "\n";
        setProfile(person, field.c_str(), value);
    }
    // Behaviour rules: merged or said clearer (only rules of the same person), and a rule removed
    // only when a newer one of the same person replaced it: never silently.
    for (JsonObjectConst entry : dream["rules"].as<JsonArrayConst>()) {
        JsonArrayConst ids = entry["ids"];
        const String text = entry["text"] | "";
        JsonObject kept = ids.size() ? findRule(ids[0] | 0) : JsonObject();
        if (!kept || text.isEmpty() || text.length() > RULE_CHARS) continue;
        const String owner = kept["person"] | "";
        bool same = true;
        for (JsonVariantConst id : ids) {
            JsonObject rule = findRule(id | 0);
            same &= rule && owner == String(rule["person"] | "");
        }
        if (!same) continue;
        report += "- Regel" + String(ids.size() > 1 ? "n zusammengeführt:" : " umformuliert:");
        for (JsonVariantConst id : ids) report += " [" + String(id | 0) + "] " + String(findRule(id | 0)["text"] | "") + ";";
        report += "\n  → " + text + " (" + (entry["reason"] | "") + ")\n";
        for (size_t i = 1; i < ids.size(); ++i) {
            JsonArray rules = core["rules"];
            for (size_t j = 0; j < rules.size(); ++j)
                if (rules[j]["id"].as<int>() == (ids[i] | 0)) rules.remove(j);
        }
        setRule(owner, text, ids[0] | 0);
    }
    for (JsonObjectConst entry : dream["rules_remove"].as<JsonArrayConst>()) {
        const int id = entry["id"] | 0, newer = entry["newer"] | 0;
        JsonObject rule = findRule(id), replacement = findRule(newer);
        if (!rule || !replacement || newer <= id || String(rule["person"] | "") != String(replacement["person"] | ""))
            continue;
        report += "- Regel entfernt [" + String(id) + "] " + String(rule["text"] | "") + " (ersetzt durch [" + String(newer) +
                  "]: " + (entry["reason"] | "") + ")\n";
        JsonArray rules = core["rules"];
        for (size_t j = 0; j < rules.size(); ++j)
            if (rules[j]["id"].as<int>() == id) rules.remove(j);
    }
    String patterns;
    for (JsonObjectConst entry : dream["patterns"].as<JsonArrayConst>()) {
        const int days = evidenceDays(entry["evidence"], times);
        patterns += (days >= DREAM_MIN_DAYS ? "- " : "- verworfen (Belege an " + String(days) + " Tag(en)): ") +
                    String(entry["text"] | "") + "\n";
    }
    for (JsonObjectConst entry : dream["add"].as<JsonArrayConst>()) {
        const String text = entry["text"] | "";
        const int days = evidenceDays(entry["evidence"], times);
        if (text.isEmpty()) continue;
        if (days < DREAM_MIN_DAYS) {
            report += "- verworfen (Belege an " + String(days) + " Tag(en)): " + text + "\n";
            continue;
        }
        report += "- neu [" + String(addFact(entry["category"] | "", text, today)) + "] " + text + "\n";
    }
    const Size after = measure();

    const String sizes = "Gedächtnis im Prompt: " + change(before.prompt, after.prompt) + " Zeichen (Backend " +
                         change(before.backend, after.backend) + "), Fakten " + change(before.facts, after.facts) +
                         ", offene Themen " + change(before.openTopics, after.openTopics);
    // The dream itself and its suggestions: in the prompts today and tomorrow (dreamContext),
    // so Jarvis can tell about it ("Wie war dein Traum?").
    JsonObject saved = core["dream"].to<JsonObject>();
    saved["date"] = today;
    saved["at"] = timeutil::format(time(nullptr), "%H:%M");
    saved["diary"] = diary;
    saved["result"] = sizes;
    JsonArray suggestions = saved["suggestions"].to<JsonArray>();
    String offers;
    for (JsonObjectConst entry : dream["suggestions"].as<JsonArrayConst>()) {
        const String text = entry["text"] | "", person = entry["person"] | "";
        if (text.isEmpty()) continue;
        JsonObject suggestion = suggestions.add<JsonObject>();
        suggestion["person"] = person;
        suggestion["text"] = text;
        offers += "- " + (person.length() ? people::name(person) + ": " : String()) + text + "\n";
    }
    saveCore();
    queueCoreBackup();
    Serial.printf("[Dream] %s\n", sizes.c_str());
    return sizes + "\n\nÄnderungen:\n" + (report.length() ? report : String("- keine\n")) + "\nMuster:\n" +
           (patterns.length() ? patterns : String("- keine\n")) + "\nVorschläge:\n" + (offers.length() ? offers : String("- keine\n"));
}

}  // namespace

bool dreamDue()
{
    if (!idle || !mounted || int32_t(millis() - retryAt) < 0) return false;
    if (requested) return true;
    if (!timeutil::synced()) return false;
    const time_t now = time(nullptr);
    struct tm local;
    localtime_r(&now, &local);
    if (local.tm_hour != DREAM_HOUR) return false;
    if (lastDreamDay < 0) {
        Locked guard;
        const String date = core["dream"]["date"] | "";
        lastDreamDay = date.length() == 10 ? date.substring(0, 4).toInt() * 10000 + date.substring(5, 7).toInt() * 100 +
                                                 date.substring(8, 10).toInt()
                                           : 0;
    }
    return lastDreamDay != dayNumber(now);
}

void requestDream() { requested = true; }

String undoDream()
{
    Locked guard;
    File file = LittleFS.exists(BEFORE_FILE) ? LittleFS.open(BEFORE_FILE, "r") : File();
    if (!file) return "Error: there is no dream to undo.";
    JsonDocument before(PsramJson::instance());
    const bool broken = deserializeJson(before, file) != DeserializationError::Ok;
    file.close();
    if (broken) return "Error: the copy from before the dream is not readable.";
    core.set(before);
    normalizeCore();
    saveCore();
    queueCoreBackup();
    LittleFS.remove(BEFORE_FILE);  // once: a second undo would go back further than one dream
    Serial.println("[Dream] undone: the memory is as before the last dream");
    return "Undone: the memory is as before the last dream (its diary stays in the archive).";
}

String dreamContext(const String& person)
{
    // The state first: in a chat the earlier answer "it is running" must not outlive the dream.
    if (requested || running)
        return "Dreaming (the review of your memory across conversations): a dream is running right now; it takes "
               "about a minute.\n";
    const String date = core["dream"]["date"] | "";
    const time_t now = time(nullptr);
    if (date != timeutil::format(now, "%Y-%m-%d") && date != timeutil::format(now - 86400, "%Y-%m-%d")) return "";
    const String with = speaker(person);
    String ideas;
    for (JsonObjectConst suggestion : core["dream"]["suggestions"].as<JsonArrayConst>()) {
        const String forWhom = suggestion["person"] | "";
        if (with.length() && forWhom.length() && forWhom != with) continue;
        ideas += "- " + (forWhom.length() ? people::name(forWhom) + ": " : String()) + (suggestion["text"] | "") + "\n";
    }
    // What the user may ask about ("Hast du gut geträumt?"): when, the diary, what changed.
    String text = "Dreaming (the review of your memory across conversations, normally at night): no dream is running "
                  "now; the last one finished " + date + " " + (core["dream"]["at"] | "") + ". Your diary of it: " +
                  (core["dream"]["diary"] | "(none)") + " Result: " + (core["dream"]["result"] | "") + "\n";
    if (ideas.length())
        text += "Ideas from that dream (offer one at a fitting moment, briefly, at most one per conversation, not if the "
                "person declined it or it is done):\n" + ideas;
    return text;
}

void dream()
{
    struct Running {  // set before `requested` is cleared, cleared on every way out
        Running() { running = true; }
        ~Running() { running = false; }
    } flag;
    const bool manual = requested;
    requested = false;
    const uint32_t started = millis();
    JsonDocument body(PsramJson::instance());
    std::set<String> times;
    {
        Locked guard;
        String topics;
        for (JsonObjectConst topic : core["topics"].as<JsonArrayConst>())
            topics += "- " + String(topic["title"] | "") + " (" + ((topic["done"] | false) ? "done" : "open") + ", " +
                      (topic["updated"] | "") + "): " + (topic["status"] | "") + "\n";
        const String conversations = recentConversations(times), rules = rulesList();
        body["input"] = "Today: " + timeutil::format(time(nullptr), "%A, %Y-%m-%d") + "\n\nMemory:\n" + contextLocked(true, "", true) +
                        "\nAll known topics:\n" + (topics.length() ? topics : String("(none)\n")) +
                        "\nJarvis' behaviour rules (oldest first):\n" + (rules.length() ? rules : String("(none)\n")) +
                        "\nConversations of the last " + String(DREAM_DAYS) + " days (oldest first):\n" +
                        (conversations.length() ? conversations : String("(none)\n"));
    }
    body["model"] = DELEGATION_MODEL;
    body["instructions"] = String(DREAM_INSTRUCTIONS) + "\n" + prompt::context();
    body["reasoning"]["effort"] = "medium";  // a whole memory at once: worth some thought
    body["store"] = false;
    JsonObject format = body["text"]["format"].to<JsonObject>();
    format["type"] = "json_schema";
    format["name"] = "dream";
    format["strict"] = true;
    String ids;
    for (const people::Person& p : people::all()) ids += String(ids.length() ? ", " : "") + "\"" + p.id + "\"";
    String schemaText = SCHEMA;
    schemaText.replace("PEOPLE", ids);
    JsonDocument schema(PsramJson::instance());
    if (deserializeJson(schema, schemaText)) Serial.println("[Dream] schema invalid");
    format["schema"] = schema;
    Serial.printf("[Dream] %s: %u conversations of the last %d days\n", manual ? "started by hand" : "night",
                  unsigned(times.size()), DREAM_DAYS);

    PsramSink sink(512 * 1024);
    const int status = openai::post("/v1/responses", body, sink);
    JsonDocument response(PsramJson::instance()), dream(PsramJson::instance());
    if (status != 200 || deserializeJson(response, sink.data, sink.size, DeserializationOption::NestingLimit(32)) ||
        deserializeJson(dream, outputText(response))) {
        Serial.printf("[Dream] failed: %s\n", status != 200 ? openai::errorMessage(status, sink).c_str() : "answer not readable");
        if (!manual) retryAt = millis() + RETRY_MS;
        return;
    }
    Serial.printf("[Dream] answer after %lu s, tokens in %d, out %d\n", (millis() - started) / 1000,
                  response["usage"]["input_tokens"] | 0, response["usage"]["output_tokens"] | 0);
    Locked guard;
    const String report = apply(dream, times);
    lastDreamDay = dayNumber(time(nullptr));
    if (report.isEmpty()) {
        core["dream"]["date"] = timeutil::today();  // not again tonight
        saveCore();
        return;
    }
    const String diary = dream["diary"] | "";
    Serial.printf("[Dream] diary: %s\n[Dream] report:\n%s\n", diary.c_str(), report.c_str());
    const String today = timeutil::today();
    queueNote(MEMORY_FOLDER_CONVERSATIONS, "Traum " + today, "",
              "Traum vom " + today + " (nächtliche Durchsicht des Gedächtnisses)\n\n" + diary + "\n\n" + report);
}

}  // namespace internal
}  // namespace memory
