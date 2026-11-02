#include "system/occupancy.h"

#include <ArduinoJson.h>
#include <Wire.h>

#include "assistant/tools.h"
#include "config.h"
#include "system/name.h"
#include "services/home_assistant.h"
#include "system/time_util.h"

namespace occupancy {
namespace {

// STHS34PF80 registers (ST's sths34pf80_reg.h).
constexpr uint8_t ADDRESS = 0x5A;
constexpr uint8_t WHO_AM_I = 0x0F, CTRL1 = 0x20, FUNC_STATUS = 0x25, TAMBIENT_L = 0x28, TPRESENCE_L = 0x3A;
constexpr uint8_t ID = 0xD3;
// 4 Hz with the default averaging (128). The chip's filters run at the data rate: 1 Hz with 1024
// samples was tried (04.10.) and let the empty room drift up to presence 775, flag on for good.
constexpr uint8_t BDU = 0x10, ODR_4HZ = 0x05;
constexpr uint8_t PRESENCE_FLAG = 0x04, MOTION_FLAG = 0x02;
constexpr uint32_t POLL_MS = 250;
constexpr uint32_t TEMPERATURE_MS = 10000;
constexpr uint32_t REPORT_RETRY_MS = 30000;

bool found = false;
bool debug = false;  // serial R: raw values every second
// occupied: The assistant's own smoothed view (held ROOM_EMPTY_AFTER_MS / ROOM_STILL_EMPTY_MS), for its
// decisions (an announcement waits until someone is there). detected: what the sensor sees now,
// cleaned only of its quirks (presence left behind by someone who walked out); this goes to HA at
// once, the holding time is HA's (an automation's "for:"), the owner's wish 25.10.2026.
volatile bool occupied = false;
volatile bool detected = false;
volatile time_t changedAt = 0, lastMotionAt = 0;  // wall clock
volatile float temperature = NAN;                  // the sensor's own, at the device
uint32_t lastSeen = 0;                             // millis() of the last sign counted
uint32_t lastMotionFlag = 0;                       // millis() of the last motion flag
bool presenceTrusted = false;                      // until someone walked out (or since the start)
bool reportPending = false;
uint32_t lastReport = 0, retryAt = 0;

bool read(uint8_t reg, uint8_t* out, uint8_t count)
{
    Wire1.beginTransmission(ADDRESS);
    Wire1.write(reg);
    if (Wire1.endTransmission(false) != 0 || Wire1.requestFrom(ADDRESS, count) != count) return false;
    for (uint8_t i = 0; i < count; ++i) out[i] = Wire1.read();
    return true;
}

bool write(uint8_t reg, uint8_t value)
{
    Wire1.beginTransmission(ADDRESS);
    Wire1.write(reg);
    Wire1.write(value);
    return Wire1.endTransmission() == 0;
}

void setOccupied(bool now)
{
    occupied = now;
    changedAt = time(nullptr);
    Serial.printf("\n[Room] %s\n", now ? "someone came in" : "empty");
}

// State over REST: no integration needed, but HA forgets it on a restart, so it is sent again
// every ROOM_REPORT_MS.
void report()
{
    JsonDocument body;
    body["state"] = detected ? "on" : "off";
    JsonObject attributes = body["attributes"].to<JsonObject>();
    attributes["friendly_name"] = ASSISTANT_NAME " Anwesenheit";
    attributes["device_class"] = "occupancy";
    attributes[name::slug() + "_occupied"] = bool(occupied);  // The assistant's own smoothed view, for information
    if (!isnan(temperature)) attributes["sensor_temperature"] = roundf(temperature * 10) / 10;
    String payload;
    serializeJson(body, payload);
    JsonDocument response;
    const int status = ha::rest("POST", String("/api/states/") + entity(), payload, response);
    lastReport = millis();
    retryAt = lastReport + REPORT_RETRY_MS;
    if (status == 200 || status == 201) reportPending = false;
    else Serial.printf("[Room] report to Home Assistant: %s\n", ha::failure(status, response).c_str());
}

String clock(time_t t) { return t ? timeutil::format(t, "%d.%m. %H:%M") : String("unknown"); }

String describe()
{
    if (!found) return "Error: no presence sensor connected.";
    String text = occupied ? "Someone is in the room at the speaker (since " + clock(changedAt) + ")."
                           : "Nobody is in the room at the speaker (empty since " + clock(changedAt) + ").";
    text += " Last movement: " + clock(lastMotionAt) + ".";
    if (!isnan(temperature))
        text += " Sensor temperature " + String(temperature, 1) + " °C (inside the device, warmer than the room).";
    text += String(" Home Assistant entity for automations: ") + entity() +
            " (on = the sensor sees someone now, off = it sees nobody now; it changes at once, so an automation "
            "that should wait gives its trigger a 'for:', e.g. off for 10 minutes before the light goes out).";
    return text;
}

}  // namespace

void begin()
{
    uint8_t id = 0;
    found = read(WHO_AM_I, &id, 1) && id == ID && write(CTRL1, BDU | ODR_4HZ);
    Serial.printf("[Room] presence sensor %s\n", found ? "ready" : "not found");
    if (found) changedAt = time(nullptr);
}

bool available() { return found; }

bool someoneThere() { return found && occupied; }

void toggleDebug()
{
    debug = !debug;
    Serial.printf("[Room] raw values %s\n", debug ? "on" : "off");
}

void update(bool reportAllowed)
{
    if (!found) return;
    static uint32_t lastPoll = 0, lastTemperature = 0;
    const uint32_t now = millis();
    if (now - lastPoll >= POLL_MS) {
        lastPoll = now;
        uint8_t flags = 0, raw[4];
        const bool ok = read(FUNC_STATUS, &flags, 1);
        const bool values = read(TPRESENCE_L, raw, 4);
        const int presence = int16_t(raw[0] | raw[1] << 8), motion = int16_t(raw[2] | raw[3] << 8);
        static uint32_t lastDebug = 0;
        if (debug && values && now - lastDebug >= 1000) {
            lastDebug = now;
            Serial.printf("[Room] flags %d%d presence %6d motion %6d%s\n", (flags & PRESENCE_FLAG) != 0,
                          (flags & MOTION_FLAG) != 0, presence, motion, presenceTrusted ? "" : " (presence ignored)");
        }
        if (now < ROOM_WARMUP_MS) return;  // settling after the start (millis since boot)
        const bool moved = ok && (flags & MOTION_FLAG);
        if (moved) {
            lastMotionFlag = now;
            lastMotionAt = time(nullptr);
        }
        // A warm body leaving the view drives motion far negative, one coming in positive.
        if (values && motion <= -ROOM_LEAVE_MOTION) presenceTrusted = false;
        else if (moved && motion >= ROOM_COME_IN_MOTION) presenceTrusted = true;
        const bool present = ok && (flags & PRESENCE_FLAG) && presenceTrusted &&
                             now - lastMotionFlag < ROOM_PRESENCE_ONLY_MS;
        if (moved || present) {
            lastSeen = now;
            if (!occupied) setOccupied(true);
        } else if (occupied && now - lastSeen > (presenceTrusted ? ROOM_STILL_EMPTY_MS : ROOM_EMPTY_AFTER_MS)) {
            setOccupied(false);  // silence alone is someone sitting still as often as an empty room
        }
        if ((moved || present) != detected) {  // HA gets each change (at most every ROOM_REPORT_MIN_MS)
            detected = moved || present;
            reportPending = true;
        }
    }
    if (now - lastTemperature >= TEMPERATURE_MS) {
        lastTemperature = now;
        uint8_t raw[2];
        if (read(TAMBIENT_L, raw, 2)) temperature = int16_t(raw[0] | raw[1] << 8) / 100.0f;
    }
    if (!reportAllowed || !ha::configured() || !ha::reachable()) return;  // HA away: blocks the loop
    // A change at once, but not more often than ROOM_REPORT_MIN_MS (the motion flag pulses; each
    // report blocks the loop for its request): the last state always follows. A failed report again
    // after REPORT_RETRY_MS, the state again now and then.
    const bool due = reportPending && int32_t(now - retryAt) >= 0 && now - lastReport >= ROOM_REPORT_MIN_MS;
    if (due || !lastReport || now - lastReport > ROOM_REPORT_MS) report();
}

void addTools()
{
    tools::add(tools::Tool{
        "room_sensor",
        tools::keep(String("The presence sensor at the speaker (infrared: also notices people sitting still): whether someone is "
        "in the room right now, since when, and the last movement. Automations that react to it ('Licht an, "
        "wenn jemand reinkommt und es dunkel ist', 'begrüße mich, wenn ich reinkomme') are "
        "create_home_automation with a state trigger on ") + entity() + "; it changes as soon as the "
        "sensor sees someone or nobody, so 'when nobody is there' needs 'for:' in the trigger (e.g. 10 minutes)."),
        R"json({"type": "object", "additionalProperties": false, "properties": {}, "required": []})json",
        [](JsonObjectConst, const tools::Context&) { return describe(); }, 0, tools::Everywhere});
}

}  // namespace occupancy

String occupancy::entity() { return "binary_sensor." + name::slug() + "_presence"; }
