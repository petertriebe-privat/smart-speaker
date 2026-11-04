#pragma once

#include <Arduino.h>
#include <WiFiClientSecure.h>

#include "services/psram_sink.h"

// Plain HTTP/1.1 over TLS for OpenAI and Telegram, instead of HTTPClient: its timeout is a
// uint16_t (max. 65 s), and OpenAI sends nothing until reasoning, a web search or an image is
// done. The connection stays open for further requests while the server allows it (Telegram's
// polling); the certificate is checked against `caCert`.
namespace https {

class Client {
public:
    Client(const char* host, const char* caCert) : host(host), caCert(caCert) {}
    ~Client() { stop(); }

    // Connects unless still connected. Every request starts with it.
    bool connect();
    bool connected() { return client.connected(); }
    void stop();

    // Deadline for the request from now on (connect, write and read).
    void setTimeout(uint32_t ms);
    // Writes everything, in 4 KB pieces: one print() of a large body (>30 KB) wrote only part of it.
    bool write(const uint8_t* data, size_t length);
    bool write(const String& data) { return write(reinterpret_cast<const uint8_t*>(data.c_str()), data.length()); }
    // Reads status line, headers and body (Content-Length, chunked or up to the end) into
    // `reply`. Returns the HTTP status or a negative HTTPClient error
    // (HTTPC_ERROR_READ_TIMEOUT at the deadline). Closes the connection unless the server keeps it.
    int read(PsramSink& reply);

    const char* host;

private:
    bool readLine(String& line);
    bool copy(int64_t length, Stream& sink);
    bool waiting() const { return millis() - started < timeout; }

    const char* caCert;
    WiFiClientSecure client;
    uint32_t started = 0, timeout = 10000;
};

// A kept connection to `host` for one request, from a small pool: a TLS handshake costs the
// ESP32 1-2 s of CPU, a request on an open connection a few hundred ms. Held exclusively while
// the Lease lives; if another task is using the host's connection, a temporary one. Connections
// idle for over POOL_IDLE_MS are closed at the next lease. Requests must not send
// "Connection: close".
class Lease {
public:
    Lease(const char* host, const char* caCert);
    ~Lease();
    Lease(const Lease&) = delete;
    Lease& operator=(const Lease&) = delete;
    Client& client() { return *used; }
    // The connection was open before this request: if the request fails, the server may have
    // closed it meanwhile; stop() and try once more.
    bool reused() const { return wasOpen; }

private:
    int slot = -1;
    Client* used = nullptr;
    Client* temporary = nullptr;
    bool wasOpen = false;
};

}  // namespace https
