#pragma once

#include <Arduino.h>
#include <ArduinoJson.h>

// Home Assistant as MCP server ("Model Context Protocol Server" integration) in the LAN.
// The device is the MCP client: it loads HA's tools once, offers them to the backend model
// as function tools and forwards calls to HA. The server is stateless, so every request is
// a single JSON-RPC POST to /api/mcp without initialize.
namespace ha {

bool configured();                 // HA_URL and HA_TOKEN in secrets.h
bool toolsLoaded();
bool loadTools();                  // tools/list; blocks up to a few seconds
// Loads the tools if missing, retrying every HA_RETRY_MS. Only between conversations
// (`allowed`): the request blocks.
void update(bool allowed);
// False for HA_AWAY_MS after a connection to HA failed: the main loop's own requests (the tools'
// retry, the room sensor's report) wait, each would block it for the connect timeout.
bool reachable();
void addTools(JsonArray tools);    // as Responses function tools
bool hasTool(const char* name);
String callTool(const char* name, JsonObjectConst arguments);  // result text for the model

// One request to HA's REST API (/api/...), for what MCP does not offer (automations, service
// calls, templates). Returns the HTTP status (negative: HTTPClient error); the body lands in
// `response` (JSON, or {"message": text}). Blocks up to 8 s.
int rest(const char* method, const String& path, const String& payload, JsonDocument& response,
         const JsonDocument* filter = nullptr);
// Error text for the model from rest()'s status and response.
String failure(int status, const JsonDocument& response);

// The states of one domain ("person", "automation"; "" = all), only those whose entity_id or name
// contains `query` (case-insensitive; "" = all), at most `limit`: a JSON array as /api/states gives
// (entity_id, state, last_changed, last_updated, attributes: friendly_name, id, last_triggered,
// latitude, longitude, gps_accuracy). HA filters itself through the template API: never /api/states
// on the M5, whose ~165 KB left the M5's network dead until a restart (#85, 25.10.2026).
int states(const String& domain, const String& query, size_t limit, JsonDocument& response);

// What HA's own sentence matching did with a text (assist).
struct AssistResult {
    String type;    // action_done, query_answer or error
    String code;    // for an error: no_intent_match, no_valid_targets, ...
    String speech;  // HA's answer in its language ("Eingeschaltet")
    int done = 0, failed = 0;  // targets that did it / failed
};
// Hands a sentence to HA's built-in conversation agent ("Licht in der Küche aus"): HA's own
// sentence matching in the LAN, which knows the areas, names and aliases (no household data
// here). Not the default agent, which may be an LLM. HA acts at once when it matches: only what
// is meant as a command. Blocks up to timeoutMs; false = no answer (the reason in result.code).
bool assist(const String& text, AssistResult& result, uint16_t timeoutMs);

}  // namespace ha
