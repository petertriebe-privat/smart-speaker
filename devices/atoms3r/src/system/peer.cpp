#include "system/peer.h"

#include <ArduinoJson.h>
#include <HTTPClient.h>
#include <WiFi.h>
#include <esp_random.h>
#include <lwip/sockets.h>
#include <mbedtls/md.h>
#include <sys/time.h>

#include "audio/audio.h"
#include "config.h"
#include "system/network.h"
#include "system/occupancy.h"
#include "system/time_util.h"
#include "voice/live_session.h"

namespace peer {
namespace {

// As core/internal/peer/peer.go.
constexpr uint32_t CLAIM_WINDOW_MS = 250;   // a claimant waits this long for the other claims
// Claims this close in time are the same wake word. This model detects up to ~2 s after the
// Echo's (19.10.2026): 1.5 s let both answer.
constexpr int64_t SAME_WAKE_MS = 3000;
constexpr int64_t MAX_SKEW_MS = 10000;      // older or newer datagrams are dropped (replay)
constexpr uint32_t HEARTBEAT_MS = 5000;
constexpr uint32_t ALIVE_MS = 3 * HEARTBEAT_MS;  // a device counts as present this long
constexpr uint32_t DIRECT_MS = 10 * 60 * 1000;   // a device that went quiet still gets every datagram directly
constexpr uint32_t LEVEL_WINDOW_MS = 1000;  // the level is measured in the second before a detection
constexpr uint32_t BUSY_AFTER_MS = 2000;    // a conversation this old answers claims (not our own new one)
constexpr const char* AUTH_HEADER = "X-Jarvis-Auth";
constexpr int MAX_PEERS = 8, MAX_CLAIMS = 8;

struct Device {
    bool used;
    char name[32], room[32], state[16], addr[16], run[16];
    uint64_t seq;
    int port, priority, epoch;
    bool primary;
    uint32_t seen;  // millis()
};

struct Claim {
    char from[32], id[16];
    int64_t at;  // unix ms of the detection
    float level;
    int priority;
    bool busy;
};

Device devices[MAX_PEERS] = {};
Claim claims[MAX_CLAIMS] = {};
int claimNext = 0;
SemaphoreHandle_t lock = nullptr;  // devices: the worker's calls read them too

// One UDP socket: its own task blocks in recvfrom and hands the datagrams to the main loop.
// WiFiUDP::parsePacket in the loop, nominally without waiting, now and then hung 0.4-2.5 s in
// lwIP (19.10.2026) and the loop with it.
int sock = -1;
bool listening = false;
struct Datagram {
    uint32_t from;  // IPv4, network order
    int length;
    char data[];
};
QueueHandle_t inbox = nullptr;

void receiver(void*)
{
    static char buffer[1500];
    for (;;) {
        sockaddr_in from = {};
        socklen_t size = sizeof(from);
        const int got = recvfrom(sock, buffer, sizeof(buffer) - 1, 0, reinterpret_cast<sockaddr*>(&from), &size);
        if (got <= 0) {
            vTaskDelay(pdMS_TO_TICKS(100));
            continue;
        }
        auto* d = static_cast<Datagram*>(heap_caps_malloc(sizeof(Datagram) + got + 1, MALLOC_CAP_SPIRAM | MALLOC_CAP_8BIT));
        if (!d) continue;
        d->from = from.sin_addr.s_addr;
        d->length = got;
        memcpy(d->data, buffer, got);
        d->data[got] = 0;
        if (xQueueSend(inbox, &d, 0) != pdTRUE) heap_caps_free(d);  // the loop is behind: dropped
    }
}

bool startListening()
{
    sock = socket(AF_INET, SOCK_DGRAM, 0);
    if (sock < 0) return false;
    const int on = 1;
    setsockopt(sock, SOL_SOCKET, SO_BROADCAST, &on, sizeof(on));
    setsockopt(sock, SOL_SOCKET, SO_REUSEADDR, &on, sizeof(on));
    sockaddr_in address = {};
    address.sin_family = AF_INET;
    address.sin_port = htons(PEER_PORT);
    address.sin_addr.s_addr = htonl(INADDR_ANY);
    inbox = xQueueCreate(16, sizeof(Datagram*));
    if (!inbox || bind(sock, reinterpret_cast<sockaddr*>(&address), sizeof(address)) < 0 ||
        xTaskCreatePinnedToCore(receiver, "peer", 3072, nullptr, 2, nullptr, 0) != pdPASS) {
        close(sock);
        sock = -1;
        return false;
    }
    return true;
}
bool serving = false;  // the calls' HTTP server runs (PEER_PORT + 1)
char run[13] = "";
uint64_t seq = 0;
uint32_t lastHeartbeat = 0;
bool heartbeatSoon = false;

bool pending = false;  // this device's claim waits for the window
bool won = false;      // it won: a louder claim of the same words that comes late still takes it
Claim own = {};
uint32_t decideAt = 0;
volatile bool lost = false;
String lostWhy;

const char* name() { return *DEVICE_NAME ? DEVICE_NAME : "M5"; }

int64_t nowMs()
{
    timeval now;
    gettimeofday(&now, nullptr);
    return int64_t(now.tv_sec) * 1000 + now.tv_usec / 1000;
}

void randomId(char* out, size_t bytes)
{
    static const char* digits = "0123456789abcdef";
    for (size_t i = 0; i < bytes; ++i) {
        const uint8_t b = esp_random();
        out[2 * i] = digits[b >> 4];
        out[2 * i + 1] = digits[b & 15];
    }
    out[2 * bytes] = 0;
}

String hex(const uint8_t* data, size_t length)
{
    static const char* digits = "0123456789abcdef";
    String text;
    text.reserve(2 * length);
    for (size_t i = 0; i < length; ++i) {
        text += digits[data[i] >> 4];
        text += digits[data[i] & 15];
    }
    return text;
}

// HMAC-SHA256 under PEER_KEY of prefix + body, as hex.
String mac(const String& prefix, const char* body, size_t length)
{
    uint8_t out[32];
    mbedtls_md_context_t context;
    mbedtls_md_init(&context);
    mbedtls_md_setup(&context, mbedtls_md_info_from_type(MBEDTLS_MD_SHA256), 1);
    mbedtls_md_hmac_starts(&context, reinterpret_cast<const uint8_t*>(PEER_KEY), strlen(PEER_KEY));
    mbedtls_md_hmac_update(&context, reinterpret_cast<const uint8_t*>(prefix.c_str()), prefix.length());
    mbedtls_md_hmac_update(&context, reinterpret_cast<const uint8_t*>(body), length);
    mbedtls_md_hmac_finish(&context, out);
    mbedtls_md_free(&context);
    return hex(out, sizeof(out));
}

// Compares in constant time.
bool sameText(const char* a, const char* b, size_t length)
{
    uint8_t diff = 0;
    for (size_t i = 0; i < length; ++i) diff |= a[i] ^ b[i];
    return diff == 0;
}

void copy(char* to, size_t size, const char* from)
{
    strncpy(to, from ? from : "", size - 1);
    to[size - 1] = 0;
}

// The microphone's level in the second before `at` (unix ms), on the Echos' scale.
float levelAt(int64_t at)
{
    const uint32_t end = millis() - uint32_t(max<int64_t>(0, nowMs() - at));
    return audio::rms(end - LEVEL_WINDOW_MS, end) * PEER_LEVEL_SCALE;
}

// The level of this device's own wake word: the loudest second of the last 3.5 s. The model
// here detects one to two seconds after the words (19.10.2026: 17 in the second before the
// detection, 987 for the same words a moment earlier), the Echo's at once.
float ownLevel()
{
    const uint32_t now = millis();
    float loudest = 0;
    for (uint32_t back = 0; back <= 2500; back += 250)
        loudest = max(loudest, audio::rms(now - back - LEVEL_WINDOW_MS, now - back));
    return loudest * PEER_LEVEL_SCALE;
}

void send(JsonDocument& message)
{
    if (!listening) return;
    message["from"] = name();
    if (message["at"].isNull()) message["at"] = nowMs();
    message["run"] = run;
    message["seq"] = ++seq;
    message["prio"] = 0;
    String body;
    serializeJson(message, body);
    const String datagram = "J1 " + mac("", body.c_str(), body.length()) + " " + body;
    // The LAN's broadcast address, as the Echos send (they listen on the same port).
    sockaddr_in to = {};
    to.sin_family = AF_INET;
    to.sin_port = htons(PEER_PORT);
    to.sin_addr.s_addr = htonl(INADDR_BROADCAST);
    if (sendto(sock, datagram.c_str(), datagram.length(), 0, reinterpret_cast<sockaddr*>(&to), sizeof(to)) < 0)
        Serial.printf("[Peer] send failed (errno %d)\n", errno);
    // And to every device seen lately directly (the receiver drops the second copy by its
    // sequence number): the Echo Dot 2's Wi-Fi loses the LAN's broadcasts after every second group
    // key renewal, for ten minutes each, and answered the wake word together with this one
    // (20.10.2026). As the Echos' internal/peer.
    uint32_t direct[MAX_PEERS];
    int n = 0;
    xSemaphoreTake(lock, portMAX_DELAY);
    for (const Device& d : devices) {
        IPAddress ip;
        if (d.used && d.seen && millis() - d.seen < DIRECT_MS && ip.fromString(d.addr)) direct[n++] = uint32_t(ip);
    }
    xSemaphoreGive(lock);
    for (int i = 0; i < n; i++) {
        to.sin_addr.s_addr = direct[i];  // IPAddress keeps network order
        sendto(sock, datagram.c_str(), datagram.length(), 0, reinterpret_cast<sockaddr*>(&to), sizeof(to));
    }
}

const char* state()
{
    if (live::onPhone()) return "phone";
    if (live::open()) return "conversation";
    return "idle";
}

void heartbeat()
{
    JsonDocument message;
    message["kind"] = "hb";
    message["state"] = state();
    if (*DEVICE_ROOM) message["room"] = DEVICE_ROOM;
    message["version"] = "m5";
    message["no_lead"] = true;  // never the leader: the Echos leave it out of the election
    if (serving) message["port"] = PEER_PORT + 1;  // calls to this device (announcements here)
    // The presence sensor (system/occupancy): the leader sees where someone is.
    if (occupancy::available()) message["occupied"] = occupancy::someoneThere();
    send(message);
    lastHeartbeat = millis();
}

Device* deviceFor(const char* from)
{
    Device* free = nullptr;
    Device* oldest = nullptr;
    for (Device& d : devices) {
        if (d.used && !strcmp(d.name, from)) return &d;
        if (!d.used && !free) free = &d;
        if (d.used && (!oldest || int32_t(d.seen - oldest->seen) < 0)) oldest = &d;
    }
    Device* d = free ? free : oldest;
    *d = Device{};
    d->used = true;
    copy(d->name, sizeof(d->name), from);
    return d;
}

bool beats(const Claim& a, const Claim& b)
{
    if (a.level != b.level) return a.level > b.level;
    if (a.priority != b.priority) return a.priority > b.priority;
    return strcmp(a.from, b.from) < 0;
}

// The device `from` stands in this room, or one of the rooms is not known.
bool sameRoom(const char* from)
{
    if (!*DEVICE_ROOM) return true;
    xSemaphoreTake(lock, portMAX_DELAY);
    bool same = true;
    for (const Device& d : devices)
        if (d.used && !strcmp(d.name, from)) same = !*d.room || !strcasecmp(d.room, DEVICE_ROOM);
    xSemaphoreGive(lock);
    return same;
}

void decide()
{
    pending = false;
    for (const Claim& c : claims) {
        if (!c.at || !strcmp(c.from, own.from)) continue;
        const int64_t d = c.at - own.at;
        // A conversation's answer counts only for the claim it answers.
        if (d > SAME_WAKE_MS || d < -SAME_WAKE_MS || (c.busy && strcmp(c.id, own.id))) continue;
        // A conversation in this room keeps the wake word whatever the levels: this microphone hears
        // the other device's speaker, which its own echo cancellation takes out (#62: two of them
        // talking to each other). Only this device decides on its claim, so the rule is its own.
        if (c.busy && sameRoom(c.from)) {
            lostWhy = String(c.from) + " has a conversation in this room";
            lost = true;
            Serial.printf("\n[Peer] wake word: %s\n", lostWhy.c_str());
            return;
        }
        if (beats(c, own)) {
            lostWhy = String(c.from) + (c.busy ? " louder, in a conversation (" : " louder (") + String(c.level, 0) +
                      " > " + String(own.level, 0) + ")";
            lost = true;
            Serial.printf("\n[Peer] wake word: %s answers\n", lostWhy.c_str());
            return;
        }
    }
    Serial.printf("\n[Peer] wake word: answering here (loudest, %.0f)\n", own.level);
    won = true;
}

// A claim that came after the window: a device whose model detects later (this one's does, by
// about a second) must still be able to take the words when it heard them louder. The same rule
// on the Echo (peer.go, outvoiced): within SAME_WAKE_MS of the words the louder one keeps them.
void lateClaim(const Claim& c)
{
    if (!won || c.busy || nowMs() - own.at > SAME_WAKE_MS) {
        if (won && nowMs() - own.at > SAME_WAKE_MS) won = false;
        return;
    }
    const int64_t d = c.at - own.at;
    if (d > SAME_WAKE_MS || d < -SAME_WAKE_MS || !beats(c, own)) return;
    won = false;
    lostWhy = String(c.from) + " louder, late (" + String(c.level, 0) + " > " + String(own.level, 0) + ")";
    lost = true;
    Serial.printf("\n[Peer] wake word: %s answers\n", lostWhy.c_str());
}

void handle(JsonDocument& m, const IPAddress& from)
{
    const char* sender = m["from"] | "";
    const char* kind = m["kind"] | "";
    xSemaphoreTake(lock, portMAX_DELAY);
    Device* d = deviceFor(sender);
    // A device that is new or back gets a heartbeat at once, as on the Echo.
    const bool news = !d->seen || millis() - d->seen > ALIVE_MS;
    d->seen = millis();
    copy(d->addr, sizeof(d->addr), from.toString().c_str());
    d->priority = m["prio"] | 0;
    if (!strcmp(kind, "hb")) {
        copy(d->room, sizeof(d->room), m["room"] | "");
        copy(d->state, sizeof(d->state), m["state"] | "");
        d->primary = m["primary"] | false;
        d->port = m["port"] | 0;
        d->epoch = m["epoch"] | 0;
    }
    xSemaphoreGive(lock);
    if (news) {
        Serial.printf("\n[Peer] %s at %s\n", sender, from.toString().c_str());
        heartbeatSoon = true;
    }
    if (strcmp(kind, "wake")) return;
    Claim& c = claims[claimNext];
    claimNext = (claimNext + 1) % MAX_CLAIMS;
    c = Claim{};
    copy(c.from, sizeof(c.from), sender);
    copy(c.id, sizeof(c.id), m["id"] | "");
    c.at = m["at"] | int64_t(0);
    c.level = m["level"] | 0.0f;
    c.priority = m["prio"] | 0;
    c.busy = m["busy"] | false;
    // For calibrating PEER_LEVEL_SCALE: the other device's level next to this one's.
    Serial.printf("\n[Peer] claim from %s%s: level %.0f, here %.0f, %lld ms old\n", c.from,
                  c.busy ? " (in a conversation)" : "", c.level, levelAt(c.at), (long long)(nowMs() - c.at));
    if (!pending) lateClaim(c);
    // Someone heard the wake word while a conversation runs here: how loud was it here?
    if (!c.busy && live::open() && live::elapsedSeconds() * 1000 >= BUSY_AFTER_MS) {
        JsonDocument answer;
        answer["kind"] = "wake";
        answer["id"] = c.id;
        answer["at"] = c.at;
        answer["busy"] = true;
        answer["level"] = levelAt(c.at);
        send(answer);
    }
}

// The message of a datagram, if it is signed with our key, fresh and not seen before.
bool open(char* data, size_t length, JsonDocument& m)
{
    if (length < 3 + 64 + 2 || strncmp(data, "J1 ", 3) || data[3 + 64] != ' ') return false;
    const char* body = data + 3 + 64 + 1;
    const size_t bodyLength = length - (body - data);
    const String expected = mac("", body, bodyLength);
    if (!sameText(expected.c_str(), data + 3, 64)) return false;
    if (deserializeJson(m, body, bodyLength)) return false;
    const char* sender = m["from"] | "";
    if (!*sender || !strcmp(sender, name())) return false;
    const int64_t age = nowMs() - (m["at"] | int64_t(0));
    const bool wake = !strcmp(m["kind"] | "", "wake");
    if (age > MAX_SKEW_MS + (wake ? SAME_WAKE_MS : 0) || age < -MAX_SKEW_MS) return false;
    const char* senderRun = m["run"] | "";
    const uint64_t senderSeq = m["seq"] | uint64_t(0);
    xSemaphoreTake(lock, portMAX_DELAY);
    Device* d = deviceFor(sender);
    const bool replay = !strcmp(d->run, senderRun) && senderSeq <= d->seq;
    if (!replay) {
        copy(d->run, sizeof(d->run), senderRun);
        d->seq = senderSeq;
    }
    xSemaphoreGive(lock);
    return !replay;
}

void receive()
{
    Datagram* d = nullptr;
    while (xQueueReceive(inbox, &d, 0) == pdTRUE) {
        JsonDocument m;
        if (open(d->data, d->length, m)) handle(m, IPAddress(d->from));
        heap_caps_free(d);
    }
}

// "<ms> <nonce> <hex HMAC of '<ms> <nonce> <path> <body>'>", as rpc.go signs.
String sign(int64_t ms, const String& nonce, const String& path, const String& body)
{
    const String prefix = String(ms) + " " + nonce + " " + path + " ";
    return String(ms) + " " + nonce + " " + mac(prefix, body.c_str(), body.length());
}

bool verify(const String& header, const String& path, const String& body)
{
    const int first = header.indexOf(' '), second = header.indexOf(' ', first + 1);
    if (first <= 0 || second <= first) return false;
    const int64_t ms = atoll(header.substring(0, first).c_str());
    const int64_t age = nowMs() - ms;
    if (age > MAX_SKEW_MS || age < -MAX_SKEW_MS) return false;
    const String expected = sign(ms, header.substring(first + 1, second), path, body);
    return expected.length() == header.length() && sameText(expected.c_str(), header.c_str(), header.length());
}

bool aliveDevice(const Device& d) { return d.used && d.seen && millis() - d.seen < ALIVE_MS; }

// --- Calls to this device (HTTP on PEER_PORT + 1) -------------------------------------------

constexpr uint32_t REQUEST_TIMEOUT_MS = 500;  // a device in the LAN sends its request at once
constexpr size_t BODY_MAX = 8192;
constexpr int NONCES = 16;

struct Route {
    const char* path;
    Handler handler;
};
Route routes[4] = {};
int routeCount = 0;
WiFiServer server(PEER_PORT + 1);
String nonces[NONCES];  // the last requests' nonces: a replayed call is refused
int nonceNext = 0;

// One line up to CRLF, until `deadline`; false if none came.
bool readLine(WiFiClient& client, String& line, uint32_t deadline)
{
    line = "";
    while (int32_t(millis() - deadline) < 0) {
        while (client.available()) {
            const char c = client.read();
            if (c == '\n') return true;
            if (c != '\r' && line.length() < 512) line += c;
        }
        if (!client.connected()) return false;
        delay(1);
    }
    return false;
}

void reply(WiFiClient& client, int status, const String& path, const String& body)
{
    String head = "HTTP/1.1 " + String(status) + (status == 200 ? " OK" : status == 404 ? " Not Found" : " Forbidden") +
                  "\r\nContent-Type: application/json\r\nConnection: close\r\nContent-Length: " + String(body.length()) +
                  "\r\n";
    if (status == 200) {
        char nonce[13];
        randomId(nonce, 6);
        head += String(AUTH_HEADER) + ": " + sign(nowMs(), nonce, "answer " + path, body) + "\r\n";
    }
    client.print(head + "\r\n" + body);
}

void serve()
{
    WiFiClient client = server.available();
    if (!client) return;
    const uint32_t deadline = millis() + REQUEST_TIMEOUT_MS;
    String line, path, auth;
    size_t length = 0;
    if (readLine(client, line, deadline) && line.startsWith("POST ")) path = line.substring(5, line.indexOf(' ', 5));
    while (readLine(client, line, deadline) && line.length()) {
        const int colon = line.indexOf(':');
        if (colon <= 0) continue;
        String name = line.substring(0, colon), value = line.substring(colon + 1);
        name.toLowerCase();
        value.trim();
        if (name == "content-length") length = value.toInt();
        else if (name == "x-jarvis-auth") auth = value;
    }
    String body;
    if (length <= BODY_MAX) {
        body.reserve(length);
        while (body.length() < length && int32_t(millis() - deadline) < 0) {
            while (client.available() && body.length() < length) body += char(client.read());
            if (body.length() < length) delay(1);
        }
    }
    const int space = auth.indexOf(' '), second = auth.indexOf(' ', space + 1);
    const String nonce = space > 0 && second > space ? auth.substring(space + 1, second) : String();
    bool replayed = nonce.isEmpty();
    for (const String& seen : nonces) replayed |= seen == nonce;
    if (path.isEmpty() || body.length() != length || replayed || !verify(auth, path, body)) {
        Serial.printf("\n[Peer] call %s from %s refused\n", path.c_str(), client.remoteIP().toString().c_str());
        reply(client, 403, path, "forbidden");
        client.stop();
        return;
    }
    nonces[nonceNext] = nonce;
    nonceNext = (nonceNext + 1) % NONCES;
    for (int i = 0; i < routeCount; ++i) {
        if (path != routes[i].path) continue;
        Serial.printf("\n[Peer] call %s from %s\n", path.c_str(), client.remoteIP().toString().c_str());
        reply(client, 200, path, routes[i].handler(body));
        client.stop();
        return;
    }
    reply(client, 404, path, "unknown");
    client.stop();
}

}  // namespace

void handle(const char* path, Handler handler)
{
    if (routeCount < int(sizeof(routes) / sizeof(routes[0]))) routes[routeCount++] = Route{path, handler};
}

void begin()
{
    if (!MEMBER) return;
    lock = xSemaphoreCreateMutex();
    randomId(run, 6);
    Serial.printf("[Peer] member of the network as \"%s\"%s%s\n", name(), *DEVICE_ROOM ? " in " : "", DEVICE_ROOM);
}

void update()
{
    if (!MEMBER || !lock || !network::connected() || !timeutil::synced()) return;
    if (!listening) {
        listening = startListening();
        if (!listening) return;
        Serial.printf("\n[Peer] listening on %u\n", PEER_PORT);
        heartbeatSoon = true;
    }
    // The calls' server in a task of its own: WiFiServer::available() in the loop hung it 2.5 s
    // now and then (19.10.2026). The handlers may run in any task.
    if (!serving && routeCount)
        serving = xTaskCreatePinnedToCore(
                      [](void*) {
                          server.begin();
                          server.setNoDelay(true);
                          for (;;) {
                              serve();
                              vTaskDelay(pdMS_TO_TICKS(20));
                          }
                      },
                      "peer calls", 6144, nullptr, 1, nullptr, 0) == pdPASS;
    const uint32_t started = millis();
    receive();
    const uint32_t received = millis();
    static bool wasOccupied = false;  // a change goes out at once, not after HEARTBEAT_MS
    if (occupancy::someoneThere() != wasOccupied) {
        wasOccupied = !wasOccupied;
        heartbeatSoon = true;
    }
    if (heartbeatSoon || millis() - lastHeartbeat >= HEARTBEAT_MS) {
        heartbeatSoon = false;
        heartbeat();
    }
    if (millis() - started > 200)
        Serial.printf("[Peer] slow: receive %lu ms, heartbeat %lu ms\n", (unsigned long)(received - started),
                      (unsigned long)(millis() - received));
    if (pending && int32_t(millis() - decideAt) >= 0) decide();
}

bool claim(float score)
{
    lost = won = false;
    if (!MEMBER || !listening || !alive()) return true;
    own = Claim{};
    copy(own.from, sizeof(own.from), name());
    randomId(own.id, 6);
    own.at = nowMs();
    own.level = ownLevel();
    JsonDocument message;
    message["kind"] = "wake";
    message["id"] = own.id;
    message["at"] = own.at;
    message["level"] = own.level;
    message["score"] = score;
    send(message);
    Serial.printf("\n[Peer] claim sent: level %.0f, score %.2f\n", own.level, score);
    pending = true;
    decideAt = millis() + CLAIM_WINDOW_MS;
    return false;
}

bool deciding() { return pending; }

bool takeLost(String& why)
{
    if (!lost) return false;
    lost = false;
    why = lostWhy;
    return true;
}

int alive()
{
    if (!lock) return 0;
    int n = 0;
    xSemaphoreTake(lock, portMAX_DELAY);
    for (const Device& d : devices) n += aliveDevice(d);
    xSemaphoreGive(lock);
    return n;
}

bool leaderPresent() { return leaderName().length() > 0; }

volatile bool testOffline = false;
void setOffline(bool on) { testOffline = on; }
bool offline() { return testOffline; }

String leaderName()
{
    if (!lock || testOffline) return "";
    String found;
    xSemaphoreTake(lock, portMAX_DELAY);
    for (const Device& d : devices)
        if (aliveDevice(d) && d.primary) found = d.name;
    xSemaphoreGive(lock);
    return found;
}

bool callLeader(const char* path, const String& body, String& answer, uint32_t timeoutMs)
{
    if (!lock || testOffline) {
        answer = testOffline ? "no leader reachable (test)" : "peer link off";
        return false;
    }
    String host;
    int port = 0;
    xSemaphoreTake(lock, portMAX_DELAY);
    for (const Device& d : devices) {
        if (aliveDevice(d) && d.primary) {
            host = d.addr;
            port = d.port ? d.port : PEER_PORT + 1;
        }
    }
    xSemaphoreGive(lock);
    if (host.isEmpty()) {
        answer = "no leader reachable";
        return false;
    }
    char nonce[13];
    randomId(nonce, 6);
    HTTPClient http;
    http.useHTTP10(true);  // no chunked answer: the whole body in one read
    http.setTimeout(timeoutMs);
    http.setConnectTimeout(min<uint32_t>(timeoutMs, 3000));
    if (!http.begin("http://" + host + ":" + String(port) + path)) {
        answer = "bad address";
        return false;
    }
    const char* collect[] = {AUTH_HEADER};
    http.collectHeaders(collect, 1);
    http.addHeader("Content-Type", "application/json");
    http.addHeader(AUTH_HEADER, sign(nowMs(), nonce, path, body));
    const int status = http.POST(body);
    answer = status > 0 ? http.getString() : HTTPClient::errorToString(status);
    const String header = http.header(AUTH_HEADER);
    http.end();
    if (status != 200) {
        answer = String(path) + ": " + (status > 0 ? "HTTP " + String(status) + " " : String()) + answer;
        answer.trim();
        return false;
    }
    if (!verify(header, String("answer ") + path, answer)) {
        answer = String(path) + ": answer not signed";
        return false;
    }
    return true;
}

String describe()
{
    String text = String("this one is ") + name() + (*DEVICE_ROOM ? String(" (") + DEVICE_ROOM + ")" : String());
    if (!lock) return text;
    xSemaphoreTake(lock, portMAX_DELAY);
    for (const Device& d : devices) {
        if (!aliveDevice(d)) continue;
        text += String("; ") + d.name;
        String extra = d.room;
        if (d.primary) extra += String(extra.length() ? ", " : "") + "primary device: phone, chats, mail, memory";
        if (extra.length()) text += " (" + extra + ")";
    }
    xSemaphoreGive(lock);
    return text;
}

void print()
{
    if (!MEMBER) {
        Serial.println("\n[Peer] off (no PEER_KEY): this device acts alone");
        return;
    }
    Serial.printf("\n[Peer] \"%s\" room \"%s\", %s, level now %.0f\n", name(), DEVICE_ROOM,
                  listening ? "listening" : "not listening yet", levelAt(nowMs()));
    xSemaphoreTake(lock, portMAX_DELAY);
    for (const Device& d : devices) {
        if (!d.used) continue;
        Serial.printf("  %s at %s: %s, room \"%s\", primary %d, epoch %d, prio %d, seen %lu s ago%s\n", d.name, d.addr,
                      d.state, d.room, d.primary, d.epoch, d.priority, (unsigned long)((millis() - d.seen) / 1000),
                      aliveDevice(d) ? "" : " (gone)");
    }
    xSemaphoreGive(lock);
}

}  // namespace peer
