#include "system/network.h"

#include <WiFi.h>
#include <WiFiClient.h>
#include <time.h>

#include "config.h"
#include "system/error_log.h"

#if __has_include("secrets.h")
#include "secrets.h"
#else
#error "Copy include/secrets.h.example to include/secrets.h and fill in WiFi and API key"
#endif

namespace network {
namespace {

constexpr uint32_t REPORT_MS = 10000;  // repeat "no connection" in the log

// Why the access point dropped us (wifi_err_reason_t, e.g. 8 = we left, 200 = beacon timeout,
// 201 = no AP found). Set by the WiFi event task, logged by update().
volatile uint8_t lastDropReason = 0;

void reconnect()
{
    WiFi.disconnect();
    WiFi.begin(WIFI_SSID, WIFI_PASSWORD);
}

// Once the WiFi fell out after an upload and stayed in status 0 (idle) for good: the
// automatic reconnect never tried again.
void reconnectIfStuck(bool online)
{
    static uint32_t offlineSince = 0, lastAttempt = 0;
    if (online) {
        offlineSince = 0;
        return;
    }
    const uint32_t now = millis();
    if (!offlineSince) offlineSince = lastAttempt = now;
    if (now - offlineSince > WIFI_RESTART_MS) {
        errorlog::add("WiFi does not come back: restart");
        delay(100);
        ESP.restart();
    }
    if (now - lastAttempt > WIFI_RETRY_MS) {
        lastAttempt = now;
        Serial.println("WiFi: new connection attempt");
        reconnect();
    }
}

// Can packets get through? A TCP connection to the router's web interface (port 80), else a DNS
// lookup (answered from the cache for a while, so only the fallback).
bool reachable()
{
    WiFiClient probe;
    const bool router = probe.connect(WiFi.gatewayIP(), 80, NETWORK_PROBE_TIMEOUT_MS);
    probe.stop();
    IPAddress ip;
    return router || WiFi.hostByName(LIVE_HOST, ip) == 1;
}

// On 27.09. the WiFi status stayed "connected" while nothing got through: DNS failed, every
// connection aborted with errno 113, the device did not answer pings, and reconnectIfStuck
// never fired. Between conversations the link is probed every NETWORK_PROBE_MS; after two
// failures the WiFi reconnects, after WIFI_RESTART_MS without success the device restarts.
void probeLink(bool quiet)
{
    static uint32_t lastProbe = 0, deadSince = 0;
    static int failures = 0;
    const uint32_t now = millis();
    if (!quiet || now - lastProbe < NETWORK_PROBE_MS) return;
    lastProbe = now;
    if (reachable()) {
        if (failures) Serial.println("WiFi: network reachable again");
        failures = 0;
        deadSince = 0;
        return;
    }
    if (!deadSince) deadSince = now;
    errorlog::add("WiFi connected, but router/DNS not reachable (%d. time)", ++failures);
    if (now - deadSince > WIFI_RESTART_MS) {
        errorlog::add("WiFi connected, but router/DNS do not come back: restart");
        delay(100);
        ESP.restart();
    }
    if (failures % 2 == 0) {
        Serial.println("WiFi: new connection attempt");
        reconnect();
    }
}

// 01.10.: in another room the device sat at -84 dBm on the repeater it uses at its usual spot
// while the main router, same SSID, was nearby; GPT Live broke off within seconds ("Connection
// lost"). The ESP32 never changes the access point on its own. While idle with a weak signal,
// a background scan looks for a clearly stronger one with our SSID and switches to it.
void roamIfWeak(bool quiet)
{
    static uint32_t lastScan = 0;
    const int16_t found = WiFi.scanComplete();
    if (found == WIFI_SCAN_RUNNING) return;
    if (found >= 0) {
        int best = -1;
        for (int i = 0; i < found; i++)
            if (WiFi.SSID(i) == WIFI_SSID && (best < 0 || WiFi.RSSI(i) > WiFi.RSSI(best))) best = i;
        if (quiet && best >= 0 && WiFi.BSSIDstr(best) != WiFi.BSSIDstr() &&
            WiFi.RSSI(best) >= WiFi.RSSI() + ROAM_GAIN_DB) {
            errorlog::add("WiFi: %s; AP %s has %d dBm: switching", link().c_str(), WiFi.BSSIDstr(best).c_str(),
                          int(WiFi.RSSI(best)));
            uint8_t bssid[6];
            memcpy(bssid, WiFi.BSSID(best), sizeof bssid);
            const int32_t channel = WiFi.channel(best);
            WiFi.scanDelete();
            WiFi.begin(WIFI_SSID, WIFI_PASSWORD, channel, bssid);
            return;
        }
        WiFi.scanDelete();
    }
    if (!quiet || WiFi.RSSI() > ROAM_BELOW_DBM || (lastScan && millis() - lastScan < ROAM_SCAN_MS)) return;
    lastScan = millis();
    WiFi.scanNetworks(true);  // in the background; the result is picked up in a later pass
}

}  // namespace

void begin()
{
    WiFi.mode(WIFI_STA);
    WiFi.setSleep(false);  // power save adds latency and drops throughput
    WiFi.setAutoReconnect(true);
    // The default fast scan joins the first access point with the SSID it finds, not the
    // strongest: away from the usual spot, GPT Live then broke off within seconds ("Connection
    // lost") while the WiFi stayed up. A full scan (~1-2 s longer) picks by signal.
    WiFi.setScanMethod(WIFI_ALL_CHANNEL_SCAN);
    WiFi.setSortMethod(WIFI_CONNECT_AP_BY_SIGNAL);
    WiFi.onEvent([](arduino_event_id_t, arduino_event_info_t info) {
        lastDropReason = info.wifi_sta_disconnected.reason;
    }, ARDUINO_EVENT_WIFI_STA_DISCONNECTED);
    WiFi.begin(WIFI_SSID, WIFI_PASSWORD);
}

String link()
{
    if (!connected()) return "no WiFi";
    return String(WiFi.RSSI()) + " dBm, AP " + WiFi.BSSIDstr() + ", channel " + String(WiFi.channel());
}

void printScan()
{
    const int count = WiFi.scanNetworks();
    Serial.printf("[WiFi scan] %d access points, joined: %s\n", count, link().c_str());
    for (int i = 0; i < count; i++) {
        Serial.printf("[WiFi scan] %4d dBm  ch %2d  auth %d  %s  %s%s\n", WiFi.RSSI(i), WiFi.channel(i),
                      int(WiFi.encryptionType(i)), WiFi.BSSIDstr(i).c_str(), WiFi.SSID(i).c_str(),
                      WiFi.SSID(i) == WIFI_SSID ? "  <- ours" : "");
    }
    WiFi.scanDelete();
}

void update(bool quiet)
{
    static bool wasConnected = false, clockSet = false;
    static uint32_t lastReport = 0;
    const bool online = connected();
    if (online && !clockSet) {
        configTzTime(TIMEZONE, "pool.ntp.org", "time.google.com");
        clockSet = true;
    }
    if (online && !wasConnected) {
        Serial.printf("WiFi connected: %s\n", WiFi.localIP().toString().c_str());
        errorlog::add("WiFi joined: %s", link().c_str());
    } else if (!online && wasConnected) {
        errorlog::add("WiFi lost (reason %u)", unsigned(lastDropReason));
        lastReport = millis();
    } else if (!online && millis() - lastReport > REPORT_MS) {
        // wl_status_t: 1 = SSID not found, 4 = connect failed (password), 6 = disconnected
        Serial.printf("WiFi: no connection (status %d)\n", int(WiFi.status()));
        lastReport = millis();
    }
    wasConnected = online;
    reconnectIfStuck(online);
    if (online) probeLink(quiet);
    if (online) roamIfWeak(quiet);
}

bool connected() { return WiFi.status() == WL_CONNECTED; }

const char* ssid() { return WIFI_SSID; }

}  // namespace network
