#pragma once

#include <Arduino.h>

// WiFi (WIFI_SSID / WIFI_PASSWORD in secrets.h) and the clock via NTP.
namespace network {

void begin();
// Once connected: sets the clock; logs connection changes. `quiet` (no conversation): also
// checks that packets get through, and reconnects or restarts if not.
void update(bool quiet);
bool connected();
const char* ssid();
String link();  // signal, access point and channel, for logs
void printScan();  // every access point in range (blocks ~2-4 s)

}  // namespace network
