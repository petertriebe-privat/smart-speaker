#include "services/ha_automations.h"

#include <HTTPClient.h>
#include <time.h>
#include <vector>

#include "assistant/tools.h"
#include "config.h"
#include "system/name.h"
#include "services/home_assistant.h"

namespace automations {
namespace {

// The ids of the automations the assistant made: the slug of its name and _ (jarvis_ by default).
String idPrefix() { return name::slug() + "_"; }
constexpr size_t ENTITY_HITS = 40;

// All automations the assistant created: entity id, alias, config id, state, last run.
std::vector<JsonDocument> ownAutomations(String& error)
{
    std::vector<JsonDocument> found;
    JsonDocument states;
    const int status = ha::states("automation", "", 500, states);  // not /api/states (#85)
    if (status != 200) {
        error = ha::failure(status, states);
        return found;
    }
    for (JsonObjectConst entity : states.as<JsonArrayConst>()) {
        const String id = entity["attributes"]["id"] | "";
        if (!String(entity["entity_id"] | "").startsWith("automation.") || !id.startsWith(idPrefix())) continue;
        found.emplace_back();
        found.back().set(entity);
    }
    return found;
}

String create(JsonObjectConst arguments)
{
    const String alias = arguments["alias"] | "";
    JsonDocument config;
    if (alias.isEmpty()) return "Error: give an alias";
    if (deserializeJson(config, arguments["config"] | "") || !config.is<JsonObject>())
        return "Error: config is not a JSON object";
    if (!config["triggers"].is<JsonArray>() || !config["actions"].is<JsonArray>())
        return "Error: config needs triggers and actions (lists)";
    const String id = String(idPrefix()) + String((unsigned long)time(nullptr));
    config["id"] = id;
    config["alias"] = ASSISTANT_NAME ": " + alias;
    config["description"] = String(arguments["description"] | "") + " (created by the assistant)";
    if (!config["mode"].is<const char*>()) config["mode"] = "single";
    String payload;
    serializeJson(config, payload);
    JsonDocument response;
    const int status = ha::rest("POST", "/api/config/automation/config/" + id, payload, response);
    Serial.printf("[Automations] %s: HTTP %d %s\n", id.c_str(), status, payload.c_str());
    if (status != 200) return ha::failure(status, response) + ". Check the config and try again.";
    return "Created: \"" ASSISTANT_NAME ": " + alias + "\" (" + id + "). Home Assistant runs it by itself from now on.";
}

String list()
{
    String error;
    const std::vector<JsonDocument> found = ownAutomations(error);
    if (error.length()) return error;
    if (found.empty()) return "No automations by the assistant in Home Assistant.";
    String text;
    for (const JsonDocument& entity : found) {
        text += "- " + String(entity["attributes"]["friendly_name"] | "") + " [" +
                (entity["attributes"]["id"] | "") + "], " + (strcmp(entity["state"] | "", "on") ? "aus" : "aktiv");
        const String last = entity["attributes"]["last_triggered"] | "";
        text += last.length() ? ", last " + last.substring(0, 16) + "\n" : ", never triggered\n";
    }
    return text;
}

String remove(const String& id)
{
    if (!id.startsWith(idPrefix())) return "Error: " ASSISTANT_NAME " deletes only its own automations (" + idPrefix() + "...)";
    JsonDocument response;
    const int status = ha::rest("DELETE", "/api/config/automation/config/" + id, "", response);
    if (status != 200) return ha::failure(status, response);
    return "Automation " + id + " deleted.";
}

// entity_ids for automations: GetLiveContext gives names, HA's config needs ids.
String findEntities(const String& query, const String& domain)
{
    JsonDocument states;
    // HA filters by domain and query (not /api/states, #85); one more than shown tells of more hits
    const int status = ha::states(domain, query, ENTITY_HITS + 1, states);
    if (status != 200) return ha::failure(status, states);
    String wanted = query;
    wanted.toLowerCase();
    String text;
    size_t hits = 0;
    for (JsonObjectConst entity : states.as<JsonArrayConst>()) {
        const String id = entity["entity_id"] | "";
        const String name = entity["attributes"]["friendly_name"] | "";
        if (domain.length() && !id.startsWith(domain + ".")) continue;
        String haystack = id + " " + name;
        haystack.toLowerCase();
        if (wanted.length() && haystack.indexOf(wanted) < 0) continue;
        if (++hits > ENTITY_HITS) {
            text += "… (mehr Treffer, genauer suchen)\n";
            break;
        }
        text += id + " = " + name + " (" + (entity["state"] | "") + ")\n";
    }
    return text.length() ? text : "No matching entities.";
}

}  // namespace

void addTools()
{
    if (!ha::configured()) return;
    using tools::Tool;
    tools::add(Tool{
        "create_home_automation",
        tools::keep(String("Creates a permanent automation in Home Assistant, after the user confirmed it: recurring device "
        "control at a time, at sunrise/sunset or when a sensor or device changes. HA runs it locally from "
        "then on; a time trigger fires every day. Never for one-off or delayed actions ('in a minute', "
        "'tomorrow once'): use create_task. config is the automation as JSON in Home Assistant's format "
        "with triggers, optional conditions, actions and mode, e.g. "
        "{\"triggers\":[{\"trigger\":\"time\",\"at\":\"19:00:00\"}],\"conditions\":[],"
        "\"actions\":[{\"action\":\"light.turn_on\",\"target\":{\"entity_id\":\"light.x\"}}],"
        "\"mode\":\"single\"}. Sunset: {\"trigger\":\"sun\",\"event\":\"sunset\",\"offset\":\"-00:15:00\"}; "
        "state: {\"trigger\":\"state\",\"entity_id\":\"...\",\"to\":\"off\",\"for\":\"00:05:00\"}; weekdays: "
        "condition {\"condition\":\"time\",\"weekday\":[\"mon\",\"tue\"]}. Get entity_ids with find_entities "
        "first. To make " ASSISTANT_NAME " speak: action {\"event\":\"") + name::slug() + "_announce\",\"event_data\":{\"text\":\"...\"}}; "
        "to make it think and act (search, mail, summarize): {\"event\":\"" + name::slug() + "_task\","
        "\"event_data\":{\"title\":\"...\",\"instruction\":\"...\",\"deliver\":\"speak\"}} "
        "(deliver: speak, email, both, telegram, none); to make it talk WITH whoever is there (greet them "
        "and ask, wait for the answer): {\"event\":\"" + name::slug() + "_conversation\",\"event_data\":{\"text\":\"...\"}} "
        "(text: the occasion and what to say or ask, in German; optional person: a household name; "
        "insist: true keeps at it until someone answers)."),
        R"json({"type": "object", "additionalProperties": false,
            "properties": {
              "alias": {"type": "string", "description": "short German name"},
              "description": {"type": "string", "description": "what it does, in German"},
              "config": {"type": "string", "description": "the automation as a JSON object (string)"}},
            "required": ["alias", "description", "config"]})json",
        [](JsonObjectConst arguments, const tools::Context&) { return create(arguments); }, 0, tools::Everywhere});
    tools::add(Tool{
        "list_home_automations", "Lists the automations the assistant created in Home Assistant (name, id, active, last run).",
        R"json({"type": "object", "additionalProperties": false, "properties": {}, "required": []})json",
        [](JsonObjectConst, const tools::Context&) { return list(); }, 0, tools::Everywhere});
    tools::add(Tool{
        "delete_home_automation", tools::keep("Deletes an automation the assistant created (id " + idPrefix() + "...)."),
        R"json({"type": "object", "additionalProperties": false, "properties": {"id": {"type": "string"}}, "required": ["id"]})json",
        [](JsonObjectConst arguments, const tools::Context&) { return remove(arguments["id"] | ""); }, 0,
        tools::Everywhere});
    tools::add(Tool{
        "find_entities",
        "Finds Home Assistant entity_ids (light.*, switch.*, sensor.*, binary_sensor.*, ...) by name, for "
        "create_home_automation.",
        R"json({"type": "object", "additionalProperties": false,
            "properties": {
              "query": {"type": "string", "description": "part of the name or id, e.g. 'wohnzimmer'; '' for all of a domain"},
              "domain": {"type": "string", "description": "optional: light, switch, sensor, binary_sensor, ...; '' for all"}},
            "required": ["query"]})json",
        [](JsonObjectConst arguments, const tools::Context&) {
            return findEntities(arguments["query"] | "", arguments["domain"] | "");
        },
        0, tools::Everywhere});
}

}  // namespace automations
