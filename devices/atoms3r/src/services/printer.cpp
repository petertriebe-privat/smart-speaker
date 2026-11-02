#include "services/printer.h"

#include <HTTPClient.h>
#include <WiFiClientSecure.h>
#include <esp_heap_caps.h>

#include "config.h"
#include "system/utf8.h"

namespace printer {
namespace {

constexpr size_t IPP_HEADER_BYTES = 1024;

uint8_t* psramAlloc(size_t bytes)
{
    return static_cast<uint8_t*>(heap_caps_malloc(bytes, MALLOC_CAP_SPIRAM | MALLOC_CAP_8BIT));
}

// --- IPP ------------------------------------------------------------------

size_t putAttribute(uint8_t* out, uint8_t tag, const char* name, const char* value)
{
    const size_t nameLength = strlen(name), valueLength = strlen(value);
    size_t n = 0;
    out[n++] = tag;
    out[n++] = nameLength >> 8;
    out[n++] = nameLength;
    memcpy(out + n, name, nameLength);
    n += nameLength;
    out[n++] = valueLength >> 8;
    out[n++] = valueLength;
    memcpy(out + n, value, valueLength);
    return n + valueLength;
}

// An enum attribute (tag 0x23, four bytes big-endian).
size_t putEnum(uint8_t* out, const char* name, uint32_t value)
{
    const size_t nameLength = strlen(name);
    size_t n = 0;
    out[n++] = 0x23;
    out[n++] = nameLength >> 8;
    out[n++] = nameLength;
    memcpy(out + n, name, nameLength);
    n += nameLength;
    out[n++] = 0;
    out[n++] = 4;
    for (int shift = 24; shift >= 0; shift -= 8) out[n++] = value >> shift;
    return n;
}

// IPP 2.0 request header with the operation attributes every request needs.
size_t beginRequest(uint8_t* out, uint16_t operation)
{
    static uint32_t requestId = 0;
    const String uri = String("ipp://") + PRINTER_HOST + ":" + PRINTER_PORT + PRINTER_PATH;
    size_t n = 0;
    out[n++] = 2;  // version 2.0
    out[n++] = 0;
    out[n++] = operation >> 8;
    out[n++] = operation;
    ++requestId;
    for (int shift = 24; shift >= 0; shift -= 8) out[n++] = requestId >> shift;
    out[n++] = 0x01;  // operation attributes
    n += putAttribute(out + n, 0x47, "attributes-charset", "utf-8");
    n += putAttribute(out + n, 0x48, "attributes-natural-language", "de");
    n += putAttribute(out + n, 0x45, "printer-uri", uri.c_str());
    n += putAttribute(out + n, 0x42, "requesting-user-name", "assistant");
    return n;
}

// Sends one request. Returns the IPP status code, or -1 with `error` set.
int ippSend(const uint8_t* request, size_t length, uint8_t* response, size_t& responseLength,
            String& error)
{
    WiFiClientSecure client;
    client.setInsecure();  // the printer only has a self-signed certificate
    HTTPClient http;
    http.setConnectTimeout(5000);
    http.setTimeout(30000);
    int ippStatus = -1;
    responseLength = 0;
    if (!http.begin(client, PRINTER_HOST, PRINTER_PORT, PRINTER_PATH, true)) {
        error = "printer URL invalid";
        return -1;
    }
    http.addHeader("Content-Type", "application/ipp");
    const int status = http.POST(const_cast<uint8_t*>(request), length);
    if (status != 200) {
        error = status < 0 ? "printer not reachable (" + HTTPClient::errorToString(status) + ")"
                           : "Drucker: HTTP " + String(status);
    } else {
        client.setTimeout(3);  // seconds
        responseLength = http.getStream().readBytes(response, 512);
        if (responseLength >= 8) ippStatus = response[2] << 8 | response[3];
        else error = "printer: no IPP answer";
    }
    http.end();
    return ippStatus;
}

// Integer or enum attribute (tag 0x21 / 0x23) from a response, -1 if missing.
int findInteger(const uint8_t* response, size_t length, const char* name)
{
    const size_t nameLength = strlen(name);
    for (size_t i = 0; i + 9 + nameLength <= length; ++i) {
        if ((response[i] == 0x21 || response[i] == 0x23) && response[i + 1] == 0 &&
            response[i + 2] == nameLength && !memcmp(response + i + 3, name, nameLength)) {
            const uint8_t* v = response + i + 3 + nameLength + 2;
            return v[0] << 24 | v[1] << 16 | v[2] << 8 | v[3];
        }
    }
    return -1;
}

// The ET-4850 has no queue: it rejects a job while it prints the previous one (the second
// page of a text came back as an error). Wait until it is idle with nothing queued.
bool waitUntilIdle(String& error)
{
    uint8_t request[256], response[512];
    size_t n = beginRequest(request, 0x000B);  // Get-Printer-Attributes
    n += putAttribute(request + n, 0x44, "requested-attributes", "printer-state");
    n += putAttribute(request + n, 0x44, "", "queued-job-count");
    request[n++] = 0x03;
    const uint32_t started = millis();
    int state = -1, queued = -1;
    while (millis() - started < PRINTER_WAIT_MS) {
        size_t length = 0;
        if (ippSend(request, n, response, length, error) == 0) {
            state = findInteger(response, length, "printer-state");
            queued = findInteger(response, length, "queued-job-count");
            if (state == 3 && queued <= 0) return true;  // idle
        }
        delay(3000);
    }
    error = "printer not ready after " + String(PRINTER_WAIT_MS / 1000) + " s (status " +
            String(state) + ", Warteschlange " + String(queued) + ")";
    return false;
}

}  // namespace

bool configured() { return *PRINTER_HOST; }

// Sends one JPEG as its own job once the printer is idle. Returns the job id, or 0 with
// `error` set.
int printJpeg(const uint8_t* jpeg, size_t length, const String& jobName, bool color, bool landscape, String& error)
{
    uint8_t* request = psramAlloc(IPP_HEADER_BYTES + length);
    if (!request) { error = "not enough memory"; return 0; }
    size_t n = beginRequest(request, 0x0002);  // Print-Job
    n += putAttribute(request + n, 0x42, "job-name", utf8::prefix(jobName, 100).c_str());
    n += putAttribute(request + n, 0x49, "document-format", "image/jpeg");
    request[n++] = 0x02;  // job attributes
    n += putAttribute(request + n, 0x44, "media", "iso_a4_210x297mm");
    n += putAttribute(request + n, 0x44, "print-color-mode", color ? "color" : "monochrome");
    n += putAttribute(request + n, 0x44, "print-scaling", "fit");
    if (landscape) n += putEnum(request + n, "orientation-requested", 4);  // landscape
    request[n++] = 0x03;
    memcpy(request + n, jpeg, length);

    int jobId = 0;
    uint8_t response[512];
    for (int attempt = 1; attempt <= 3 && !jobId; ++attempt) {
        if (!waitUntilIdle(error)) break;
        size_t responseLength = 0;
        const int status = ippSend(request, n + length, response, responseLength, error);
        Serial.printf("[Print] Print-Job attempt %d: IPP status 0x%04x\n", attempt, status);
        if (status >= 0 && status < 0x0400) {
            jobId = findInteger(response, responseLength, "job-id");
            if (jobId <= 0) jobId = -1;  // accepted, id unknown
        } else if (status >= 0) {
            // 0x0506 not accepting jobs, 0x0507 busy: wait for idle again
            error = "printer refuses (IPP status 0x" + String(status, HEX) + ")";
            if (status != 0x0506 && status != 0x0507) break;
        }
    }
    free(request);
    return jobId;
}

}  // namespace printer
