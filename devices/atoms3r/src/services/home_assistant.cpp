#include "services/home_assistant.h"

#include <HTTPClient.h>

#include "config.h"
#include "services/psram_json.h"
#include "system/network.h"

#if __has_include("secrets.h")
#include "secrets.h"
#endif

namespace ha {
namespace {

constexpr uint16_t TIMEOUT_MS = 5000;
constexpr uint16_t REST_TIMEOUT_MS = 8000;

JsonDocument tools(PsramJson::instance());  // result.tools of tools/list, kept: not in internal RAM
bool loaded = false;
// millis() of the last failed connection, 0 = none since the last answer. With HA away (no route
// to its host) every attempt waits out the connect timeout: 5-8 s of the main loop for the tools'
// retry and the room sensor's report, wake word and peer link stalled (19.10.2026).
volatile uint32_t unreachableAt = 0;

void noteStatus(int status) { unreachableAt = status < 0 ? max<uint32_t>(1, millis()) : 0; }

// One JSON-RPC request. Returns the "result" member, or an error text in `error`.
bool request(const char* method, const JsonDocument& params, JsonDocument& response, String& error)
{
#if defined(HA_URL) && defined(HA_TOKEN)
    JsonDocument body;
    body["jsonrpc"] = "2.0";
    body["id"] = 1;
    body["method"] = method;
    if (!params.isNull()) body["params"] = params;
    String payload;
    serializeJson(body, payload);

    HTTPClient http;
    http.setTimeout(TIMEOUT_MS);
    http.setConnectTimeout(TIMEOUT_MS);
    if (!http.begin(String(HA_URL) + "/api/mcp")) { error = "bad HA_URL"; return false; }
    http.addHeader("Authorization", String("Bearer ") + HA_TOKEN);
    http.addHeader("Content-Type", "application/json");
    http.addHeader("Accept", "application/json, text/event-stream");
    const int status = http.POST(payload);
    noteStatus(status);
    if (status != 200) {
        error = status < 0 ? HTTPClient::errorToString(status) : "HTTP " + String(status);
        http.end();
        return false;
    }
    const DeserializationError parsed = deserializeJson(response, http.getStream(),
                                                         DeserializationOption::NestingLimit(32));
    http.end();
    if (parsed) { error = String("JSON: ") + parsed.c_str(); return false; }
    if (response["error"].is<JsonObject>()) {
        error = response["error"]["message"] | "MCP error";
        return false;
    }
    return true;
#else
    error = "HA_URL / HA_TOKEN missing in secrets.h";
    return false;
#endif
}

}  // namespace

// One REST request to Home Assistant. Returns the HTTP status (negative: HTTPClient error);
// the body lands in `response` (JSON, or {"message": text}).
int rest(const char* method, const String& path, const String& payload, JsonDocument& response,
         const JsonDocument* filter)
{
#if defined(HA_URL) && defined(HA_TOKEN)
    HTTPClient http;
    http.useHTTP10(true);  // no chunked encoding: the body streams straight into the parser
    http.setTimeout(REST_TIMEOUT_MS);
    http.setConnectTimeout(REST_TIMEOUT_MS);
    if (!http.begin(String(HA_URL) + path)) return -1;
    http.addHeader("Authorization", String("Bearer ") + HA_TOKEN);
    http.addHeader("Content-Type", "application/json");
    const int status = http.sendRequest(method, payload);
    noteStatus(status);
    if (status > 0) {
        const DeserializationError error =
            filter ? deserializeJson(response, http.getStream(), DeserializationOption::Filter(*filter))
                   : deserializeJson(response, http.getStream(), DeserializationOption::NestingLimit(32));
        if (error) response["message"] = "answer not readable";
    }
    http.end();
    return status;
#else
    return -1;
#endif
}

bool assist(const String& text, AssistResult& result, uint16_t timeoutMs)
{
#if defined(HA_URL) && defined(HA_TOKEN)
    JsonDocument body;
    body["text"] = text;
    body["agent_id"] = "conversation.home_assistant";
    String payload;
    serializeJson(body, payload);
    HTTPClient http;
    http.useHTTP10(true);
    http.setTimeout(timeoutMs);
    http.setConnectTimeout(timeoutMs);
    if (!http.begin(String(HA_URL) + "/api/conversation/process")) {
        result.code = "bad HA_URL";
        return false;
    }
    http.addHeader("Authorization", String("Bearer ") + HA_TOKEN);
    http.addHeader("Content-Type", "application/json");
    const int status = http.POST(payload);
    noteStatus(status);
    if (status != 200) {
        result.code = status < 0 ? HTTPClient::errorToString(status) : "HTTP " + String(status);
        http.end();
        return false;
    }
    JsonDocument filter;
    filter["response"]["response_type"] = true;
    filter["response"]["speech"]["plain"]["speech"] = true;
    filter["response"]["data"]["code"] = true;
    filter["response"]["data"]["success"] = true;
    filter["response"]["data"]["failed"] = true;
    JsonDocument response;
    const DeserializationError error =
        deserializeJson(response, http.getStream(), DeserializationOption::Filter(filter));
    http.end();
    if (error) {
        result.code = String("JSON: ") + error.c_str();
        return false;
    }
    JsonObjectConst r = response["response"];
    result.type = r["response_type"] | "";
    result.code = r["data"]["code"] | "";
    result.speech = r["speech"]["plain"]["speech"] | "";
    result.done = r["data"]["success"].size();
    result.failed = r["data"]["failed"].size();
    return true;
#else
    result.code = "HA_URL / HA_TOKEN missing in secrets.h";
    return false;
#endif
}

int states(const String& domain, const String& query, size_t limit, JsonDocument& response)
{
    for (char c : domain)
        if (!(isLowerCase(c) || c == '_')) {
            response["message"] = "bad domain";
            return 400;
        }
    String q = query;
    q.toLowerCase();
    String quoted;  // a JSON string is a valid Jinja string literal
    JsonDocument literal;
    literal.set(q);
    serializeJson(literal, quoted);
    // Only JSON-safe attribute values: a datetime (last_triggered) or a tuple (a light's colour)
    // made HA refuse the whole template; such values go as text.
    String tpl = "{% set ns = namespace(n=0, first=true) %}[{% for s in " +
                 String(domain.length() ? "states." + domain : "states") + " %}" +
                 "{% if ns.n < " + String(limit) + " and " + quoted + " in (s.entity_id ~ ' ' ~ s.name) | lower %}" +
                 "{% set ns.n = ns.n + 1 %}{% set a = namespace(d={}) %}"
                 "{% for k in ['friendly_name', 'id', 'last_triggered', 'latitude', 'longitude', 'gps_accuracy'] %}"
                 "{% set v = s.attributes.get(k) %}{% if v is not none %}"
                 "{% set a.d = dict(a.d, **{k: (v if v is number or v is string or v is boolean else v | string)}) %}"
                 "{% endif %}{% endfor %}"
                 "{{ '' if ns.first else ',' }}{% set ns.first = false %}"
                 "{{ {'entity_id': s.entity_id, 'state': s.state, 'last_changed': s.last_changed.isoformat(), "
                 "'last_updated': s.last_updated.isoformat(), 'attributes': a.d} | tojson }}"
                 "{% endif %}{% endfor %}]";
    JsonDocument body;
    body["template"] = tpl;
    String payload;
    serializeJson(body, payload);
    return rest("POST", "/api/template", payload, response);
}

String failure(int status, const JsonDocument& response)
{
    // with the WiFi link: "connection refused" was "Host is unreachable" in lwIP, and on 25.10.2026 HA,
    // the proxy and the VPS answered the PC at once while the M5 got nothing (#85)
    if (status < 0)
        return "Error: Home Assistant not reachable (" + HTTPClient::errorToString(status) + "; WiFi " + network::link() +
               ")";
    String message = response["message"] | "";
    return "Error from Home Assistant (HTTP " + String(status) + ")" + (message.length() ? ": " + message : "");
}

bool configured()
{
#if defined(HA_URL) && defined(HA_TOKEN)
    return true;
#else
    return false;
#endif
}

bool toolsLoaded() { return loaded; }

bool loadTools()
{
    JsonDocument response;
    String error;
    if (!request("tools/list", JsonDocument(), response, error)) {
        Serial.printf("[Home Assistant] tools not loaded: %s\n", error.c_str());
        return false;
    }
    tools = response["result"]["tools"];
    loaded = true;
    Serial.print("[Home Assistant] Tools:");
    for (JsonObjectConst tool : tools.as<JsonArrayConst>()) Serial.printf(" %s", (const char*)(tool["name"] | "?"));
    Serial.println();
    return true;
}

bool reachable() { return !unreachableAt || millis() - unreachableAt >= HA_AWAY_MS; }

void update(bool allowed)
{
    static uint32_t lastAttempt = 0;
    if (!allowed || loaded || !configured() || !reachable()) return;
    if (lastAttempt && millis() - lastAttempt < HA_RETRY_MS) return;
    lastAttempt = millis();
    loadTools();
}

void addTools(JsonArray out)
{
    for (JsonObjectConst tool : tools.as<JsonArrayConst>()) {
        JsonObject function = out.add<JsonObject>();
        function["type"] = "function";
        function["name"] = tool["name"];
        function["description"] = tool["description"];
        function["parameters"] = tool["inputSchema"];
    }
}

bool hasTool(const char* name)
{
    for (JsonObjectConst tool : tools.as<JsonArrayConst>())
        if (!strcmp(tool["name"] | "", name)) return true;
    return false;
}

String callTool(const char* name, JsonObjectConst arguments)
{
    // HA's schemas mark nothing as required, so the backend model fills every slot with an
    // empty value ("floor": "", "device_class": [], "temperature": 0). HA rejects those as
    // "invalid slot info": pass only the slots that carry a value.
    JsonDocument params;
    params["name"] = name;
    JsonObject cleaned = params["arguments"].to<JsonObject>();
    for (JsonPairConst slot : arguments) {
        JsonVariantConst value = slot.value();
        if (value.isNull()) continue;
        if (value.is<const char*>() && !*value.as<const char*>()) continue;
        if (value.is<JsonArrayConst>() && !value.size()) continue;
        if (value.is<JsonObjectConst>() && !value.size()) continue;
        if (!strcmp(slot.key().c_str(), "temperature") && value.as<int>() == 0) continue;
        cleaned[slot.key()] = value;
    }

    JsonDocument response;
    String error;
    if (!request("tools/call", params, response, error)) return "Error: " + error;
    String text;
    for (JsonObjectConst part : response["result"]["content"].as<JsonArrayConst>()) text += part["text"] | "";
    if (response["result"]["isError"] | false) return "Error: " + text;
    return text;
}

}  // namespace ha
