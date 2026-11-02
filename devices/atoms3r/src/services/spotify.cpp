#include "services/spotify.h"

#include <algorithm>
#include <ArduinoJson.h>
#include <HTTPClient.h>
#include <base64.h>
#include <vector>

#include "assistant/people.h"
#include "assistant/tools.h"
#include "ca_certs.h"
#include "config.h"
#include "services/https.h"
#include "services/psram_json.h"
#include "services/psram_sink.h"
#include "system/utf8.h"

namespace spotify {
namespace {

constexpr const char* API_HOST = "api.spotify.com";
constexpr const char* TOKEN_HOST = "accounts.spotify.com";
constexpr const char* NOT_VISIBLE =
    " Phones and computers appear only while the Spotify app is open on them; an Echo of the household "
    "that is missing can start Spotify itself with alexa (command, e.g. 'spiele ... von Spotify'). A speaker "
    "the Spotify app finds only in the local WiFi (e.g. a friend's while visiting) is not known to the account "
    "until the user picks it once in the app; after that it can be controlled here.";

// Other people's accounts from secrets.h; the admin's is SPOTIFY_REFRESH_TOKEN.
struct Account {
    const char* person;  // name or id in the register, "" = the admin
    const char* refresh;
};
const std::vector<Account> OTHERS = {SPOTIFY_ACCOUNTS};

// Access tokens per refresh token, with the account's Spotify user id. Guarded by tokenLock:
// tools run in several tasks (worker, Telegram, WhatsApp).
struct Token {
    String refresh, access, user;
    uint32_t until = 0;  // millis()
};
std::vector<Token> tokens;
SemaphoreHandle_t tokenLock = xSemaphoreCreateMutex();

// Whose account a call uses: the refresh token, the person (people id) and how results name it.
struct Session {
    String refresh, person, label;  // label "" for the admin's own account
};

String lower(String text)
{
    text.trim();
    text.toLowerCase();  // ASCII only; umlauts compare byte by byte
    return text;
}

String urlEncode(const String& text)
{
    static const char* hex = "0123456789ABCDEF";
    String out;
    for (size_t i = 0; i < text.length(); ++i) {
        const uint8_t c = text[i];
        if (isalnum(c) || c == '-' || c == '_' || c == '.' || c == '~') {
            out += char(c);
        } else {
            out += '%';
            out += hex[c >> 4];
            out += hex[c & 15];
        }
    }
    return out;
}

String clock(int64_t ms)
{
    const int seconds = int(ms / 1000);
    char text[16];
    snprintf(text, sizeof(text), "%d:%02d", seconds / 60, seconds % 60);
    return text;
}

String names(JsonArrayConst list)
{
    String text;
    for (JsonObjectConst entry : list) text += (text.length() ? ", " : "") + String(entry["name"] | "");
    return text;
}

// One line for a track, episode, album, artist, playlist, show or audiobook.
String describe(JsonObjectConst item)
{
    const String type = item["type"] | "", name = item["name"] | "";
    if (type == "track") return "'" + name + "' by " + names(item["artists"]);
    if (type == "episode") return "episode '" + name + "' of " + String(item["show"]["name"] | "a podcast");
    if (type == "album") return "album '" + name + "' by " + names(item["artists"]);
    if (type == "artist") return "artist " + name;
    if (type == "playlist") return "playlist '" + name + "' (by " + String(item["owner"]["display_name"] | "?") + ")";
    if (type == "show") return "podcast '" + name + "'";
    if (type == "audiobook") return "audiobook '" + name + "' by " + names(item["authors"]);
    return "'" + name + "'";
}

// --- HTTP and OAuth -------------------------------------------------------------------

// Over a kept connection (https::Lease); one the server closed meanwhile is opened anew once.
int send(const char* method, const char* host, const char* ca, const String& path, const String& headers,
         const String& body, PsramSink& reply)
{
    https::Lease lease(host, ca);
    https::Client& client = lease.client();
    const String head = String(method) + " " + path + " HTTP/1.1\r\nHost: " + host + "\r\n" + headers +
                        "Content-Length: " + String(body.length()) + "\r\n\r\n";
    for (int attempt = 0;; ++attempt) {
        client.setTimeout(SPOTIFY_TIMEOUT_MS);
        if (!client.connect()) return HTTPC_ERROR_CONNECTION_REFUSED;
        int status = HTTPC_ERROR_SEND_PAYLOAD_FAILED;
        reply.size = 0;
        if (client.write(head) && (body.isEmpty() || client.write(body))) status = client.read(reply);
        if (status >= 0 || status == HTTPC_ERROR_READ_TIMEOUT || !lease.reused() || attempt) {
            if (status < 0) client.stop();
            return status;
        }
        client.stop();  // the kept connection was closed by the server: once more on a new one
    }
}

String errorText(int status, const PsramSink& reply)
{
    if (status == HTTPC_ERROR_READ_TIMEOUT) return "Spotify does not answer";
    if (status < 0) return "Spotify not reachable (" + HTTPClient::errorToString(status) + ")";
    if (status == 429) return "Spotify's rate limit, try again in a minute";
    JsonDocument parsed;
    String message;
    if (reply.data && reply.size && !deserializeJson(parsed, reply.data, reply.size)) {
        message = parsed["error"]["message"] | "";                            // API
        if (message.isEmpty()) message = parsed["error_description"] | "";  // token endpoint
        if (message.isEmpty()) message = parsed["error"] | "";
    }
    if (status == 403 && message.indexOf("remium") >= 0) message += " (this needs Spotify Premium)";
    return "HTTP " + String(status) + (message.length() ? ": " + utf8::prefix(message, 200) : "");
}

// A valid access token for `refresh` (fetched with it, kept until 5 minutes before it expires).
bool authorizeLocked(Token& entry, String& error)
{
    if (entry.access.length() && int32_t(entry.until - millis()) > 0) return true;
    const String headers = "Authorization: Basic " + base64::encode(String(SPOTIFY_CLIENT_ID) + ":" + SPOTIFY_CLIENT_SECRET) +
                           "\r\nContent-Type: application/x-www-form-urlencoded\r\n";
    PsramSink reply(16 * 1024);
    const int status = send("POST", TOKEN_HOST, SPOTIFY_ACCOUNTS_CA, "/api/token", headers,
                            "grant_type=refresh_token&refresh_token=" + urlEncode(entry.refresh), reply);
    JsonDocument parsed;
    if (status != 200 || deserializeJson(parsed, reply.data, reply.size) || !parsed["access_token"].is<const char*>()) {
        if (status == 400 && reply.data && strstr(reinterpret_cast<const char*>(reply.data), "invalid_grant"))
            error = "Error: the Spotify login expired or was revoked; scripts/spotify_auth.py has to run once more.";
        else
            error = "Error: Spotify login failed (" + errorText(status, reply) + ")";
        return false;
    }
    entry.access = parsed["access_token"].as<const char*>();
    entry.until = millis() + (max(uint32_t(parsed["expires_in"] | 3600), uint32_t(600)) - 300) * 1000;
    return true;
}

Token& tokenLocked(const String& refresh)
{
    for (Token& t : tokens)
        if (t.refresh == refresh) return t;
    tokens.emplace_back();
    tokens.back().refresh = refresh;
    return tokens.back();
}

bool authorize(const String& refresh, String& token, String& error)
{
    xSemaphoreTake(tokenLock, portMAX_DELAY);
    Token& entry = tokenLocked(refresh);
    const bool ok = authorizeLocked(entry, error);
    token = entry.access;
    xSemaphoreGive(tokenLock);
    return ok;
}

void dropToken(const String& refresh)
{
    xSemaphoreTake(tokenLock, portMAX_DELAY);
    tokenLocked(refresh).access = "";
    xSemaphoreGive(tokenLock);
}

// One Web API call (`path` from /v1 on, `body` JSON or ""). Returns the HTTP status (0 without a
// token); `error` is set unless it is 2xx. A 401 fetches a new token once.
int api(const Session& session, const char* method, const String& path, const String& body, PsramSink& reply,
        String& error)
{
    for (int attempt = 0; attempt < 2; ++attempt) {
        String token;
        if (!authorize(session.refresh, token, error)) return 0;
        String headers = "Authorization: Bearer " + token + "\r\n";
        if (body.length()) headers += "Content-Type: application/json\r\n";
        const int status = send(method, API_HOST, META_CA, "/v1" + path, headers, body, reply);
        if (status == 401 && attempt == 0) {
            dropToken(session.refresh);
            continue;
        }
        if (status < 200 || status >= 300) error = "Error: " + errorText(status, reply);
        return status;
    }
    return 401;
}

// A call whose answer is JSON, parsed into `doc`. False with `error` otherwise (204 = empty doc).
bool apiJson(const Session& session, const char* method, const String& path, const String& body, JsonDocument& doc,
             String& error, int* statusOut = nullptr)
{
    PsramSink reply;
    const int status = api(session, method, path, body, reply, error);
    if (statusOut) *statusOut = status;
    if (status < 200 || status >= 300) return false;
    if (status == 204 || !reply.size) return true;
    if (deserializeJson(doc, reply.data, reply.size, DeserializationOption::NestingLimit(32))) {
        error = "Error: Spotify sent no valid JSON";
        return false;
    }
    return true;
}

bool apiJson(const Session& session, const String& path, JsonDocument& doc, String& error)
{
    return apiJson(session, "GET", path, "", doc, error);
}

// A call without an interesting answer: "" or the error.
String apiDo(const Session& session, const char* method, const String& path, const String& body = "")
{
    PsramSink reply(64 * 1024);
    String error;
    const int status = api(session, method, path, body, reply, error);
    return status >= 200 && status < 300 ? String() : error;
}

// The account's Spotify user id (kept with its token).
String userId(const Session& session, String& error)
{
    xSemaphoreTake(tokenLock, portMAX_DELAY);
    const String known = tokenLocked(session.refresh).user;
    xSemaphoreGive(tokenLock);
    if (known.length()) return known;
    JsonDocument me(PsramJson::instance());
    if (!apiJson(session, "/me", me, error)) return "";
    const String id = me["id"] | "";
    xSemaphoreTake(tokenLock, portMAX_DELAY);
    tokenLocked(session.refresh).user = id;
    xSemaphoreGive(tokenLock);
    return id;
}

// --- Accounts ---------------------------------------------------------------------------

String accountOf(const String& person)  // refresh token of a people id, "" if none
{
    if (person == people::admin()) return SPOTIFY_REFRESH_TOKEN;
    for (const Account& account : OTHERS) {
        const String id = *account.person ? people::byName(account.person) : people::admin();
        if (id == person && *account.refresh) return account.refresh;
    }
    return "";
}

// The account for a call: `wanted` (a person's name) if given, else the caller's own, else the
// admin's. Another person's account only for them and the admin (at home, unknown = trusted).
bool session(const tools::Context& context, const String& wanted, Session& out, String& error)
{
    String person = context.person.length() && accountOf(context.person).length() ? context.person : people::admin();
    if (wanted.length()) {
        person = people::byName(wanted);
        if (person.isEmpty()) {
            error = "Error: nobody called \"" + wanted + "\" in the household.";
            return false;
        }
        if (context.person.length() && context.person != person && context.person != people::admin()) {
            error = "Error: only " + people::name(person) + " or the admin may use " + people::name(person) + "'s Spotify.";
            return false;
        }
    }
    out.refresh = accountOf(person);
    out.person = person;
    out.label = person == people::admin() ? "" : people::name(person);
    if (out.refresh.isEmpty()) {
        error = "Error: " + people::name(person) + " has no Spotify account linked yet (scripts/spotify_auth.py --other, "
                "then SPOTIFY_ACCOUNTS in secrets.h).";
        return false;
    }
    return true;
}

String suffix(const Session& session) { return session.label.length() ? " (" + session.label + "'s Spotify)" : ""; }

// --- Devices ----------------------------------------------------------------------------

struct Device {
    String id, name, type;
    bool active = false, restricted = false, volumeOk = false;
    int volume = -1;
};

bool loadDevices(const Session& session, std::vector<Device>& out, String& error)
{
    JsonDocument doc(PsramJson::instance());
    if (!apiJson(session, "/me/player/devices", doc, error)) return false;
    for (JsonObjectConst d : doc["devices"].as<JsonArrayConst>()) {
        Device device;
        device.id = d["id"] | "";
        device.name = d["name"] | "";
        device.type = d["type"] | "";
        device.active = d["is_active"] | false;
        device.restricted = d["is_restricted"] | false;
        device.volumeOk = d["supports_volume"] | false;
        device.volume = d["volume_percent"].is<int>() ? d["volume_percent"].as<int>() : -1;
        if (device.id.length()) out.push_back(device);
    }
    return true;
}

String deviceNames(const std::vector<Device>& devices)
{
    String text;
    for (const Device& d : devices) text += (text.length() ? ", " : "") + d.name + " (" + d.type + ")";
    return text.length() ? text : String("none");
}

bool isPhoneWord(const String& word)
{
    return word == "handy" || word == "smartphone" || word == "phone" || word == "telefon" || word == "mobile";
}

// Whether every word of `key` (3 letters or more, a German genitive s dropped) is in `name`.
bool wordsIn(const String& name, const String& key, bool skipPhoneWords)
{
    int start = 0, words = 0;
    while (start <= int(key.length())) {
        int end = key.indexOf(' ', start);
        if (end < 0) end = key.length();
        String word = key.substring(start, end);
        start = end + 1;
        if (word.length() < 3 || (skipPhoneWords && isPhoneWord(word))) continue;
        ++words;
        if (name.indexOf(word) >= 0) continue;
        if (word.endsWith("s") && name.indexOf(word.substring(0, word.length() - 1)) >= 0) continue;
        return false;
    }
    return words > 0;
}

// The numbers among the words of `text`, sorted and joined: a number tells devices of one family
// apart ("Jarvis" is not "Jarvis 2", either way round; two Echos, 18.10.2026).
String numbersIn(const String& text)
{
    std::vector<String> numbers;
    int start = 0;
    while (start <= int(text.length())) {
        int end = text.indexOf(' ', start);
        if (end < 0) end = text.length();
        const String word = text.substring(start, end);
        start = end + 1;
        bool digits = word.length() > 0;
        for (char c : word) digits = digits && isdigit((unsigned char)c);
        if (digits) numbers.push_back(word);
    }
    std::sort(numbers.begin(), numbers.end());
    String out;
    for (const String& n : numbers) out += n + " ";
    return out;
}

// The device meant by `wanted`: its exact name, else the only match (by name or words; "Handy"
// means the smartphones, an "Echo ..." wins over others). Nullptr with `error` otherwise.
const Device* findDevice(const std::vector<Device>& devices, const String& wanted, String& error)
{
    const String key = lower(wanted);
    for (const Device& d : devices)
        if (lower(d.name) == key) return &d;
    bool phone = false;
    for (const char* word : {"handy", "smartphone", "phone", "telefon", "mobile"})
        if (key.indexOf(word) >= 0) phone = true;
    std::vector<const Device*> hits, echoHits;
    for (const Device& d : devices) {
        const String name = lower(d.name);
        bool hit;
        if (phone) hit = d.type == "Smartphone" && (wordsIn(name, key, true) || !wordsIn(key, key, true));
        else hit = (name.indexOf(key) >= 0 || wordsIn(name, key, false)) && numbersIn(name) == numbersIn(key);
        if (!hit) continue;
        hits.push_back(&d);
        if (name.startsWith("echo")) echoHits.push_back(&d);
    }
    if (hits.size() == 1) return hits[0];
    if (echoHits.size() == 1) return echoHits[0];
    if (hits.empty()) {
        error = "Error: Spotify does not see a speaker \"" + wanted + "\". Visible: " + deviceNames(devices) + "." + NOT_VISIBLE;
    } else {
        std::vector<Device> several;
        for (const Device* d : hits) several.push_back(*d);
        error = "Error: several speakers match \"" + wanted + "\": " + deviceNames(several) + ". Name one.";
    }
    return nullptr;
}

// The names in SPOTIFY_ROOM_SPEAKERS, lower case.
std::vector<String> roomSpeakers()
{
    std::vector<String> list;
    const String all = SPOTIFY_ROOM_SPEAKERS;
    int start = 0;
    while (start < int(all.length())) {
        int end = all.indexOf(',', start);
        if (end < 0) end = all.length();
        const String name = lower(all.substring(start, end));
        if (name.length()) list.push_back(name);
        start = end + 1;
    }
    return list;
}

// Whether music on this device can disturb a conversation at the speaker: one of
// SPOTIFY_ROOM_SPEAKERS, or without that list every speaker at home (not phones, tablets or
// computers: they may be anywhere, often with headphones).
bool inRoom(const String& device, const String& type)
{
    const std::vector<String> room = roomSpeakers();
    if (room.empty()) return type != "Smartphone" && type != "Tablet" && type != "Computer";
    for (const String& name : room)
        if (name == lower(device)) return true;
    return false;
}

// The device to play on: the named one, else the one playing, else the first in the assistant's room.
const Device* target(const std::vector<Device>& devices, const String& wanted, String& error)
{
    const Device* device = nullptr;
    if (wanted.length()) {
        device = findDevice(devices, wanted, error);
    } else {
        for (const Device& d : devices)
            if (d.active) device = &d;
        const std::vector<String> room = roomSpeakers();
        if (!device && room.size()) device = findDevice(devices, room[0], error);
        if (!device && error.isEmpty())
            error = "Error: nothing is playing anywhere; ask the user on which speaker. Visible: " +
                    deviceNames(devices) + "." + NOT_VISIBLE;
    }
    if (device && device->restricted) {
        error = "Error: " + device->name + " cannot be controlled through Spotify's Web API.";
        return nullptr;
    }
    return device;
}

// "?device_id=..." for a named speaker, "" for the active one; false with `error`.
bool deviceQuery(const Session& session, const String& speaker, String& query, String& name, String& error)
{
    if (speaker.isEmpty()) return true;
    std::vector<Device> devices;
    if (!loadDevices(session, devices, error)) return false;
    const Device* device = findDevice(devices, speaker, error);
    if (!device) return false;
    query = "device_id=" + device->id;
    name = device->name;
    return true;
}

String withQuery(const String& path, const String& query)
{
    if (query.isEmpty()) return path;
    return path + (path.indexOf('?') >= 0 ? "&" : "?") + query;
}

// --- Ducking while the assistant talks ------------------------------------------------------------

struct Ducked {
    String refresh, device;
    int volume, original;
};
std::vector<Ducked> ducked;
SemaphoreHandle_t duckLock = xSemaphoreCreateMutex();
bool conversationOpen = false;

std::vector<Session> allSessions()
{
    std::vector<Session> list;
    if (*SPOTIFY_REFRESH_TOKEN) list.push_back({SPOTIFY_REFRESH_TOKEN, people::admin(), ""});
    for (const Account& account : OTHERS)
        if (*account.refresh) list.push_back({account.refresh, "", account.person});
    return list;
}

// Someone set a volume himself: it is not put back after the conversation.
void forgetDuck(const String& device)
{
    xSemaphoreTake(duckLock, portMAX_DELAY);
    for (size_t i = ducked.size(); i-- > 0;)
        if (device.isEmpty() || ducked[i].device == device) ducked.erase(ducked.begin() + i);
    xSemaphoreGive(duckLock);
}

void duck()
{
    for (const Session& session : allSessions()) {
        JsonDocument state(PsramJson::instance());
        String error;
        if (!apiJson(session, "/me/player", state, error) || !(state["is_playing"] | false)) continue;
        JsonObjectConst device = state["device"];
        const int volume = device["volume_percent"] | -1;
        const String id = device["id"] | "";
        if (!inRoom(device["name"] | "", device["type"] | "")) continue;  // music elsewhere does not disturb
        if (!(device["supports_volume"] | false) || id.isEmpty() || volume <= SPOTIFY_DUCK_VOLUME) continue;
        error = apiDo(session, "PUT", "/me/player/volume?volume_percent=" + String(SPOTIFY_DUCK_VOLUME) + "&device_id=" + id);
        Serial.printf("[Spotify] %s turned down from %d%%%s\n", device["name"] | "?", volume, error.c_str());
        if (error.isEmpty()) {
            xSemaphoreTake(duckLock, portMAX_DELAY);
            ducked.push_back({session.refresh, id, SPOTIFY_DUCK_VOLUME, volume});
            xSemaphoreGive(duckLock);
        }
    }
}

void restore()
{
    xSemaphoreTake(duckLock, portMAX_DELAY);
    std::vector<Ducked> list = ducked;
    ducked.clear();
    xSemaphoreGive(duckLock);
    for (const Ducked& d : list) {
        const Session session{d.refresh, "", ""};
        JsonDocument state(PsramJson::instance());
        String error;
        if (!apiJson(session, "/me/player", state, error)) continue;
        // Only where the music still plays as the assistant left it.
        if (String(state["device"]["id"] | "") != d.device || (state["device"]["volume_percent"] | -1) != d.volume) continue;
        error = apiDo(session, "PUT", "/me/player/volume?volume_percent=" + String(d.original) + "&device_id=" + d.device);
        Serial.printf("[Spotify] volume back to %d%%%s\n", d.original, error.c_str());
    }
}

// --- Search and lookups -----------------------------------------------------------------

// The first result of a search for `type` (Spotify returns null for items it hides), or null.
JsonObjectConst searchFirst(const Session& session, const String& query, const String& type, JsonDocument& doc,
                            String& error)
{
    if (!apiJson(session, "/search?q=" + urlEncode(query) + "&type=" + type + "&limit=5", doc, error))
        return JsonObjectConst();
    for (JsonObjectConst item : doc[type + "s"]["items"].as<JsonArrayConst>())
        if (!item.isNull()) return item;
    error = "Error: nothing found on Spotify for " + type + " \"" + query + "\".";
    return JsonObjectConst();
}

// The track or episode playing now (uri, line), false with `error` if nothing plays.
bool current(const Session& session, String& uri, String& line, String& error)
{
    JsonDocument doc(PsramJson::instance());
    if (!apiJson(session, "/me/player/currently-playing?additional_types=episode", doc, error)) return false;
    JsonObjectConst item = doc["item"];
    if (item.isNull()) {
        error = "Error: nothing is playing right now.";
        return false;
    }
    uri = item["uri"] | "";
    line = describe(item);
    return true;
}

// Song names ("Artist - Title", "current" = the song playing, or Spotify URIs) to track URIs;
// misses are listed.
void resolveTracks(const Session& session, JsonArrayConst wanted, std::vector<String>& uris, String& found,
                   String& missed)
{
    size_t count = 0;
    for (JsonVariantConst entry : wanted) {
        if (++count > SPOTIFY_LIST_TRACKS) {
            missed += (missed.length() ? ", " : "") + String("(more than ") + SPOTIFY_LIST_TRACKS + " songs)";
            break;
        }
        const String name = entry | "";
        String uri, line, error;
        if (lower(name) == "current") {
            if (!current(session, uri, line, error)) uri = "";
        } else if (name.startsWith("spotify:")) {
            uri = line = name;
        } else {
            JsonDocument doc(PsramJson::instance());
            JsonObjectConst item = searchFirst(session, name, "track", doc, error);
            if (!item.isNull()) {
                uri = item["uri"] | "";
                line = describe(item);
            }
        }
        if (uri.isEmpty()) {
            missed += (missed.length() ? ", " : "") + name;
            continue;
        }
        uris.push_back(uri);
        found += (found.length() ? "; " : "") + line;
    }
}

struct Playlist {
    String id, uri, name, owner;
    bool own = false;
    int tracks = -1;
};

bool loadPlaylists(const Session& session, std::vector<Playlist>& out, String& error)
{
    const String me = userId(session, error);
    if (me.isEmpty()) return false;
    for (int page = 0; page < SPOTIFY_PLAYLIST_PAGES; ++page) {
        JsonDocument doc(PsramJson::instance());
        if (!apiJson(session, "/me/playlists?limit=50&offset=" + String(page * 50), doc, error)) return false;
        for (JsonObjectConst p : doc["items"].as<JsonArrayConst>()) {
            if (p.isNull()) continue;
            Playlist playlist;
            playlist.id = p["id"] | "";
            playlist.uri = p["uri"] | "";
            playlist.name = p["name"] | "";
            playlist.owner = p["owner"]["display_name"] | "";
            playlist.own = String(p["owner"]["id"] | "") == me || (p["collaborative"] | false);
            JsonVariantConst total = p["items"]["total"].isNull() ? p["tracks"]["total"] : p["items"]["total"];
            playlist.tracks = total | -1;
            out.push_back(playlist);
        }
        if (doc["next"].isNull()) break;
    }
    return true;
}

String playlistNames(const std::vector<Playlist>& list, bool ownOnly)
{
    String text;
    int shown = 0;
    for (const Playlist& p : list) {
        if (ownOnly && !p.own) continue;
        if (++shown > 40) {
            text += ", ...";
            break;
        }
        text += (text.length() ? ", " : "") + p.name;
    }
    return text;
}

// A playlist by name: exact (ignoring case), else the only one containing it.
const Playlist* findPlaylist(const std::vector<Playlist>& list, const String& wanted, bool ownOnly, String& error)
{
    const String key = lower(wanted);
    if (key.isEmpty()) {
        error = "Error: which playlist? " + String(ownOnly ? "Own" : "Known") + " playlists: " + playlistNames(list, ownOnly);
        return nullptr;
    }
    const Playlist* hit = nullptr;
    int hits = 0;
    for (const Playlist& p : list) {
        if (ownOnly && !p.own) continue;
        if (lower(p.name) == key) return &p;
        if (lower(p.name).indexOf(key) >= 0 || wordsIn(lower(p.name), key, false)) {
            hit = &p;
            ++hits;
        }
    }
    if (hits == 1) return hit;
    error = String(hits ? "Error: several playlists match \"" : "Error: no playlist \"") + wanted + "\". " +
            (ownOnly ? "Own" : "Known") + " playlists: " + playlistNames(list, ownOnly);
    if (!hits && ownOnly)
        for (const Playlist& p : list)
            if (!p.own && lower(p.name).indexOf(key) >= 0)
                error += ". '" + p.name + "' belongs to " + p.owner + ": Spotify lets only its owner change it or read its songs.";
    return nullptr;
}

// Up to `pages` x 50 items of an own playlist into `doc["all"]` as {uri, line}.
bool playlistItems(const Session& session, const String& id, int pages, JsonDocument& out, String& error)
{
    JsonArray all = out["all"].to<JsonArray>();
    for (int page = 0; page < pages; ++page) {
        JsonDocument doc(PsramJson::instance());
        if (!apiJson(session, "/playlists/" + id + "/items?limit=50&additional_types=episode&offset=" + String(page * 50),
                     doc, error))
            return false;
        for (JsonObjectConst entry : doc["items"].as<JsonArrayConst>()) {
            JsonObjectConst item = entry["item"].isNull() ? entry["track"] : entry["item"];
            if (item.isNull()) continue;
            JsonObject row = all.add<JsonObject>();
            row["uri"] = item["uri"] | "";
            row["line"] = describe(item);
            row["name"] = lower(item["name"] | "");
            row["artists"] = lower(names(item["artists"]));
        }
        if (doc["next"].isNull()) break;
    }
    return true;
}

// --- play_music -------------------------------------------------------------------------

const char* timeRange(const String& period)
{
    if (period == "4_weeks") return "short_term";
    if (period == "all_time") return "long_term";
    return "medium_term";
}

String playMusic(JsonObjectConst arguments, const tools::Context& context)
{
    Session s;
    String error;
    if (!session(context, arguments["account"] | "", s, error)) return error;
    const String type = arguments["type"] | "track", what = arguments["what"] | "";
    const bool queue = arguments["queue"] | false;

    String contextUri, trackInContext, described, missed;
    std::vector<String> uris;
    if (type == "track" || type == "episode" || type == "album" || type == "artist" || type == "playlist" ||
        type == "show" || type == "audiobook") {
        if (what.isEmpty()) return "Error: what to play is missing";
        JsonDocument doc(PsramJson::instance());
        JsonObjectConst item = searchFirst(s, what, type, doc, error);
        if (item.isNull()) return error;
        described = describe(item);
        const String album = item["album"]["uri"] | "";
        if (type == "track" && album.length()) {
            // in its album, as the Spotify app does: "nächstes Lied" then goes on (a single track
            // left nothing to skip to and Spotify paused; found on the Echo, 16.10.2026)
            contextUri = album;
            trackInContext = item["uri"] | "";
        } else if (type == "track" || type == "episode") {
            uris.push_back(item["uri"] | "");
        } else {
            contextUri = item["uri"] | "";
        }
    } else if (type == "my_playlist") {
        std::vector<Playlist> list;
        if (!loadPlaylists(s, list, error)) return error;
        const Playlist* playlist = findPlaylist(list, what, false, error);
        if (!playlist) return error;
        contextUri = playlist->uri;
        described = "playlist '" + playlist->name + "'";
    } else if (type == "liked" || type == "top") {
        JsonDocument doc(PsramJson::instance());
        const String path = type == "liked" ? String("/me/tracks?limit=50")
                                            : "/me/top/tracks?limit=50&time_range=" + String(timeRange(arguments["period"] | ""));
        if (!apiJson(s, path, doc, error)) return error;
        for (JsonObjectConst entry : doc["items"].as<JsonArrayConst>()) {
            JsonObjectConst track = type == "liked" ? entry["track"].as<JsonObjectConst>() : entry;
            if (!track.isNull()) uris.push_back(track["uri"] | "");
        }
        if (uris.empty()) return "Error: no " + String(type == "liked" ? "liked songs" : "top songs") + " yet.";
        described = type == "liked" ? "the liked songs (" + String(uris.size()) + ")" : "the top songs (" + String(uris.size()) + ")";
    } else if (type == "tracks") {
        resolveTracks(s, arguments["tracks"], uris, described, missed);
        if (uris.empty()) return "Error: none of the songs found on Spotify" + (missed.length() ? ": " + missed : "");
        described = String(uris.size()) + " songs: " + described;
    } else {
        return "Error: unknown type";
    }

    std::vector<Device> devices;
    if (!loadDevices(s, devices, error)) return error;
    const Device* device = target(devices, arguments["speaker"] | "", error);
    if (!device) return error;
    const String on = "device_id=" + device->id;

    if (queue && trackInContext.length()) {  // a queued song goes alone, not its album
        uris.push_back(trackInContext);
        contextUri = "";
    }
    if (queue) {
        if (uris.empty()) return "Error: only songs and episodes can be queued, not a whole " + type;
        for (const String& uri : uris) {
            error = apiDo(s, "POST", "/me/player/queue?uri=" + urlEncode(uri) + "&" + on);
            if (error.length()) return error;
        }
        return "Queued on " + device->name + ": " + described + suffix(s) + (missed.length() ? ". Not found: " + missed : "");
    }

    // > 0: the strict schema makes the model send "volume": 0 for "no change" (that silenced a
    // speaker on the Echo, 16.10.2026)
    const int volume = arguments["volume"] | -1;
    if (volume > 0 && device->volumeOk) {
        apiDo(s, "PUT", "/me/player/volume?volume_percent=" + String(constrain(volume, 0, 100)) + "&" + on);
        forgetDuck(device->id);
    }
    JsonDocument body;
    if (contextUri.length()) {
        body["context_uri"] = contextUri;
        if (trackInContext.length()) body["offset"]["uri"] = trackInContext;
    } else {
        for (const String& uri : uris) body["uris"].add(uri);
    }
    String json;
    serializeJson(body, json);
    PsramSink reply(64 * 1024);
    int status = api(s, "PUT", "/me/player/play?" + on, json, reply, error);
    if (status == 404 || status == 502) {  // an idle device: wake it with a transfer, then again
        error = "";
        apiDo(s, "PUT", "/me/player", "{\"device_ids\":[\"" + device->id + "\"],\"play\":false}");
        delay(SPOTIFY_SETTLE_MS);
        status = api(s, "PUT", "/me/player/play?" + on, json, reply, error);
    }
    if (status < 200 || status >= 300) return error;
    String extra;
    if (arguments["shuffle"].is<bool>()) {
        const bool shuffle = arguments["shuffle"];
        const String failed = apiDo(s, "PUT", "/me/player/shuffle?state=" + String(shuffle ? "true" : "false") + "&" + on);
        extra = failed.length() ? " (shuffle not set: " + failed + ")" : shuffle ? " in shuffle" : "";
    }
    return "Playing on " + device->name + ": " + described + extra + suffix(s) + (missed.length() ? ". Not found: " + missed : "");
}

// --- music_control ----------------------------------------------------------------------

// What plays where, for the model ("Nothing is playing" when Spotify knows no playback).
String state(const Session& s)
{
    JsonDocument doc(PsramJson::instance());
    String error;
    int status = 0;
    if (!apiJson(s, "GET", "/me/player?additional_types=episode", "", doc, error, &status)) return error;
    if (status == 204 || doc["item"].isNull()) return "Nothing is playing" + suffix(s) + ".";
    JsonObjectConst item = doc["item"];
    JsonObjectConst device = doc["device"];
    String text = String((doc["is_playing"] | false) ? "Playing" : "Paused") + ": " + describe(item);
    if (!item["album"].isNull()) text += ", album '" + String(item["album"]["name"] | "") + "'";
    text += ", " + clock(doc["progress_ms"] | 0) + " of " + clock(item["duration_ms"] | 0);
    text += ", on " + String(device["name"] | "?") + " (" + String(device["type"] | "") +
            (device["volume_percent"].is<int>() ? ", volume " + String(device["volume_percent"].as<int>()) + "%" : "") + ")";
    text += ", shuffle " + String((doc["shuffle_state"] | false) ? "on" : "off") + ", repeat " + String(doc["repeat_state"] | "off");
    const String contextType = doc["context"]["type"] | "";
    const String contextUri = doc["context"]["uri"] | "";
    if (contextType == "playlist") {
        JsonDocument playlist(PsramJson::instance());
        const String id = contextUri.substring(contextUri.lastIndexOf(':') + 1);
        if (apiJson(s, "/playlists/" + id + "?fields=name", playlist, error))
            text += ", from playlist '" + String(playlist["name"] | "") + "'";
    } else if (contextType.length()) {
        text += ", from the " + contextType;
    }
    const String link = item["external_urls"]["spotify"] | "";
    if (link.length()) text += ". Link: " + link;
    return text + suffix(s);
}

String musicControl(JsonObjectConst arguments, const tools::Context& context)
{
    Session s;
    String error;
    if (!session(context, arguments["account"] | "", s, error)) return error;
    const String action = arguments["action"] | "status";
    String query, deviceName;
    if (!deviceQuery(s, arguments["speaker"] | "", query, deviceName, error)) return error;

    if (action == "status") return state(s);
    if (action == "queue") {
        JsonDocument doc(PsramJson::instance());
        if (!apiJson(s, "/me/player/queue", doc, error)) return error;
        if (doc["currently_playing"].isNull()) return "Nothing is playing" + suffix(s) + ".";
        String text = "Now: " + describe(doc["currently_playing"]) + ". Next:";
        int shown = 0;
        for (JsonObjectConst item : doc["queue"].as<JsonArrayConst>()) {
            if (++shown > 10) break;
            text += "\n" + String(shown) + ". " + describe(item);
        }
        return shown ? text : text + " nothing queued";
    }
    if (action == "pause" || action == "resume") {
        error = apiDo(s, "PUT", withQuery(action == "pause" ? "/me/player/pause" : "/me/player/play", query));
        return error.length() ? error : String(action == "pause" ? "Paused" : "Playing again") + suffix(s);
    }
    if (action == "next" || action == "previous") {
        int times = 1;
        if (action == "previous") {
            // A speaker goes back to the start of the song once it played more than ~3 s; "der
            // vorherige Titel" means the one before (it took two "zurück", found on the Echo).
            JsonDocument now(PsramJson::instance());
            String ignored;
            if (apiJson(s, "GET", "/me/player", "", now, ignored) && (now["progress_ms"] | 0) > 3000) times = 2;
        }
        for (int i = 0; i < times; ++i) {
            if (i) delay(400);
            error = apiDo(s, "POST", withQuery("/me/player/" + action, query));
            if (error.length()) return error;
        }
        delay(SPOTIFY_SETTLE_MS);
        return state(s);
    }
    if (action == "shuffle") {
        const bool on = arguments["on"] | true;
        error = apiDo(s, "PUT", withQuery("/me/player/shuffle?state=" + String(on ? "true" : "false"), query));
        return error.length() ? error : String("Shuffle ") + (on ? "on" : "off");
    }
    if (action == "repeat") {
        const String mode = arguments["mode"] | "context";
        if (mode != "off" && mode != "track" && mode != "context") return "Error: mode is off, track or context";
        error = apiDo(s, "PUT", withQuery("/me/player/repeat?state=" + mode, query));
        return error.length() ? error : "Repeat " + mode;
    }
    if (action != "seek" && action != "volume") return "Error: unknown action";

    JsonDocument doc(PsramJson::instance());
    int status = 0;
    if (!apiJson(s, "GET", "/me/player", "", doc, error, &status)) return error;
    if (status == 204 || doc["device"].isNull()) return "Error: nothing is playing" + suffix(s) + ".";
    if (action == "seek") {
        int64_t to = int64_t(doc["progress_ms"] | 0) + int64_t(arguments["seconds"] | 0) * 1000;
        if (to < 0) to = 0;
        error = apiDo(s, "PUT", withQuery("/me/player/seek?position_ms=" + String(int32_t(to)), query));
        return error.length() ? error : "Now at " + clock(to);
    }
    const String device = query.length() ? query.substring(10) : String(doc["device"]["id"] | "");
    if (query.isEmpty() && !(doc["device"]["supports_volume"] | false))
        return "Error: " + String(doc["device"]["name"] | "this speaker") + " does not let Spotify set its volume.";
    const int now = doc["device"]["volume_percent"] | 50;
    // A step wins over a level: the model sends both ("level": 0, "step": 10 for "lauter" set the
    // volume to 0 on the Echo, 16.10.2026).
    const int step = arguments["step"] | 0;
    const int level = constrain(step != 0 ? now + step : int(arguments["level"] | now), 0, 100);
    error = apiDo(s, "PUT", "/me/player/volume?volume_percent=" + String(level) + "&device_id=" + device);
    forgetDuck(device);
    return error.length() ? error : "Volume " + String(level) + "% on " + (deviceName.length() ? deviceName : String(doc["device"]["name"] | ""));
}

// --- music_speakers ---------------------------------------------------------------------

String musicSpeakers(JsonObjectConst arguments, const tools::Context& context)
{
    Session s;
    String error;
    if (!session(context, arguments["account"] | "", s, error)) return error;
    const String action = arguments["action"] | "list";
    std::vector<Device> devices;
    if (!loadDevices(s, devices, error)) return error;
    if (action == "list") {
        String text = "Speakers Spotify sees" + suffix(s) + ":";
        for (const Device& d : devices) {
            text += "\n- " + d.name + " (" + d.type + (d.active ? ", playing" : "") +
                    (d.volume >= 0 ? ", volume " + String(d.volume) + "%" : "") + (d.restricted ? ", not controllable" : "") + ")";
        }
        return (devices.empty() ? text + " none." : text) + NOT_VISIBLE;
    }
    const Device* device = findDevice(devices, arguments["speaker"] | "", error);
    if (!device) return error;
    if (action == "move") {
        error = apiDo(s, "PUT", "/me/player", "{\"device_ids\":[\"" + device->id + "\"],\"play\":true}");
        if (error.indexOf("404") >= 0) return "Error: nothing to move; start something with play_music.";
        return error.length() ? error : "Playback moved to " + device->name;
    }
    if (action == "volume") {
        if (!arguments["level"].is<int>()) return "Error: level missing";
        if (!device->volumeOk) return "Error: " + device->name + " does not let Spotify set its volume.";
        const int level = constrain(arguments["level"].as<int>(), 0, 100);
        error = apiDo(s, "PUT", "/me/player/volume?volume_percent=" + String(level) + "&device_id=" + device->id);
        forgetDuck(device->id);
        return error.length() ? error : "Volume " + String(level) + "% on " + device->name;
    }
    return "Error: action is list, move or volume";
}

// --- music_playlist ---------------------------------------------------------------------

String addToPlaylist(const Session& s, const Playlist& playlist, JsonArrayConst tracks)
{
    std::vector<String> uris;
    String found, missed, error;
    resolveTracks(s, tracks, uris, found, missed);
    if (uris.empty()) return "Error: no song found" + (missed.length() ? ": " + missed : "");
    JsonDocument existing(PsramJson::instance());
    if (!playlistItems(s, playlist.id, 4, existing, error)) return error;
    JsonDocument body;
    String skipped;
    for (const String& uri : uris) {
        bool there = false;
        for (JsonObjectConst row : existing["all"].as<JsonArrayConst>())
            if (uri == String(row["uri"] | "")) there = true;
        if (there) skipped += (skipped.length() ? ", " : "") + uri;
        else body["uris"].add(uri);
    }
    if (body["uris"].isNull()) return "Already in '" + playlist.name + "': " + found;
    String json;
    serializeJson(body, json);
    error = apiDo(s, "POST", "/playlists/" + playlist.id + "/items", json);
    if (error.length()) return error;
    return "Added to '" + playlist.name + "': " + found + (skipped.length() ? ". Some were already in it" : "") +
           (missed.length() ? ". Not found: " + missed : "") + suffix(s);
}

String musicPlaylist(JsonObjectConst arguments, const tools::Context& context)
{
    Session s;
    String error;
    if (!session(context, arguments["account"] | "", s, error)) return error;
    const String action = arguments["action"] | "list";

    if (action == "create") {
        const String name = arguments["name"] | "";
        if (name.isEmpty()) return "Error: name missing";
        JsonDocument body, made(PsramJson::instance());
        body["name"] = name;
        body["description"] = arguments["description"] | "";
        body["public"] = arguments["public"] | false;
        String json;
        serializeJson(body, json);
        if (!apiJson(s, "POST", "/me/playlists", json, made, error)) return error;
        Playlist playlist;
        playlist.id = made["id"] | "";
        playlist.name = name;
        const String link = made["external_urls"]["spotify"] | "";
        String text = "Created playlist '" + name + "'" + suffix(s) + (link.length() ? ". Link: " + link : "");
        if (arguments["tracks"].as<JsonArrayConst>().size()) text += ". " + addToPlaylist(s, playlist, arguments["tracks"]);
        return text;
    }

    std::vector<Playlist> list;
    if (!loadPlaylists(s, list, error)) return error;
    if (action == "list") {
        String own, followed;
        for (const Playlist& p : list) {
            String& to = p.own ? own : followed;
            to += (to.length() ? "; " : "") + p.name + (p.tracks >= 0 ? " (" + String(p.tracks) + ")" : "") +
                  (p.own ? "" : " by " + p.owner);
        }
        return "Own playlists" + suffix(s) + ": " + (own.length() ? own : String("none")) +
               "\nFollowed (play only, songs not readable): " + (followed.length() ? followed : String("none"));
    }
    const Playlist* playlist = findPlaylist(list, arguments["playlist"] | "", true, error);
    if (!playlist) return error;

    if (action == "show") {
        JsonDocument items(PsramJson::instance());
        if (!playlistItems(s, playlist->id, 2, items, error)) return error;
        String text = "'" + playlist->name + "'" + (playlist->tracks >= 0 ? " (" + String(playlist->tracks) + " songs)" : "") + ":";
        int number = 0;
        for (JsonObjectConst row : items["all"].as<JsonArrayConst>()) text += "\n" + String(++number) + ". " + String(row["line"] | "");
        return number < playlist->tracks ? text + "\n..." : text;
    }
    if (action == "add") return addToPlaylist(s, *playlist, arguments["tracks"]);
    if (action == "remove") {
        JsonDocument items(PsramJson::instance());
        if (!playlistItems(s, playlist->id, 10, items, error)) return error;
        JsonDocument body;
        String removed, missed;
        for (JsonVariantConst entry : arguments["tracks"].as<JsonArrayConst>()) {
            String wanted = lower(entry | ""), uri, line;
            if (wanted == "current" && !current(s, uri, line, error)) return error;
            // "Artist - Title": the title must match, the artist too if given.
            String artist;
            const int dash = wanted.indexOf(" - ");
            if (dash > 0) {
                artist = wanted.substring(0, dash);
                wanted = wanted.substring(dash + 3);
            }
            bool hit = false;
            for (JsonObjectConst row : items["all"].as<JsonArrayConst>()) {
                const String name = row["name"] | "", artists = row["artists"] | "";
                const bool match = uri.length() ? uri == String(row["uri"] | "")
                                                : name.indexOf(wanted) >= 0 && (artist.isEmpty() || artists.indexOf(artist) >= 0);
                if (!match) continue;
                body["items"].add<JsonObject>()["uri"] = row["uri"];
                removed += (removed.length() ? "; " : "") + String(row["line"] | "");
                hit = true;
                break;
            }
            if (!hit) missed += (missed.length() ? ", " : "") + String(entry | "");
        }
        if (body["items"].isNull()) return "Error: not in '" + playlist->name + "': " + missed;
        String json;
        serializeJson(body, json);
        error = apiDo(s, "DELETE", "/playlists/" + playlist->id + "/items", json);
        return error.length() ? error : "Removed from '" + playlist->name + "': " + removed + (missed.length() ? ". Not in it: " + missed : "");
    }
    if (action == "rename") {
        JsonDocument body;
        if ((arguments["name"] | "")[0]) body["name"] = arguments["name"];
        if (arguments["description"].is<const char*>()) body["description"] = arguments["description"];
        if (arguments["public"].is<bool>()) body["public"] = arguments["public"];
        if (body.isNull() || body.size() == 0) return "Error: nothing to change";
        String json;
        serializeJson(body, json);
        error = apiDo(s, "PUT", "/playlists/" + playlist->id, json);
        return error.length() ? error : "Changed playlist '" + playlist->name + "'";
    }
    if (action == "delete") {
        error = apiDo(s, "DELETE", "/me/library?uris=" + urlEncode(playlist->uri));
        return error.length() ? error : "Deleted playlist '" + playlist->name + "' (removed from the library)" + suffix(s);
    }
    return "Error: unknown action";
}

// --- music_library ----------------------------------------------------------------------

String musicLibrary(JsonObjectConst arguments, const tools::Context& context)
{
    Session s;
    String error;
    if (!session(context, arguments["account"] | "", s, error)) return error;
    const String action = arguments["action"] | "list", type = arguments["type"] | "track", what = arguments["what"] | "";

    if (action == "list") {
        JsonDocument doc(PsramJson::instance());
        String path = "/me/" + type + "s?limit=20";
        if (type == "artist") path = "/me/following?type=artist&limit=20";
        if (type == "playlist") return "Error: playlists come from music_playlist list";
        if (!apiJson(s, path, doc, error)) {
            if (error.indexOf("scope") >= 0) error += " (scripts/spotify_auth.py has to run once more for followed artists)";
            return error;
        }
        JsonObjectConst page = type == "artist" ? doc["artists"].as<JsonObjectConst>() : doc.as<JsonObjectConst>();
        String text = "Saved " + type + "s" + suffix(s) + " (" + String(page["total"] | 0) + ", newest first):";
        for (JsonObjectConst entry : page["items"].as<JsonArrayConst>()) {
            JsonObjectConst item = entry[type].isNull() ? entry : entry[type].as<JsonObjectConst>();
            text += "\n- " + describe(item);
        }
        return text;
    }

    String uri, line;
    if (what.isEmpty() || lower(what) == "current") {
        if (!current(s, uri, line, error)) return error;
    } else if (type == "playlist") {
        std::vector<Playlist> list;
        if (!loadPlaylists(s, list, error)) return error;
        const Playlist* playlist = findPlaylist(list, what, false, error);
        if (playlist) {
            uri = playlist->uri;
            line = "playlist '" + playlist->name + "'";
        } else {
            JsonDocument doc(PsramJson::instance());
            JsonObjectConst item = searchFirst(s, what, type, doc, error);
            if (item.isNull()) return error;
            uri = item["uri"] | "";
            line = describe(item);
        }
    } else {
        JsonDocument doc(PsramJson::instance());
        JsonObjectConst item = searchFirst(s, what, type, doc, error);
        if (item.isNull()) return error;
        uri = item["uri"] | "";
        line = describe(item);
    }

    if (action == "check") {
        JsonDocument doc(PsramJson::instance());
        if (!apiJson(s, "/me/library/contains?uris=" + urlEncode(uri), doc, error)) return error;
        return line + ((doc[0] | false) ? " is in the library" : " is not in the library") + suffix(s);
    }
    if (action != "like" && action != "unlike") return "Error: action is like, unlike, check or list";
    error = apiDo(s, action == "like" ? "PUT" : "DELETE", "/me/library?uris=" + urlEncode(uri));
    if (error.indexOf("scope") >= 0) error += " (scripts/spotify_auth.py has to run once more for following artists)";
    return error.length() ? error : (action == "like" ? "Saved to the library: " : "Removed from the library: ") + line + suffix(s);
}

// --- music_insights ---------------------------------------------------------------------

// Seconds since 1970 of "YYYY-MM-DDTHH:MM:SS..." in UTC (days from the civil calendar).
time_t utcTime(const char* stamp)
{
    int y, mo, d, h, mi, se;
    if (sscanf(stamp, "%d-%d-%dT%d:%d:%d", &y, &mo, &d, &h, &mi, &se) != 6) return 0;
    y -= mo <= 2;
    const int era = y / 400, yoe = y - era * 400;
    const int doy = (153 * (mo + (mo > 2 ? -3 : 9)) + 2) / 5 + d - 1;
    const int days = era * 146097 + yoe * 365 + yoe / 4 - yoe / 100 + doy - 719468;
    return time_t(days) * 86400 + h * 3600 + mi * 60 + se;
}

String ago(const char* stamp)
{
    const time_t then = utcTime(stamp), now = time(nullptr);
    if (!then || now < then) return "";
    const long minutes = long(now - then) / 60;
    if (minutes < 60) return String(minutes) + " min ago";
    if (minutes < 48 * 60) return String(minutes / 60) + " h ago";
    return String(minutes / 1440) + " days ago";
}

String musicInsights(JsonObjectConst arguments, const tools::Context& context)
{
    Session s;
    String error;
    if (!session(context, arguments["account"] | "", s, error)) return error;
    const String action = arguments["action"] | "", what = arguments["what"] | "";
    JsonDocument doc(PsramJson::instance());
    String text;

    if (action == "top_tracks" || action == "top_artists") {
        const String period = arguments["period"] | "6_months";
        if (!apiJson(s, "/me/" + String(action == "top_tracks" ? "top/tracks" : "top/artists") + "?limit=20&time_range=" +
                            timeRange(period), doc, error))
            return error;
        text = String(action == "top_tracks" ? "Top songs" : "Top artists") + " (" + period + ")" + suffix(s) + ":";
        int rank = 0;
        for (JsonObjectConst item : doc["items"].as<JsonArrayConst>()) text += "\n" + String(++rank) + ". " + describe(item);
        return rank ? text : text + " none yet";
    }
    if (action == "recent") {
        if (!apiJson(s, "/me/player/recently-played?limit=20", doc, error)) return error;
        text = "Recently played" + suffix(s) + ":";
        for (JsonObjectConst entry : doc["items"].as<JsonArrayConst>())
            text += "\n- " + describe(entry["track"]) + " (" + ago(entry["played_at"] | "") + ")";
        return text;
    }
    if (what.isEmpty()) return "Error: what missing (an artist, album or podcast)";
    JsonDocument found(PsramJson::instance());
    if (action == "artist_albums") {
        JsonObjectConst artist = searchFirst(s, what, "artist", found, error);
        if (artist.isNull()) return error;
        if (!apiJson(s, "/artists/" + String(artist["id"] | "") + "/albums?include_groups=album,single&limit=10", doc, error))
            return error;
        text = "Albums and singles of " + String(artist["name"] | "") + " (" + String(doc["total"] | 0) + ", the 10 newest):";
        for (JsonObjectConst album : doc["items"].as<JsonArrayConst>())
            text += "\n- " + String(album["name"] | "") + " (" + String(album["album_type"] | "") + ", " +
                    String(album["release_date"] | "").substring(0, 4) + ")";
        return text;
    }
    if (action == "album_tracks") {
        JsonObjectConst album = searchFirst(s, what, "album", found, error);
        if (album.isNull()) return error;
        if (!apiJson(s, "/albums/" + String(album["id"] | "") + "/tracks?limit=50", doc, error)) return error;
        text = describe(album) + " (" + String(album["release_date"] | "").substring(0, 4) + "):";
        for (JsonObjectConst track : doc["items"].as<JsonArrayConst>())
            text += "\n" + String(track["track_number"] | 0) + ". " + String(track["name"] | "") + " (" + clock(track["duration_ms"] | 0) + ")";
        return text;
    }
    if (action == "show_episodes") {
        JsonObjectConst show = searchFirst(s, what, "show", found, error);
        if (show.isNull()) return error;
        if (!apiJson(s, "/shows/" + String(show["id"] | "") + "/episodes?limit=5", doc, error)) return error;
        text = "Newest episodes of " + describe(show) + ":";
        for (JsonObjectConst episode : doc["items"].as<JsonArrayConst>()) {
            if (episode.isNull()) continue;
            text += "\n- " + String(episode["release_date"] | "") + ": " + String(episode["name"] | "") + " (" +
                    String(int(episode["duration_ms"] | 0) / 60000) + " min" +
                    ((episode["resume_point"]["fully_played"] | false) ? ", heard" : "") + ")";
        }
        return text;
    }
    return "Error: unknown action";
}

}  // namespace

bool configured() { return *SPOTIFY_CLIENT_ID && *SPOTIFY_CLIENT_SECRET && *SPOTIFY_REFRESH_TOKEN; }

void setConversation(bool open)
{
    if (!configured() || SPOTIFY_DUCK_VOLUME <= 0 || open == conversationOpen) return;
    conversationOpen = open;
    if (!tools::post(open ? duck : restore)) Serial.println("[Spotify] worker busy, volume left as it is");
}

void addTools()
{
    if (!configured()) return;
    using tools::Tool;
    // All slow (TLS to Spotify, a few hundred ms per request): in a conversation they run in the registry's worker.
    tools::add(Tool{
        "play_music",
        "Plays music, podcasts or audiobooks from Spotify on a speaker at home or a phone (Spotify Connect: Echos, "
        "phones with the Spotify app open, ...). type: track, album, artist, playlist (search), my_playlist (an own or "
        "followed playlist by name), liked (liked songs), top (the user's most played songs, period), show (podcast), "
        "episode, audiobook, or tracks: a list you put together yourself ('Artist - Title', up to 15) for 'something "
        "like X', a mood or an occasion; Spotify has no recommendations, use your own music knowledge (music_insights "
        "shows the user's taste). speaker: name or room, '' = where music plays now; if nothing plays anywhere ask "
        "where. queue: add songs to the queue instead of playing now. volume: set before it starts (e.g. quiet for "
        "waking up). Spotify's own mixes (Mix der Woche, Daily Mix, Release Radar) and radio stations are not "
        "reachable here: use alexa on an Echo for those.",
        R"json({"type": "object", "additionalProperties": false,
            "properties": {"what": {"type": "string", "description": "search words or playlist name, e.g. 'Daft Punk Get Lucky'; '' for liked/top/tracks"},
                           "type": {"type": "string", "enum": ["track", "album", "artist", "playlist", "my_playlist", "liked", "top", "show", "episode", "audiobook", "tracks"]},
                           "tracks": {"type": "array", "items": {"type": "string"}, "description": "type tracks: 'Artist - Title' each"},
                           "speaker": {"type": "string", "description": "e.g. 'Echo Wohnzimmer', 'Küche', 'Handy'; '' = current"},
                           "shuffle": {"type": "boolean"},
                           "queue": {"type": "boolean"},
                           "volume": {"type": "integer", "minimum": 0, "maximum": 100},
                           "period": {"type": "string", "enum": ["4_weeks", "6_months", "all_time"], "description": "type top"},
                           "account": {"type": "string", "description": "whose Spotify: a person's name, '' = the person talking (else the admin's)"}},
            "required": ["what", "type", "speaker"]})json",
        playMusic, tools::Slow, tools::Everywhere});
    tools::add(Tool{
        "music_control",
        "Controls Spotify playback: status (what plays where, progress, volume, a link to share), pause, resume, next, "
        "previous, seek (seconds relative, e.g. -30), volume (level or step), shuffle (on), repeat (mode), queue (what "
        "comes next). speaker: only to control a specific device, '' = the one playing. While Spotify plays, "
        "pause, skip and volume go here, not to alexa. A sleep timer is create_task with the instruction to pause.",
        R"json({"type": "object", "additionalProperties": false,
            "properties": {"action": {"type": "string", "enum": ["status", "pause", "resume", "next", "previous", "seek", "volume", "shuffle", "repeat", "queue"]},
                           "seconds": {"type": "integer"},
                           "level": {"type": "integer", "minimum": 0, "maximum": 100},
                           "step": {"type": "integer", "minimum": -100, "maximum": 100},
                           "on": {"type": "boolean"},
                           "mode": {"type": "string", "enum": ["off", "track", "context"]},
                           "speaker": {"type": "string"},
                           "account": {"type": "string", "description": "whose Spotify: a person's name, '' = the person talking (else the admin's)"}},
            "required": ["action"]})json",
        musicControl, tools::Slow, tools::Everywhere});
    tools::add(Tool{
        "music_speakers",
        "Spotify's speakers: list (all devices Spotify sees, which one plays, volumes), move (the music continues on "
        "another speaker, also an Echo: 'mach die Musik in der Küche an', 'spiel auf dem Echo im Arbeitszimmer weiter'; "
        "not alexa), volume (of one speaker).",
        R"json({"type": "object", "additionalProperties": false,
            "properties": {"action": {"type": "string", "enum": ["list", "move", "volume"]},
                           "speaker": {"type": "string"},
                           "level": {"type": "integer", "minimum": 0, "maximum": 100},
                           "account": {"type": "string", "description": "whose Spotify: a person's name, '' = the person talking (else the admin's)"}},
            "required": ["action"]})json",
        musicSpeakers, tools::Slow, tools::Everywhere});
    tools::add(Tool{
        "music_playlist",
        "Manages the user's Spotify playlists: list (own and followed), show (songs of an own playlist), create (name, "
        "optional description and tracks), add / remove tracks ('Artist - Title' or 'current' = the song playing now), "
        "rename (new name and/or description), delete (after the user confirmed). Only own playlists can be changed "
        "or read; followed ones can be played.",
        R"json({"type": "object", "additionalProperties": false,
            "properties": {"action": {"type": "string", "enum": ["list", "show", "create", "add", "remove", "rename", "delete"]},
                           "playlist": {"type": "string", "description": "name of an existing playlist"},
                           "name": {"type": "string", "description": "create: its name; rename: the new name"},
                           "description": {"type": "string"},
                           "public": {"type": "boolean", "description": "default private"},
                           "tracks": {"type": "array", "items": {"type": "string"}, "description": "'Artist - Title' or 'current'"},
                           "account": {"type": "string", "description": "whose Spotify: a person's name, '' = the person talking (else the admin's)"}},
            "required": ["action"]})json",
        musicPlaylist, tools::Slow, tools::Everywhere});
    tools::add(Tool{
        "music_library",
        "The user's Spotify library: like / unlike (save or remove a song, album, podcast, episode, audiobook, follow "
        "an artist or playlist; what 'current' = the song playing now), check (is it saved?), list (saved items of a "
        "type, newest first).",
        R"json({"type": "object", "additionalProperties": false,
            "properties": {"action": {"type": "string", "enum": ["like", "unlike", "check", "list"]},
                           "type": {"type": "string", "enum": ["track", "album", "artist", "playlist", "show", "episode", "audiobook"]},
                           "what": {"type": "string", "description": "search words or 'current'"},
                           "account": {"type": "string", "description": "whose Spotify: a person's name, '' = the person talking (else the admin's)"}},
            "required": ["action", "type"]})json",
        musicLibrary, tools::Slow, tools::Everywhere});
    tools::add(Tool{
        "music_insights",
        "What the user listens to and catalog facts: top_tracks / top_artists (period), recent (what played lately, "
        "'what was that song earlier?'), artist_albums (an artist's albums and singles with years), album_tracks, "
        "show_episodes (a podcast's newest episodes, whether heard). Spotify gives no genres, popularity or similar "
        "artists any more: use your own knowledge for those.",
        R"json({"type": "object", "additionalProperties": false,
            "properties": {"action": {"type": "string", "enum": ["top_tracks", "top_artists", "recent", "artist_albums", "album_tracks", "show_episodes"]},
                           "period": {"type": "string", "enum": ["4_weeks", "6_months", "all_time"]},
                           "what": {"type": "string", "description": "artist, album or podcast name"},
                           "account": {"type": "string", "description": "whose Spotify: a person's name, '' = the person talking (else the admin's)"}},
            "required": ["action"]})json",
        musicInsights, tools::Slow, tools::Everywhere});
}

}  // namespace spotify
