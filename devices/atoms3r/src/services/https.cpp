#include "services/https.h"

#include <HTTPClient.h>

namespace https {

bool Client::connect()
{
    if (client.connected()) return true;
    client.stop();
    client.setCACert(caCert);
    const uint32_t start = millis();
    const bool connected = client.connect(host, 443, 10000);
    Serial.printf("[HTTPS] %s: %s in %lu ms\n", host, connected ? "connected" : "no connection", millis() - start);
    return connected;
}

// --- Pool of kept connections ---------------------------------------------------------------

namespace {

constexpr int POOL_SLOTS = 4;
constexpr uint32_t POOL_IDLE_MS = 60000;  // Google closes idle connections after a few minutes

struct Slot {
    const char* host = nullptr;
    Client* client = nullptr;
    bool busy = false;
    uint32_t lastUsed = 0;
};
Slot slots[POOL_SLOTS];
portMUX_TYPE poolMux = portMUX_INITIALIZER_UNLOCKED;

}  // namespace

Lease::Lease(const char* host, const char* caCert)
{
    Client* stale[POOL_SLOTS] = {};
    taskENTER_CRITICAL(&poolMux);
    const uint32_t now = millis();
    int free = -1, oldest = -1;
    for (int i = 0; i < POOL_SLOTS; ++i) {
        Slot& s = slots[i];
        if (s.client && !s.busy && now - s.lastUsed > POOL_IDLE_MS) {  // idle too long: close
            stale[i] = s.client;
            s = Slot();
        }
        if (s.host && !strcmp(s.host, host) && !s.busy && slot < 0) slot = i;
        if (!s.host && free < 0) free = i;
        if (s.host && !s.busy && (oldest < 0 || s.lastUsed < slots[oldest].lastUsed)) oldest = i;
    }
    bool hostBusy = false;
    for (const Slot& s : slots) hostBusy |= s.host && !strcmp(s.host, host) && s.busy;
    if (slot < 0 && !hostBusy) {
        slot = free >= 0 ? free : oldest;  // the least recently used one makes room
        if (slot >= 0 && slots[slot].client) stale[slot] = slots[slot].client;
        if (slot >= 0) {
            slots[slot] = Slot();
            slots[slot].host = host;
            slots[slot].lastUsed = now;
        }
    }
    if (slot >= 0) slots[slot].busy = true;
    taskEXIT_CRITICAL(&poolMux);
    for (Client* client : stale) delete client;  // stop() outside the critical section
    if (slot < 0) {
        used = temporary = new Client(host, caCert);
        return;
    }
    if (!slots[slot].client) slots[slot].client = new Client(host, caCert);
    used = slots[slot].client;
    wasOpen = used->connected();
}

Lease::~Lease()
{
    delete temporary;
    if (slot < 0) return;
    taskENTER_CRITICAL(&poolMux);
    slots[slot].busy = false;
    slots[slot].lastUsed = millis();
    taskEXIT_CRITICAL(&poolMux);
}

void Client::stop() { client.stop(); }

void Client::setTimeout(uint32_t ms)
{
    started = millis();
    timeout = ms;
}

bool Client::write(const uint8_t* data, size_t length)
{
    for (size_t done = 0; done < length;) {
        const size_t n = client.write(data + done, min(size_t(4096), length - done));
        if (!n) {
            if (!client.connected() || !waiting()) return false;
            delay(10);
            continue;
        }
        done += n;
    }
    return true;
}

// Reads one line without CRLF. False at the deadline or when the connection ends.
bool Client::readLine(String& line)
{
    line = "";
    while (waiting()) {
        while (client.available()) {
            const int c = client.read();
            if (c == '\n') return true;
            if (c >= 0 && c != '\r') line += char(c);
        }
        if (!client.connected()) return false;
        delay(10);
    }
    return false;
}

// Copies `length` bytes (or up to the end of the connection if length < 0) into `sink`.
bool Client::copy(int64_t length, Stream& sink)
{
    uint8_t buffer[1024];
    while (length != 0 && waiting()) {
        const int available = client.available();
        if (available <= 0) {
            if (!client.connected()) return length < 0;
            delay(10);
            continue;
        }
        size_t chunk = min(size_t(available), sizeof(buffer));
        if (length > 0) chunk = min(size_t(length), chunk);
        const int n = client.read(buffer, chunk);
        if (n <= 0) continue;
        if (sink.write(buffer, n) != size_t(n)) {  // sink limit: keep what fits, drop the rest
            client.stop();
            return true;
        }
        if (length > 0) length -= n;
    }
    return length == 0 || length < 0;
}

int Client::read(PsramSink& reply)
{
    // Status line and headers
    String line;
    if (!readLine(line)) {
        client.stop();
        return waiting() ? HTTPC_ERROR_CONNECTION_LOST : HTTPC_ERROR_READ_TIMEOUT;
    }
    const int status = line.substring(line.indexOf(' ') + 1).toInt();  // "HTTP/1.1 200 OK"
    int64_t length = -1;
    bool chunked = false, keepAlive = true;
    while (readLine(line) && line.length()) {
        const int colon = line.indexOf(':');
        String name = line.substring(0, colon), value = line.substring(colon + 1);
        name.toLowerCase();
        value.trim();
        if (name == "content-length") length = value.toInt();
        else if (name == "transfer-encoding" && value.equalsIgnoreCase("chunked")) chunked = true;
        else if (name == "connection" && value.equalsIgnoreCase("close")) keepAlive = false;
    }
    // Never a body, with or without Content-Length (Spotify's player answers 204 without one).
    if (status == 204 || status == 304) {
        length = 0;
        chunked = false;
    }

    bool complete = true;
    if (chunked) {
        for (;;) {
            if (!readLine(line)) { complete = false; break; }
            const long size = strtol(line.c_str(), nullptr, 16);
            if (size <= 0) {  // last chunk; skip the (empty) trailer
                while (readLine(line) && line.length()) {}
                break;
            }
            if (!copy(size, reply) || !readLine(line)) { complete = false; break; }
        }
    } else {
        complete = copy(length, reply);
        if (length < 0) keepAlive = false;  // the body ended with the connection
    }
    if (!complete || !keepAlive) client.stop();
    if (!complete) return waiting() ? HTTPC_ERROR_CONNECTION_LOST : HTTPC_ERROR_READ_TIMEOUT;
    return status > 0 ? status : HTTPC_ERROR_NO_HTTP_SERVER;
}

}  // namespace https
