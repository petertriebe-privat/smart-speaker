#include "services/alexa.h"

#include <ArduinoJson.h>
#include <vector>

#include "assistant/tools.h"
#include "services/home_assistant.h"

namespace alexa {
namespace {

// The available Echos (media players of alexa_devices) with room, device id and their notify
// entities for speaking and announcing; the entity ids end in the German entity names.
constexpr const char* ECHOS_TEMPLATE =
    "{% set ns = namespace(l=[]) %}"
    "{% for e in integration_entities('alexa_devices') | select('match', 'media_player[.]')"
    " | reject('is_state', 'unavailable') %}"
    "{% set d = device_id(e) %}"
    "{% set n = device_entities(d) | select('match', 'notify[.]') | list %}"
    "{% set ns.l = ns.l + [{'name': state_attr(e, 'friendly_name'), 'area': area_name(e) or '', 'device': d,"
    " 'speak': n | select('search', '_sprechen$') | first | default(''),"
    " 'announce': n | select('search', '_durchsagen$') | first | default('')}] %}"
    "{% endfor %}{{ {'echos': ns.l} | tojson }}";

struct Echo {
    String name, area, device, speak, announce;
};

String lower(String text)
{
    text.trim();
    text.toLowerCase();  // ASCII only; umlauts compare byte by byte
    return text;
}

String names(const std::vector<Echo>& echos)
{
    String text;
    for (const Echo& echo : echos)
        text += (text.length() ? ", " : "") + echo.name + (echo.area.length() ? " (" + echo.area + ")" : "");
    return text;
}

bool loadEchos(std::vector<Echo>& echos, String& error)
{
    JsonDocument request, response;
    request["template"] = ECHOS_TEMPLATE;
    String payload;
    serializeJson(request, payload);
    const int status = ha::rest("POST", "/api/template", payload, response);
    if (status != 200) {
        error = ha::failure(status, response);
        return false;
    }
    for (JsonObjectConst item : response["echos"].as<JsonArrayConst>())
        echos.push_back({item["name"] | "", item["area"] | "", item["device"] | "", item["speak"] | "",
                         item["announce"] | ""});
    if (echos.empty()) {
        error = "Error: Home Assistant reports no reachable Echos (integration Alexa Devices).";
        return false;
    }
    return true;
}

// The Echo meant by `wanted`: its exact name, else the only one in that room or with the name
// part (an "Echo ..." before speaker groups and Fire TVs). Nullptr with `error` otherwise.
const Echo* find(const std::vector<Echo>& echos, const String& wanted, String& error)
{
    const String key = lower(wanted);
    for (const Echo& echo : echos)
        if (lower(echo.name) == key) return &echo;
    std::vector<const Echo*> hits, echoHits;
    for (const Echo& echo : echos) {
        if (key.isEmpty() || (lower(echo.area) != key && lower(echo.name).indexOf(key) < 0)) continue;
        hits.push_back(&echo);
        if (lower(echo.name).startsWith("echo")) echoHits.push_back(&echo);
    }
    if (hits.size() == 1) return hits[0];
    if (echoHits.size() == 1) return echoHits[0];
    if (key.isEmpty()) error = "Error: device missing. Reachable devices: " + names(echos);
    else if (hits.empty()) error = "Error: no device \"" + wanted + "\". Reachable devices: " + names(echos);
    else {
        std::vector<Echo> several;
        for (const Echo* echo : hits) several.push_back(*echo);
        error = "Error: several devices match \"" + wanted + "\": " + names(several) + ". Name one.";
    }
    return nullptr;
}

String call(const char* service, JsonDocument& data)
{
    String payload;
    serializeJson(data, payload);
    JsonDocument response;
    const int status = ha::rest("POST", String("/api/services/") + service, payload, response);
    return status == 200 ? "" : ha::failure(status, response);
}

String run(JsonObjectConst arguments, const tools::Context&)
{
    const String action = arguments["action"] | "", text = arguments["text"] | "";
    if (text.isEmpty()) return "Error: text missing";
    std::vector<Echo> echos;
    String error;
    if (!loadEchos(echos, error)) return error;
    const Echo* echo = find(echos, arguments["device"] | "", error);
    if (!echo) return error;

    JsonDocument data;
    if (action == "command") {
        data["device_id"] = echo->device;
        data["text_command"] = text;
        error = call("alexa_devices/send_text_command", data);
        return error.length() ? error : "An " + echo->name + " geschickt: " + text;
    }
    if (action != "speak" && action != "announce") return "Error: action is command, speak or announce";
    const String& entity = action == "speak" ? echo->speak : echo->announce;
    if (entity.isEmpty())
        return "Error: " + echo->name + " cannot do that" + (action == "speak" ? " (announce only)" : "");
    data["entity_id"] = entity;
    data["message"] = text;
    error = call("notify/send_message", data);
    return error.length() ? error : echo->name + (action == "speak" ? " says: " : " announces: ") + text;
}

}  // namespace

void addTools()
{
    if (!ha::configured()) return;
    tools::add(tools::Tool{
        "alexa",
        "Controls the Amazon Echo speakers (Alexa) at home through Home Assistant. action command sends text to "
        "the Echo as if the user had said it to Alexa: radio ('spiel Radio Bob'), Spotify's own mixes (Mix der "
        "Woche, Daily Mix), timers or alarms on the Echo. Not for other music, podcasts or controlling what "
        "plays (the Spotify tools), not for lights or other smart home devices (Home Assistant tools), not for "
        "the volume (Home Assistant) and not for Fire TVs (fire_tv). action speak makes the "
        "Echo say text, announce plays it as an announcement with a chime. device: the Echo's name or its room; "
        "an unclear device returns the list.",
        R"json({"type": "object", "additionalProperties": false,
            "properties": {"device": {"type": "string", "description": "e.g. 'Echo Wohnzimmer' or 'Wohnzimmer'"},
                           "action": {"type": "string", "enum": ["command", "speak", "announce"]},
                           "text": {"type": "string", "description": "German: the Alexa command without 'Alexa', or the sentence to say"}},
            "required": ["device", "action", "text"]})json",
        run, tools::Slow, tools::Everywhere});
}

}  // namespace alexa
