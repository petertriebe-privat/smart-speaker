#include "services/ha_device.h"

#include <ArduinoJson.h>
#include <PubSubClient.h>
#include <WiFi.h>
#include <esp_heap_caps.h>

#include <functional>
#include <vector>

#include "assistant/agent.h"
#include "assistant/tools.h"
#include "audio/volume.h"
#include "config.h"
#include "system/name.h"
#include "system/error_log.h"
#include "system/network.h"
#include "system/occupancy.h"
#include "system/time_util.h"
#include "ui/mute.h"
#include "voice/announcer.h"
#include "voice/live_session.h"

#ifndef MQTT_URL
#define MQTT_URL ""
#endif
#ifndef MQTT_USER
#define MQTT_USER ""
#endif
#ifndef MQTT_PASSWORD
#define MQTT_PASSWORD ""
#endif
#ifndef GIT_REVISION
#define GIT_REVISION "unknown"
#endif

namespace ha_device {
namespace {

constexpr uint32_t RETRY_MS = 30000;   // between connection attempts
constexpr uint32_t STATES_MS = 2000;   // states are compared this often, sent when changed
constexpr const char* PREFIX = "homeassistant";

struct Entity {
    const char* component;
    const char* key;
    std::function<void(JsonObject)> config;  // name, icon, device_class, ...
    std::function<String()> state;           // empty = none
    std::function<void(const String&)> command;  // empty = none
    String published;
    bool sent = false;
};

WiFiClient tcp;
PubSubClient mqtt(tcp);
std::vector<Entity> entities;
String id, base;
IPAddress host;
uint16_t port = 1883;
bool configured = false;
uint32_t lastTry = 0, lastStates = 0;
bool wasOpen = false;
time_t started = 0;
uint32_t connects = 0, commands = 0;
QueueHandle_t says = xQueueCreate(2, sizeof(String*));

String topic(const char* key) { return base + "/" + key; }

// The announcement in the tools worker (TLS speech takes ~2 s), as the leader's /speak.
void sayWork()
{
    String* text = nullptr;
    if (xQueueReceive(says, &text, 0) != pdTRUE) return;
    const String result = agent::announce(*text);
    Serial.printf("[HA device] say: %s\n", result.c_str());
    delete text;
}

// What HA's UI sends from an empty field: never a command (the Echo started a conversation with
// "unknown.", 27.10.2026).
bool placeholder(String text)
{
    text.trim();
    while (text.endsWith(".")) text.remove(text.length() - 1);
    text.toLowerCase();
    return text.isEmpty() || text == "unknown" || text == "unavailable" || text == "none";
}

String onOff(bool on) { return on ? "ON" : "OFF"; }

String stateText()
{
    if (live::open()) return live::onPhone() ? "phone" : "conversation";
    if (announcer::active()) return "announcement";
    return "idle";
}

void add(const char* component, const char* key, std::function<void(JsonObject)> config,
         std::function<String()> state = nullptr, std::function<void(const String&)> command = nullptr)
{
    Entity e;
    e.component = component;
    e.key = key;
    e.config = config;
    e.state = state;
    e.command = command;
    entities.push_back(e);
}

void declare()
{
    add("sensor", "state", [](JsonObject c) {
        c["name"] = "Status";
        c["icon"] = "mdi:account-voice";
        c["device_class"] = "enum";
        JsonArray o = c["options"].to<JsonArray>();
        for (const char* s : {"idle", "conversation", "phone", "announcement"}) o.add(s);
    }, stateText);
    add("number", "volume", [](JsonObject c) {
        c["name"] = "Lautstärke";
        c["icon"] = "mdi:volume-high";
        c["min"] = 0;
        c["max"] = 100;
        c["mode"] = "slider";
        c["unit_of_measurement"] = "%";
    }, [] { return String(volume::level()); }, [](const String& p) { volume::request(int(p.toFloat() + 0.5f), 0); });
    // The M5 mutes in software (ui/mute): HA may switch it, the display shows it.
    add("switch", "muted", [](JsonObject c) {
        c["name"] = "Mikrofon stumm";
        c["icon"] = "mdi:microphone-off";
    }, [] { return onOff(mute::on()); }, [](const String& p) { mute::set(p == "ON"); });
    if (occupancy::available())
        add("binary_sensor", "presence", [](JsonObject c) {
            c["name"] = "Anwesenheit";
            c["device_class"] = "occupancy";
        }, [] { return onOff(occupancy::someoneThere()); });
    add("button", "stop", [](JsonObject c) {
        c["name"] = "Stopp";
        c["icon"] = "mdi:stop-circle-outline";
        c["payload_press"] = "PRESS";
    }, nullptr, [](const String&) {
        if (live::open()) live::close();
        else if (announcer::active()) announcer::stop();
    });
    add("button", "talk", [](JsonObject c) {
        c["name"] = "Gespräch beginnen";
        c["icon"] = "mdi:account-voice";
        c["payload_press"] = "PRESS";
    }, nullptr, [](const String&) {
        if (!live::open() && !mute::on()) live::start();
    });
    add("notify", "say", [](JsonObject c) {
        c["name"] = "Ansage";
        c["icon"] = "mdi:bullhorn";
    }, nullptr, [](const String& text) {
        if (placeholder(text)) return;
        auto* copy = new String(text);
        if (xQueueSend(says, &copy, 0) != pdTRUE || !tools::post(sayWork)) {
            delete copy;
            Serial.println("[HA device] say: busy, dropped");
        }
    });
    add("event", "events", [](JsonObject c) {
        c["name"] = "Ereignis";
        c["icon"] = "mdi:gesture-tap-button";
        JsonArray t = c["event_types"].to<JsonArray>();
        for (const char* e : {"wake_word", "button", "conversation_started", "conversation_ended"}) t.add(e);
    });
    // Diagnostics: the M5's limits (#85: TLS needs a large internal block).
    auto diagnostic = [](JsonObject c, const char* name, const char* icon, const char* unit) {
        c["name"] = name;
        c["icon"] = icon;
        c["entity_category"] = "diagnostic";
        if (unit) {
            c["unit_of_measurement"] = unit;
            c["state_class"] = "measurement";
        }
    };
    add("sensor", "heap", [=](JsonObject c) { diagnostic(c, "RAM intern frei", "mdi:memory", "KB"); },
        [] { return String(heap_caps_get_free_size(MALLOC_CAP_INTERNAL) / 1024); });
    add("sensor", "heap_block", [=](JsonObject c) { diagnostic(c, "RAM größter Block", "mdi:memory", "KB"); },
        [] { return String(heap_caps_get_largest_free_block(MALLOC_CAP_INTERNAL) / 1024); });
    add("sensor", "psram", [=](JsonObject c) { diagnostic(c, "PSRAM frei", "mdi:memory", "KB"); },
        [] { return String(heap_caps_get_free_size(MALLOC_CAP_SPIRAM) / 1024); });
    add("sensor", "wifi", [=](JsonObject c) {
        diagnostic(c, "WLAN-Signal", "mdi:wifi", "dBm");
        c["device_class"] = "signal_strength";
    }, [] { return String(WiFi.RSSI()); });
    add("sensor", "version", [=](JsonObject c) { diagnostic(c, "Version", "mdi:source-commit", nullptr); },
        [] { return String(GIT_REVISION); });
    add("sensor", "last_error", [=](JsonObject c) { diagnostic(c, "Letzter Fehler", "mdi:alert-circle-outline", nullptr); },
        [] {
            String text = errorlog::last();
            if (text.isEmpty()) return String("–");
            if (text.length() > 250) text = text.substring(0, 250) + "…";
            return text;
        });
    add("sensor", "started", [=](JsonObject c) {
        diagnostic(c, "Gestartet", "mdi:restart", nullptr);
        c["device_class"] = "timestamp";
    }, [] {
        if (!started) return String("None");
        char text[32];
        strftime(text, sizeof(text), "%Y-%m-%dT%H:%M:%S+00:00", gmtime(&started));
        return String(text);
    });
}

void announce()
{
    const String availability = topic("status");
    for (Entity& e : entities) {
        JsonDocument doc;
        JsonObject c = doc.to<JsonObject>();
        c["unique_id"] = id + "_" + e.key;
        c["availability_topic"] = availability;
        c["has_entity_name"] = true;
        JsonObject device = c["device"].to<JsonObject>();
        device["identifiers"].to<JsonArray>().add(id);
        device["name"] = *DEVICE_NAME ? DEVICE_NAME : ASSISTANT_NAME " M5";
        device["model"] = "AtomS3R + Echo Pyramid";
        device["manufacturer"] = "smart-speaker";
        device["sw_version"] = GIT_REVISION;
        if (*DEVICE_ROOM) device["suggested_area"] = DEVICE_ROOM;
        c["origin"]["name"] = "smart-speaker";
        e.config(c);
        if (e.state || !strcmp(e.component, "event")) c["state_topic"] = topic(e.key);
        if (e.command) c["command_topic"] = topic(e.key) + "/set";
        String payload;
        serializeJson(doc, payload);
        const String where = String(PREFIX) + "/" + e.component + "/" + id + "/" + e.key + "/config";
        if (!mqtt.publish(where.c_str(), (const uint8_t*)payload.c_str(), payload.length(), true))
            Serial.printf("[HA device] discovery of %s failed (%u bytes)\n", e.key, unsigned(payload.length()));
        e.sent = false;
    }
    mqtt.publish(availability.c_str(), "online", true);
    Serial.printf("[HA device] %u entities announced\n", unsigned(entities.size()));
}

void publishStates(bool force)
{
    for (Entity& e : entities) {
        if (!e.state) continue;
        const String value = e.state();
        if (e.sent && value == e.published && !force) continue;
        if (mqtt.publish(topic(e.key).c_str(), value.c_str(), true)) {
            e.published = value;
            e.sent = true;
        }
    }
}

void received(char* where, uint8_t* payload, unsigned int length)
{
    String text;
    text.reserve(length);
    for (unsigned i = 0; i < length; i++) text += char(payload[i]);
    if (!strcmp(where, (String(PREFIX) + "/status").c_str())) {
        if (text == "online") {  // HA restarted: everything again
            announce();
            publishStates(true);
        }
        return;
    }
    for (Entity& e : entities) {
        if (e.command && topic(e.key) + "/set" == where) {
            commands++;
            Serial.printf("[HA device] %s: %.80s\n", e.key, text.c_str());
            e.command(text);
            e.sent = false;  // HA shows what it sent until the state answers
            return;
        }
    }
}

bool connect()
{
    const String client = id;
    const String will = topic("status");
    if (!mqtt.connect(client.c_str(), MQTT_USER, MQTT_PASSWORD, will.c_str(), 0, true, "offline")) {
        Serial.printf("[HA device] broker refused or not reachable (state %d)\n", mqtt.state());
        return false;
    }
    connects++;
    for (Entity& e : entities)
        if (e.command) mqtt.subscribe((topic(e.key) + "/set").c_str());
    mqtt.subscribe((String(PREFIX) + "/status").c_str());
    announce();
    publishStates(true);
    return true;
}

}  // namespace

void begin()
{
    String url = MQTT_URL;  // mqtt://host[:port], host as an address: no DNS (#41)
    if (!url.startsWith("mqtt://")) {
        Serial.println("[HA device] off (MQTT_URL missing)");
        return;
    }
    String hostPort = url.substring(7);
    const int colon = hostPort.indexOf(':');
    if (colon > 0) {
        port = hostPort.substring(colon + 1).toInt();
        hostPort = hostPort.substring(0, colon);
    }
    if (!host.fromString(hostPort)) {
        Serial.println("[HA device] off (MQTT_URL needs an IP address)");
        return;
    }
    String mac = WiFi.macAddress();
    mac.replace(":", "");
    mac.toLowerCase();
    id = name::slug() + "_" + mac;
    base = "assistant/" + id;
    declare();
    mqtt.setServer(host, port);
    mqtt.setBufferSize(1280);  // the largest discovery config is ~700 bytes
    mqtt.setKeepAlive(60);
    mqtt.setSocketTimeout(5);
    mqtt.setCallback(received);
    configured = true;
}

void update(bool quiet)
{
    if (!configured || !network::connected()) return;
    if (!started && timeutil::synced()) started = time(nullptr) - millis() / 1000;
    if (!mqtt.connected()) {
        if (quiet || (lastTry && millis() - lastTry < RETRY_MS)) return;
        lastTry = millis();
        tcp.setTimeout(3);  // seconds: the loop waits for the connect
        connect();
        return;
    }
    mqtt.loop();
    const bool open = live::open();
    if (open != wasOpen) {
        wasOpen = open;
        if (!open && live::unconfirmedWake()) fire("wake_word_silent");  // a likely false start, unnoticed
        fire(open ? "conversation_started" : "conversation_ended");
    }
    if (millis() - lastStates >= STATES_MS) {
        lastStates = millis();
        publishStates(false);
    }
}

void fire(const char* eventType, float score)
{
    if (!mqtt.connected()) return;
    JsonDocument doc;
    doc["event_type"] = eventType;
    if (score >= 0) doc["score"] = roundf(score * 100) / 100;
    String payload;
    serializeJson(doc, payload);
    if (mqtt.publish(topic("events").c_str(), payload.c_str(), false))
        Serial.printf("[HA device] event %s\n", eventType);
}

void print()
{
    Serial.printf("[HA device] %s, id %s, %u entities, connects %u, commands %u, version %s\n",
                  !configured ? "off" : mqtt.connected() ? "connected" : "not connected", id.c_str(),
                  unsigned(entities.size()), unsigned(connects), unsigned(commands), GIT_REVISION);
    for (const Entity& e : entities)
        if (e.state) Serial.printf("[HA device]   %s = %s\n", e.key, e.sent ? e.published.c_str() : "(not sent)");
}

}  // namespace ha_device
