#include "system/error_log.h"

#include <LittleFS.h>
#include <esp_system.h>
#include <stdarg.h>

#include "config.h"
#include "system/time_util.h"

namespace errorlog {
namespace {

constexpr const char* PATH = "/errors.log";
SemaphoreHandle_t lock = xSemaphoreCreateMutex();
bool ready = false;
String newest;  // under the lock

// Keeps the newer half when the file grew beyond ERROR_LOG_BYTES (caller holds the lock).
void trim()
{
    File file = LittleFS.open(PATH, "r");
    if (!file || file.size() <= ERROR_LOG_BYTES) return;
    file.seek(file.size() - ERROR_LOG_BYTES / 2);
    file.readStringUntil('\n');  // rest of a cut line
    const String rest = file.readString();
    file.close();
    File out = LittleFS.open(PATH, "w");
    if (out) out.print(rest);
}

const char* resetReason()
{
    switch (esp_reset_reason()) {
        case ESP_RST_POWERON: return "power on";
        case ESP_RST_SW: return "restart by the firmware";
        case ESP_RST_PANIC: return "CRASH (exception/panic)";
        case ESP_RST_INT_WDT: return "CRASH (interrupt watchdog)";
        case ESP_RST_TASK_WDT: return "CRASH (task watchdog)";
        case ESP_RST_WDT: return "CRASH (watchdog)";
        case ESP_RST_BROWNOUT: return "brownout";
        case ESP_RST_DEEPSLEEP: return "wake from deep sleep";
        default: return "restart (USB/upload or unknown)";
    }
}

}  // namespace

void begin()
{
    ready = true;
    add("Start: %s", resetReason());
}

void add(const char* format, ...)
{
    char text[256];
    va_list args;
    va_start(args, format);
    vsnprintf(text, sizeof(text), format, args);
    va_end(args);
    Serial.printf("[ErrorLog] %s\n", text);
    if (!ready) return;
    // Before NTP the clock says 1970: uptime instead.
    const String when = timeutil::synced() ? timeutil::format(time(nullptr), "%Y-%m-%d %H:%M:%S")
                                           : "+" + String(millis() / 1000) + " s";
    xSemaphoreTake(lock, portMAX_DELAY);
    newest = (timeutil::synced() ? timeutil::format(time(nullptr), "%d.%m. %H:%M") : when) + " " + text;
    File file = LittleFS.open(PATH, "a");
    if (file) {
        file.printf("%s %s\n", when.c_str(), text);
        file.close();
    }
    trim();
    xSemaphoreGive(lock);
}

String last()
{
    if (!ready) return "";
    xSemaphoreTake(lock, portMAX_DELAY);
    const String text = newest;
    xSemaphoreGive(lock);
    return text;
}

void print()
{
    xSemaphoreTake(lock, portMAX_DELAY);
    File file = LittleFS.open(PATH, "r");
    Serial.println("[ERRORLOG-BEGIN]");
    while (file && file.available()) Serial.write(file.read());
    Serial.println("[ERRORLOG-END]");
    xSemaphoreGive(lock);
}

}  // namespace errorlog
