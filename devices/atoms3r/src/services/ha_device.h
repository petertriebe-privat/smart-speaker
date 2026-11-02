#pragma once

#include <Arduino.h>

// This speaker as a Home Assistant device over MQTT discovery (#103, the port of the Echo's #65:
// core/internal/hamqtt). MQTT in the LAN is plain TCP (MQTT_URL, MQTT_USER,
// MQTT_PASSWORD in secrets.h): it never takes the one TLS connection. Entities: state, volume,
// microphone (a switch: the M5 mutes in software), presence, stop and talk buttons, an
// announcement (notify), events (wake word, button, conversation started/ended) and diagnostics
// (free internal RAM and PSRAM, WiFi, version, last error, start). The id is the WiFi MAC, so it
// stays across renames; topics jarvis/<id>/<key>, discovery homeassistant/<component>/<id>/<key>.
namespace ha_device {

void begin();             // after network::begin
void update(bool quiet);  // every loop; connects only when not quiet (a conversation), 30 s apart
// An event for HA's automations (main loop); score >= 0 goes along (the wake word's, 0..1).
void fire(const char* eventType, float score = -1);
void print();             // serial J: connected, id, what was published

}  // namespace ha_device
