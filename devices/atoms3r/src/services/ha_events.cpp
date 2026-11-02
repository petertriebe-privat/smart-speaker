#include "services/ha_events.h"

#include <ArduinoJson.h>
#include <WebSocketsClient.h>
#include <errno.h>

#include "assistant/people.h"
#include "assistant/tasks.h"
#include "config.h"
#include "system/name.h"
#include "services/home_assistant.h"
#include "system/network.h"
#include "voice/live_session.h"

#if __has_include("secrets.h")
#include "secrets.h"
#endif

namespace ha_events {
namespace {

WebSocketsClient ws;
bool started = false, subscribed = false;

void send(const JsonDocument& doc)
{
    String text;
    serializeJson(doc, text);
    ws.sendTXT(text);
}

// The event types after the slug of the assistant's name (jarvis_announce by default).
constexpr const char* TYPES[] = {"_announce", "_task", "_conversation"};
constexpr int TYPE_COUNT = sizeof(TYPES) / sizeof(TYPES[0]);

void subscribe()
{
    int id = 1;
    for (const char* type : TYPES) {
        JsonDocument doc;
        doc["id"] = id++;
        doc["type"] = "subscribe_events";
        doc["event_type"] = name::slug() + type;
        send(doc);
    }
}

void onEvent(JsonObjectConst event)
{
    const String full = event["event_type"] | "";
    const String type = full.startsWith(name::slug()) ? full.substring(name::slug().length()) : full;
    JsonObjectConst data = event["data"];
    Serial.printf("\n[HA event] %s %s\n", full.c_str(), event["data"].as<String>().c_str());
    if (type == "_announce") {
        tasks::runNow("reminder", "Home Assistant", data["text"] | "", "speak");
    } else if (type == "_task") {
        tasks::runNow("prompt", data["title"] | "Home Assistant", data["instruction"] | "",
                      data["deliver"] | "speak");
    } else if (type == "_conversation") {
        // Whoever is in the room unless the automation names a person of the household.
        const String person = people::byName(data["person"] | "");
        const String result = live::requestConversation(data["text"] | "", person, data["insist"] | false);
        Serial.printf("[HA event] %s\n", result.c_str());
    }
}

void onMessage(const char* payload, size_t length)
{
    JsonDocument doc;
    if (deserializeJson(doc, payload, length)) return;
    const char* type = doc["type"] | "";
    if (!strcmp(type, "auth_required")) {
#if defined(HA_TOKEN)
        JsonDocument auth;
        auth["type"] = "auth";
        auth["access_token"] = HA_TOKEN;
        send(auth);
#endif
    } else if (!strcmp(type, "auth_ok")) {
        subscribe();
    } else if (!strcmp(type, "auth_invalid")) {
        Serial.printf("[HA events] login refused: %s\n", (const char*)(doc["message"] | ""));
    } else if (!strcmp(type, "result")) {
        if (!(doc["success"] | false))
            Serial.printf("[HA events] subscription refused: %s\n", (const char*)(doc["error"]["message"] | ""));
        else if (!subscribed && (doc["id"] | 0) == TYPE_COUNT) {
            subscribed = true;
            Serial.printf("[HA events] connected (%s_announce, _task, _conversation)\n", name::slug().c_str());
        }
    } else if (!strcmp(type, "event")) {
        onEvent(doc["event"]);
    }
}

void onWebSocket(WStype_t type, uint8_t* payload, size_t length)
{
    if (type == WStype_TEXT) onMessage(reinterpret_cast<const char*>(payload), length);
    else if (type == WStype_DISCONNECTED && subscribed) {
        subscribed = false;
        Serial.println("[HA events] disconnected, reconnecting");
    }
}

}  // namespace

void begin()
{
#if defined(HA_URL)
    // "http://192.168.1.10:8123" -> host and port (plain WebSocket in the LAN)
    String url = HA_URL;
    if (!url.startsWith("http://")) {
        Serial.println("[HA events] only http:// in the LAN is supported");
        return;
    }
    url.remove(0, 7);
    const int slash = url.indexOf('/');
    if (slash >= 0) url.remove(slash);
    const int colon = url.indexOf(':');
    const String host = colon >= 0 ? url.substring(0, colon) : url;
    const uint16_t port = colon >= 0 ? url.substring(colon + 1).toInt() : 80;
    ws.onEvent(onWebSocket);
    ws.setReconnectInterval(HA_EVENTS_RETRY_MS);
    ws.enableHeartbeat(30000, 5000, 2);  // a dead link is noticed and reconnected
    ws.begin(host, port, "/api/websocket");
    started = true;
#endif
}

void update()
{
    // WiFiClient::connected() peeks with recv(len 0): with a frame waiting that returns 0 without
    // setting errno, and a stale ENOTCONN from another socket (the presence report to HA, closed by
    // the server) then counts as a lost link - the event that report triggered was lost with it.
    errno = 0;
    if (started && network::connected()) ws.loop();
}

}  // namespace ha_events
