#pragma once

#include <Arduino.h>
#include <ArduinoJson.h>

// The tool registry of the backend model, like MCP: every tool is one entry with name,
// description and a JSON schema of its arguments, plus how it runs (handler, flags) and where it
// may be used (channels). Tools known only at run time (Home Assistant's via MCP) come from
// providers with the same shape. Voice conversations (voice/live_session), the agent (mail
// commands, scheduled tasks, Home Assistant events) and the serial console all go through it.
namespace tools {

// Where a call comes from; a tool is offered and runs only in its channels.
enum Channel : uint8_t {
    Voice = 1 << 0,    // voice conversation (GPT Live's backend)
    Agent = 1 << 1,    // agent runs: mail commands, scheduled tasks, Home Assistant events
    Console = 1 << 2,  // serial ">tool {json}" (tests)
    Stranger = 1 << 3,  // a phone call to someone else (phone_call): only what that call needs
};
constexpr uint8_t Everywhere = Voice | Agent | Console;

enum Flag : uint8_t {
    Hosted = 1 << 0,  // run by OpenAI (web_search): offered as {"type": name}, never called here
    Slow = 1 << 1,    // seconds of network: a conversation gets the answer later
    Private = 1 << 2,  // the admin's own accounts and the assistant's administration: not for other people
};

struct Context {
    Channel channel;
    String person;  // who asked (assistant/people id), "" = unknown (at home, nobody named)
    String origin;  // how it came in, for people: "Anruf", "Lautsprecher", "Telegram", "E-Mail", ...
};

// A German sender line for something passed on to another person of the household ("Von Anna
// (Anruf, 30.09. 14:05):"), "" when `recipient` (people id, "" = the admin) is the one who asked
// or nobody asked (console).
String fromLine(const Context& context, const String& recipient);

// Result text for the model (errors start with "Error").
using Handler = String (*)(JsonObjectConst arguments, const Context& context);

struct Tool {
    const char* name;
    const char* description;  // English for the model; text built at run time: keep() it
    const char* parameters;   // JSON schema of the arguments (an object)
    Handler handler;
    uint8_t flags;
    uint8_t channels;
};

// Tools known only at run time, e.g. Home Assistant's and other MCP servers'.
struct Provider {
    const char* name;
    // Adds the Responses definitions, without those `person` may not use ("" = anyone).
    void (*describe)(JsonArray tools, const String& person);
    bool (*has)(const char* tool);
    String (*call)(const char* tool, JsonObjectConst arguments, const Context& context);
    bool (*available)();  // nullptr = always
    uint8_t flags;
    uint8_t channels;
};

// Registration, once at start and in the order the model sees the tools.
void begin();
void add(const Tool& tool);
void add(const Provider& provider);
const char* keep(const String& text);  // a copy in PSRAM for descriptions built at run time

// As Responses tool definitions; without the tools `person` may not use (people::denied).
void describe(JsonArray tools, Channel channel, const String& person = "");
bool isSlow(const char* name);
// Runs one tool for `person` and returns its output for the model. Blocks for as long as the
// tool takes.
String run(const char* name, const char* argumentsJson, Channel channel, const String& person = "",
           const String& origin = "");

// Slow tools of a conversation run in the registry's worker task (one after another) while the
// WebSocket keeps going; the main loop hands their result back. From the console the worker
// prints the result itself. False (with `error`) if the queue is full.
bool start(const char* name, const char* argumentsJson, const char* callId, Channel channel, String& error,
           const String& person = "", const String& origin = "");
bool takeResult(String& callId, String& output);  // a conversation's finished call, once

// The worker runs from begin() on (24 KB stack, made while the internal RAM is still free: made
// on demand it sometimes found no room). When no call waits it runs this work about once a
// second, e.g. the mail check; it decides itself whether it is due.
void addBackgroundWork(void (*work)());
// Work that may run DURING a conversation (e.g. the mail check): never in the worker, whose
// time belongs to the conversation's slow calls (fluent speech first, then the conversation's
// tools, then notices). The Telegram and WhatsApp tasks run it between their polls
// (runConversationWork, one at a time): their chats wait a few seconds, never the conversation.
// It decides itself whether it is due.
void addConversationWork(void (*work)());
void runConversationWork();
// Runs `work` once in the worker, right away (queued like a slow call). False if the queue is full.
bool post(void (*work)());

}  // namespace tools
