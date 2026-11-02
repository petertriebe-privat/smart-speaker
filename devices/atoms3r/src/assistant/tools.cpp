#include "assistant/tools.h"

#include <esp_heap_caps.h>
#include <vector>

#include "assistant/agenda.h"
#include "assistant/agent.h"
#include "assistant/backup.h"
#include "assistant/files.h"
#include "assistant/mail_watch.h"
#include "config.h"
#include "assistant/memory.h"
#include "assistant/people.h"
#include "assistant/shared.h"
#include "assistant/tasks.h"
#include "audio/volume.h"
#include "services/alexa.h"
#include "services/firetv.h"
#include "services/google.h"
#include "services/maps.h"
#include "services/ha_automations.h"
#include "services/home_assistant.h"
#include "services/images.h"
#include "services/presence.h"
#include "services/mail.h"
#include "services/mcp.h"
#include "services/psram_json.h"
#include "system/time_util.h"
#include "services/telegram.h"
#include "services/whatsapp.h"
#include "services/sheets.h"
#include "services/spotify.h"
#include "services/todos.h"
#include "services/websearch.h"
#include "services/youtube.h"
#include "phone/phone.h"
#include "system/occupancy.h"
#include "voice/live_session.h"

namespace tools {
namespace {

struct Entry {
    bool provider;
    Tool tool;
    Provider source;
};

std::vector<Entry> entries;  // registration order = the order the model sees

bool allowed(uint8_t channels, Channel channel) { return channels & channel; }

// --- Tools of this module ---------------------------------------------------------

String setVolume(JsonObjectConst arguments, const Context&)
{
    const int level = volume::request(arguments["level"] | -1, arguments["step"] | 0);
    return "{\"volume\":" + String(level) + "}";
}

// --- Worker for slow tools ------------------------------------------------------------

struct Job {
    String name, arguments, callId;
    Channel channel;
    String person, origin;
    void (*work)();  // post(): a function instead of a tool, else nullptr
};
struct Done {
    String callId, output;
};

QueueHandle_t jobs = nullptr, done = nullptr;
std::vector<void (*)()> background;

void worker(void*)
{
    for (;;) {
        Job* job = nullptr;
        if (xQueueReceive(jobs, &job, pdMS_TO_TICKS(1000)) == pdTRUE) {
            if (job->work) {
                job->work();
                delete job;
                continue;
            }
            const uint32_t started = millis();
            const String output = run(job->name.c_str(), job->arguments.c_str(), job->channel, job->person, job->origin);
            if (job->channel == Console) {  // same format as a direct call (tests/device)
                Serial.printf("[Tool] %s %s\n-> %s\n[Tool-End]\n", job->name.c_str(), job->arguments.c_str(),
                              output.c_str());
            } else {
                Serial.printf("\n[Tool] %s -> %.200s (%lu ms)\n", job->name.c_str(), output.c_str(),
                              millis() - started);
                auto* result = new Done{job->callId, output};
                if (xQueueSend(done, &result, 0) != pdTRUE) delete result;
            }
            delete job;
            continue;
        }
        for (void (*work)() : background) work();  // calls first: they return only when nothing waits
    }
}

}  // namespace

void begin()
{
    entries.clear();
    jobs = xQueueCreate(4, sizeof(Job*));
    done = xQueueCreate(4, sizeof(Done*));
    // TLS handshakes, ReadyMail's parser and large JSON need a large stack; CPU 1 like the other
    // TLS tasks (images.cpp: task watchdog on CPU 0 during long uploads).
    if (!jobs || !done || xTaskCreatePinnedToCore(worker, "tools", TASK_STACK_BYTES, nullptr, 1, nullptr, 1) != pdPASS)
        Serial.println("[Tools] worker could not start");
    agent::addTools();  // speak: first in the agent's list
    websearch::addTools();  // Tavily, or OpenAI's hosted search without a key
    add(Tool{"set_volume",
             "Sets the speaker volume. Give either level (absolute value in percent) or step (relative "
             "change, e.g. -10 for 'leiser', +10 for 'lauter'), not both. Returns the new volume.",
             R"json({"type": "object", "additionalProperties": false,
                 "properties": {"level": {"type": "integer", "minimum": 0, "maximum": 100},
                                "step": {"type": "integer", "minimum": -100, "maximum": 100}},
                 "required": []})json",
             setVolume, 0, Everywhere});
    memory::addTools();
    people::addTools();
    tasks::addTools();
    // Home Assistant's tools come from HA over MCP (services/home_assistant).
    add(Provider{"home_assistant", [](JsonArray list, const String&) { ha::addTools(list); }, ha::hasTool,
                 [](const char* name, JsonObjectConst arguments, const Context&) { return ha::callTool(name, arguments); },
                 nullptr, 0, Everywhere});
    mcp::addTools();  // further MCP servers the admin connected
    automations::addTools();
    images::addTools();
    mail::addTools();
    mailwatch::addTools();
    telegram::addTools();
    whatsapp::addTools();
    files::addTools();
    backup::addTools();
    phone::addTools();
    live::addTools();
    google::addTools();
    maps::addTools();
    todos::addTools();
    agenda::addTools();
    shared::addTools();  // a member: the leader's affairs (#71)
    sheets::addTools();
    youtube::addTools();
    alexa::addTools();
    firetv::addTools();
    spotify::addTools();
    presence::begin();
    occupancy::addTools();
}

void add(const Tool& tool) { entries.push_back(Entry{false, tool, Provider{}}); }
void add(const Provider& provider) { entries.push_back(Entry{true, Tool{}, provider}); }

const char* keep(const String& text)
{
    auto* copy = static_cast<char*>(heap_caps_malloc(text.length() + 1, MALLOC_CAP_SPIRAM | MALLOC_CAP_8BIT));
    if (!copy) return "";
    memcpy(copy, text.c_str(), text.length() + 1);
    return copy;
}

void describe(JsonArray list, Channel channel, const String& person)
{
    for (const Entry& entry : entries) {
        if (entry.provider) {
            const Provider& source = entry.source;
            if (allowed(source.channels, channel) && (!source.available || source.available())) source.describe(list, person);
            continue;
        }
        const Tool& tool = entry.tool;
        if (!allowed(tool.channels, channel)) continue;
        if (person.length() && people::denied(person, tool.name, tool.flags & Private).length()) continue;
        JsonObject definition = list.add<JsonObject>();
        if (tool.flags & Hosted) {
            definition["type"] = tool.name;
            continue;
        }
        definition["type"] = "function";
        definition["name"] = tool.name;
        definition["description"] = tool.description;
        JsonDocument schema(PsramJson::instance());
        if (deserializeJson(schema, tool.parameters)) Serial.printf("[Tools] schema of %s invalid\n", tool.name);
        definition["parameters"] = schema;
    }
}

bool isSlow(const char* name)
{
    if (shared::remote(name)) return true;  // over the LAN to the leader: not in the main loop
    for (const Entry& entry : entries) {
        if (!entry.provider && !strcmp(entry.tool.name, name)) return entry.tool.flags & Slow;
        if (entry.provider && entry.source.has(name)) return entry.source.flags & Slow;
    }
    return false;
}

String fromLine(const Context& context, const String& recipient)
{
    const String to = recipient.length() ? recipient : people::admin();
    // Nobody named: only a voice conversation has an unknown sender (a task or the console is the admin's).
    const bool voice = context.origin == "Lautsprecher" || context.origin.startsWith("Anruf");
    if ((context.person.isEmpty() && !voice) || to == context.person) return "";
    const String name = people::name(context.person);
    const String who = name.length() ? name : context.origin == "Lautsprecher" ? String("jemandem zu Hause") : String("jemandem");
    const String when = timeutil::format(time(nullptr), "%d.%m. %H:%M");
    return "Von " + who + " (" + (context.origin.length() ? context.origin + ", " : String()) + when + "):";
}

String run(const char* name, const char* argumentsJson, Channel channel, const String& person, const String& origin)
{
    JsonDocument arguments(PsramJson::instance());
    deserializeJson(arguments, argumentsJson);
    const Context context{channel, person, origin};
    for (const Entry& entry : entries) {
        if (!entry.provider) {
            const Tool& tool = entry.tool;
            if (strcmp(tool.name, name)) continue;
            if (!allowed(tool.channels, channel) || (tool.flags & Hosted)) break;
            const String refused = people::denied(person, name, tool.flags & Private);
            if (refused.length()) return refused;
            String forwarded;  // a member of a network: memory, tasks and people are the leader's
            if (shared::forward(name, argumentsJson, context, forwarded)) return forwarded;
            return tool.handler(arguments.as<JsonObjectConst>(), context);
        }
        const Provider& source = entry.source;
        if ((source.available && !source.available()) || !source.has(name)) continue;
        if (!allowed(source.channels, channel)) break;
        const String refused = people::denied(person, name, false);
        if (refused.length()) return refused;
        return source.call(name, arguments.as<JsonObjectConst>(), context);
    }
    return "Error: unknown function";
}

bool start(const char* name, const char* argumentsJson, const char* callId, Channel channel, String& error,
           const String& person, const String& origin)
{
    auto* job = new Job{name, argumentsJson, callId, channel, person, origin, nullptr};
    if (!jobs || xQueueSend(jobs, &job, 0) != pdTRUE) {
        delete job;
        error = "Error: too many slow calls at once, please try again in a moment";
        return false;
    }
    return true;
}

void addBackgroundWork(void (*work)()) { background.push_back(work); }

namespace {
std::vector<void (*)()> conversationWork;  // registered at start, before the tasks run it
SemaphoreHandle_t conversationWorkLock = xSemaphoreCreateMutex();
}  // namespace

void addConversationWork(void (*work)()) { conversationWork.push_back(work); }

void runConversationWork()
{
    if (conversationWork.empty() || !live::open()) return;
    if (xSemaphoreTake(conversationWorkLock, 0) != pdTRUE) return;  // the other task runs it
    for (void (*work)() : conversationWork) work();
    xSemaphoreGive(conversationWorkLock);
}

bool post(void (*work)())
{
    auto* job = new Job{"", "", "", Voice, "", "", work};
    if (!jobs || xQueueSend(jobs, &job, 0) != pdTRUE) {
        delete job;
        return false;
    }
    return true;
}

bool takeResult(String& callId, String& output)
{
    Done* result = nullptr;
    if (!done || xQueueReceive(done, &result, 0) != pdTRUE) return false;
    callId = result->callId;
    output = result->output;
    delete result;
    return true;
}

}  // namespace tools
