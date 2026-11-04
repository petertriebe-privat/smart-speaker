#include "services/google.h"

#include <ArduinoJson.h>
#include <HTTPClient.h>
#include <Preferences.h>
#include <algorithm>
#include <atomic>
#include <esp_random.h>
#include <mbedtls/base64.h>
#include <mbedtls/sha256.h>
#include <functional>
#include <time.h>
#include <vector>

#include "assistant/agenda.h"
#include "assistant/tools.h"
#include "ca_certs.h"
#include "config.h"
#include "secrets.h"
#include "services/https.h"
#include "services/psram_json.h"
#include "services/psram_sink.h"
#include "services/telegram.h"
#include "services/whatsapp.h"
#include "system/error_log.h"
#include "system/time_util.h"
#include "system/tls_gate.h"
#include "system/utf8.h"
#include "voice/live_session.h"

// The web client for renewing the login through the relay (optional, see docs/google.md).
#ifndef GOOGLE_WEB_CLIENT_ID
#define GOOGLE_WEB_CLIENT_ID ""
#endif
#ifndef GOOGLE_WEB_CLIENT_SECRET
#define GOOGLE_WEB_CLIENT_SECRET ""
#endif
// A refresh token of the web client (e.g. the main Echo's), for a member (JARVIS_INBOUND 0) that never
// sends the login link itself.
#ifndef GOOGLE_REFRESH_TOKEN_WEB
#define GOOGLE_REFRESH_TOKEN_WEB ""
#endif

namespace google {
namespace {

constexpr const char* TOKEN_HOST = "oauth2.googleapis.com";
constexpr const char* CALENDAR_HOST = "www.googleapis.com";
constexpr const char* PEOPLE_HOST = "people.googleapis.com";
constexpr uint32_t TIMEOUT_MS = 20000;
constexpr int MAX_EVENTS = 50;       // per calendar and call
constexpr uint32_t CALENDARS_MS = 10 * 60 * 1000;  // the calendar list is kept this long
constexpr int MAX_FREE_SLOTS = 20;
constexpr const char* NO_CLOCK = "Error: the clock is not set yet.";
const char* const WEEKDAYS[] = {"So", "Mo", "Di", "Mi", "Do", "Fr", "Sa"};

// Access tokens per refresh token: the account's (GOOGLE_REFRESH_TOKEN) and those of mailboxes
// of other people (assistant/mail_watch). Guarded by tokenLock: tools run in several tasks
// (worker, Telegram).
struct Token {
    String refresh, access;
    bool web = false;    // issued to the web client (login link), not the desktop client
    uint32_t until = 0;  // millis()
};
std::vector<Token> tokens;
SemaphoreHandle_t tokenLock = xSemaphoreCreateMutex();

// The account's refresh token: the one from the last login link (NVS "google"), else
// GOOGLE_REFRESH_TOKEN. Loaded on first use, under tokenLock.
String account;
bool accountWeb = false, accountLoaded = false;
// The account's login expired: the admin gets a link (loginWork). Set from any task.
std::atomic<bool> loginNeeded{false};

void loadAccountLocked()
{
    if (accountLoaded) return;
    accountLoaded = true;
    Preferences prefs;
    if (prefs.begin("google", true)) account = prefs.getString("refresh", "");
    prefs.end();
    accountWeb = account.length() > 0;
    if (!accountWeb && *GOOGLE_REFRESH_TOKEN_WEB) {
        // a web-client login given in secrets.h (e.g. the main Echo's, for a member that never gets the
        // login link itself)
        account = GOOGLE_REFRESH_TOKEN_WEB;
        accountWeb = true;
    } else if (!accountWeb) {
        account = GOOGLE_REFRESH_TOKEN;
    }
}

// The login from the last link was refused: forget it, so the one in secrets.h is used (a member
// never gets a link, 25.10.2026: its login stayed expired). true if there is another one to try.
bool dropStoredAccountLocked(const String& refused)
{
    Preferences prefs;
    if (!prefs.begin("google", false)) return false;
    const bool stored = prefs.getString("refresh", "") == refused;
    if (stored) prefs.remove("refresh");
    prefs.end();
    if (!stored) return false;
    accountLoaded = false;
    loadAccountLocked();
    Serial.println("[Google] the stored login was refused: using the one from secrets.h");
    return account.length() && account != refused;
}

// Whether the admin can renew the login himself: web client and relay (its /google page) set up.
bool renewable() { return *GOOGLE_WEB_CLIENT_ID && *GOOGLE_WEB_CLIENT_SECRET && whatsapp::configured(); }

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

// --- HTTP and OAuth -------------------------------------------------------------------

// The body as bytes (it may be a PSRAM buffer over 64 KB, where an Arduino String breaks). Over
// a kept connection (https::Lease); one that the server closed meanwhile is opened anew once.
int send(const char* method, const char* host, const String& path, const char* contentType, const uint8_t* body,
         size_t length, const String& token, PsramSink& reply)
{
    https::Lease lease(host, CA_CERTS);
    https::Client& client = lease.client();
    String head = String(method) + " " + path + " HTTP/1.1\r\nHost: " + host + "\r\n";
    if (token.length()) head += "Authorization: Bearer " + token + "\r\n";
    if (length) head += String("Content-Type: ") + contentType + "\r\n";
    head += "Content-Length: " + String(length) + "\r\n\r\n";
    for (int attempt = 0;; ++attempt) {
        client.setTimeout(TIMEOUT_MS);
        if (!client.connect()) return HTTPC_ERROR_CONNECTION_REFUSED;
        int status = HTTPC_ERROR_SEND_PAYLOAD_FAILED;
        reply.size = 0;
        if (client.write(head) && (!length || client.write(body, length))) status = client.read(reply);
        if (status >= 0 || status == HTTPC_ERROR_READ_TIMEOUT || !lease.reused() || attempt) {
            if (status < 0) client.stop();
            return status;
        }
        client.stop();  // the kept connection was closed by the server: once more on a new one
    }
}

int send(const char* method, const char* host, const String& path, const char* contentType, const String& body,
         const String& token, PsramSink& reply)
{
    return send(method, host, path, contentType, reinterpret_cast<const uint8_t*>(body.c_str()), body.length(), token,
                reply);
}

String errorText(int status, const PsramSink& reply)
{
    if (status == HTTPC_ERROR_READ_TIMEOUT) return "Google does not answer";
    if (status < 0) return "Google not reachable (" + HTTPClient::errorToString(status) + ")";
    JsonDocument parsed;
    String message;
    if (reply.data && !deserializeJson(parsed, reply.data, reply.size)) {
        message = parsed["error"]["message"] | "";  // APIs
        if (message.isEmpty()) message = parsed["error_description"] | "";  // token endpoint
    }
    return "HTTP " + String(status) + (message.length() ? ": " + utf8::prefix(message, 200) : "");
}

// A valid access token for `refresh` ("" = the account's) into `token` (fetched with the refresh
// token, kept until 5 minutes before it expires).
bool authorizeLocked(Token& entry, String& error);
bool authorize(const String& refresh, String& token, String& error)
{
    xSemaphoreTake(tokenLock, portMAX_DELAY);
    loadAccountLocked();
    const String key = refresh.length() ? refresh : account;
    Token* entry = nullptr;
    for (Token& t : tokens)
        if (t.refresh == key) entry = &t;
    if (!entry) {
        tokens.emplace_back();
        entry = &tokens.back();
        entry->refresh = key;
        entry->web = key == account && accountWeb;
    }
    const bool ok = authorizeLocked(*entry, error);
    token = entry->access;
    xSemaphoreGive(tokenLock);
    return ok;
}

// Forgets `token` (revoked early: the next call fetches a new one).
void dropToken(const String& token)
{
    xSemaphoreTake(tokenLock, portMAX_DELAY);
    for (Token& t : tokens)
        if (t.access == token) t.access = "";
    xSemaphoreGive(tokenLock);
}

bool authorizeLocked(Token& entry, String& error)
{
    if (entry.access.length() && int32_t(entry.until - millis()) > 0) return true;
    const bool own = entry.refresh == account;
    const char* expired = renewable()
        ? "Error: the Google login has expired. The assistant sends the admin a link to log in again (Telegram); Google "
          "works again once he has used it. Tell the user so; there is nothing else to do."
        : "Error: the Google login has expired or was revoked; the admin has to run tools/google_auth.py on the "
          "PC and flash the device again.";
    if (entry.refresh.isEmpty()) {  // web client only, never logged in
        loginNeeded = true;
        error = expired;
        return false;
    }
    const String body = "client_id=" + urlEncode(entry.web ? GOOGLE_WEB_CLIENT_ID : GOOGLE_CLIENT_ID) +
                        "&client_secret=" + urlEncode(entry.web ? GOOGLE_WEB_CLIENT_SECRET : GOOGLE_CLIENT_SECRET) +
                        "&refresh_token=" + urlEncode(entry.refresh) + "&grant_type=refresh_token";
    PsramSink reply(16 * 1024);
    const int status = send("POST", TOKEN_HOST, "/token", "application/x-www-form-urlencoded", body, "", reply);
    JsonDocument parsed;
    if (status != 200 || deserializeJson(parsed, reply.data, reply.size) || !parsed["access_token"].is<const char*>()) {
        if (status == 400 && reply.data && strstr(reinterpret_cast<const char*>(reply.data), "invalid_grant")) {
            if (own && dropStoredAccountLocked(entry.refresh)) {
                entry.refresh = account;  // once more with the fallback
                entry.web = accountWeb;
                return authorizeLocked(entry, error);
            }
            if (own) loginNeeded = true;
            error = own ? expired
                        : "Error: this mailbox's Google login has expired; its owner has to log in again "
                          "(tools/google_auth.py --gmail-only).";
        } else {
            error = "Error: Google login failed (" + errorText(status, reply) + ")";
        }
        return false;
    }
    entry.access = parsed["access_token"].as<const char*>();
    entry.until = millis() + (max(uint32_t(parsed["expires_in"] | 3600), uint32_t(600)) - 300) * 1000;
    return true;
}

// One API call with the account of `refresh` ("" = the account's). Returns the HTTP status (0
// without a token); `error` is set unless it is 2xx. A 401 (token revoked early) fetches a new
// token once.
int call(const char* method, const char* host, const String& path, const char* contentType, const uint8_t* body,
         size_t length, PsramSink& reply, String& error, const String& refresh = "")
{
    for (int attempt = 0; attempt < 2; ++attempt) {
        String token;
        if (!authorize(refresh, token, error)) return 0;
        reply.size = 0;
        const int status = send(method, host, path, contentType, body, length, token, reply);
        if (status == 401 && attempt == 0) {
            dropToken(token);
            continue;
        }
        if (status < 200 || status >= 300) error = "Error: " + errorText(status, reply);
        return status;
    }
    return 401;
}

int call(const char* method, const char* host, const String& path, const String& body, PsramSink& reply,
         String& error)
{
    return call(method, host, path, "application/json", reinterpret_cast<const uint8_t*>(body.c_str()), body.length(),
                reply, error);
}

// --- Renewing the login: a link for the admin, answered through the relay --------------
// Google's device flow does not allow calendar or Gmail scopes, so the admin gets an ordinary
// authorization link (web client, PKCE) by Telegram. Google sends the browser to the relay's
// /google page, which queues code and state like a WhatsApp event; the device exchanges the code
// (useless without the client secret and the verifier, both only here) and keeps the refresh
// token in NVS. State and verifier are in NVS too, so a link survives a restart.

// As SCOPES in tools/google_auth.py.
const char* const SCOPES[] = {
    "https://www.googleapis.com/auth/calendar.events", "https://www.googleapis.com/auth/calendar.readonly",
    "https://www.googleapis.com/auth/contacts",        "https://www.googleapis.com/auth/drive.file",
    "https://www.googleapis.com/auth/tasks",           "https://www.googleapis.com/auth/gmail.modify"};

String loginState, loginVerifier;  // the pending link, under tokenLock
time_t loginSince = 0;
String loginCode;                  // from the relay, exchanged by loginWork; under tokenLock
uint32_t linkSentAt = 0;           // millis(), 0 = not yet (worker only)

String randomText(size_t length)
{
    static const char* alphabet = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789";
    String text;
    for (size_t i = 0; i < length; ++i) text += alphabet[esp_random() % 62];
    return text;
}

String redirectUri()
{
    String url = WHATSAPP_RELAY_URL;
    while (url.endsWith("/")) url.remove(url.length() - 1);
    return url + "/google";
}

void loadPendingLocked()
{
    if (loginState.length()) return;
    Preferences prefs;
    if (prefs.begin("google", true)) {
        loginState = prefs.getString("state", "");
        loginVerifier = prefs.getString("verifier", "");
        loginSince = time_t(prefs.getULong("since", 0));
    }
    prefs.end();
}

// The link for the pending state (a new one after GOOGLE_LOGIN_LINK_S), under tokenLock.
String loginUrlLocked()
{
    loadPendingLocked();
    const time_t now = time(nullptr);
    if (loginState.isEmpty() || (timeutil::synced() && now - loginSince > time_t(GOOGLE_LOGIN_LINK_S))) {
        loginState = randomText(32);
        loginVerifier = randomText(64);
        loginSince = now;
        Preferences prefs;
        if (prefs.begin("google", false)) {
            prefs.putString("state", loginState);
            prefs.putString("verifier", loginVerifier);
            prefs.putULong("since", uint32_t(loginSince));
        }
        prefs.end();
    }
    uint8_t hash[32];
    mbedtls_sha256_ret(reinterpret_cast<const uint8_t*>(loginVerifier.c_str()), loginVerifier.length(), hash, 0);
    unsigned char encoded[64];
    size_t length = 0;
    mbedtls_base64_encode(encoded, sizeof(encoded), &length, hash, sizeof(hash));
    String challenge;  // base64url without padding
    for (size_t i = 0; i < length; ++i)
        if (encoded[i] != '=') challenge += encoded[i] == '+' ? '-' : encoded[i] == '/' ? '_' : char(encoded[i]);
    String scopes;
    for (const char* scope : SCOPES) scopes += String(scopes.length() ? " " : "") + scope;
    return "https://accounts.google.com/o/oauth2/v2/auth?client_id=" + urlEncode(GOOGLE_WEB_CLIENT_ID) +
           "&redirect_uri=" + urlEncode(redirectUri()) + "&response_type=code&access_type=offline&prompt=consent" +
           "&scope=" + urlEncode(scopes) + "&state=" + loginState + "&code_challenge=" + challenge +
           "&code_challenge_method=S256";
}

// A message to the admin: Telegram, else WhatsApp. Returns "sent" or an error.
String tellAdmin(const String& html, const String& plain)
{
    return telegram::configured() ? telegram::send(html) : whatsapp::send(plain);
}

String sendLink(bool expired)
{
    if (!renewable()) return "Error: renewing the login needs GOOGLE_WEB_CLIENT_ID/SECRET and the WhatsApp relay.";
    xSemaphoreTake(tokenLock, portMAX_DELAY);
    const String url = loginUrlLocked();
    xSemaphoreGive(tokenLock);
    const String intro = expired ? "Meine Google-Anmeldung ist abgelaufen, ich komme nicht an Kalender, Kontakte "
                                   "und Drive. "
                                 : "Hier können Sie meine Google-Anmeldung erneuern. ";
    const String ask = "Bitte melden Sie sich an und setzen Sie alle Häkchen:";
    String href = url;
    href.replace("&", "&amp;");
    return tellAdmin(intro + ask + "\n<a href=\"" + href + "\">Bei Google anmelden</a>", intro + ask + "\n" + url);
}

void exchangeCode(const String& code)
{
    xSemaphoreTake(tokenLock, portMAX_DELAY);
    const String verifier = loginVerifier;
    xSemaphoreGive(tokenLock);
    const String body = "code=" + urlEncode(code) + "&client_id=" + urlEncode(GOOGLE_WEB_CLIENT_ID) +
                        "&client_secret=" + urlEncode(GOOGLE_WEB_CLIENT_SECRET) +
                        "&redirect_uri=" + urlEncode(redirectUri()) +
                        "&grant_type=authorization_code&code_verifier=" + verifier;
    PsramSink reply(16 * 1024);
    const int status = send("POST", TOKEN_HOST, "/token", "application/x-www-form-urlencoded", body, "", reply);
    JsonDocument parsed;
    if (status != 200 || deserializeJson(parsed, reply.data, reply.size) || !parsed["refresh_token"].is<const char*>()) {
        const String why = status == 200 ? String("no refresh token") : errorText(status, reply);
        errorlog::add("Google: login link failed (%s)", why.c_str());
        const String text = "Die Google-Anmeldung hat nicht geklappt (" + why + "). Bitte öffnen Sie den Link noch "
                            "einmal.";
        tellAdmin(text, text);
        return;
    }
    const String granted = parsed["scope"] | "";
    String missing;
    for (const char* scope : SCOPES)
        if (granted.indexOf(scope) < 0) missing += String(missing.length() ? ", " : "") + (strrchr(scope, '/') + 1);
    if (missing.length()) {
        const String text = "Es fehlen Berechtigungen (" + missing + "). Bitte öffnen Sie den Link noch einmal und "
                            "setzen Sie alle Häkchen.";
        tellAdmin(text, text);
        return;
    }
    const String refresh = parsed["refresh_token"].as<const char*>();
    Preferences prefs;
    if (prefs.begin("google", false)) {
        prefs.putString("refresh", refresh);
        prefs.remove("state");
        prefs.remove("verifier");
        prefs.remove("since");
    }
    prefs.end();
    xSemaphoreTake(tokenLock, portMAX_DELAY);
    account = refresh;
    accountWeb = accountLoaded = true;
    tokens.clear();
    loginState = loginVerifier = "";
    xSemaphoreGive(tokenLock);
    loginNeeded = false;
    linkSentAt = 0;
    errorlog::add("Google: login renewed by link");
    const String text = "Danke, meine Google-Anmeldung ist erneuert. Kalender, Kontakte und Drive gehen wieder.";
    tellAdmin(text, text);
}

// Background work: exchanges a received code, or sends the link when the login expired (again
// after GOOGLE_LOGIN_REMIND_MS). Not during a conversation.
void loginWork()
{
    if (live::open()) return;
    xSemaphoreTake(tokenLock, portMAX_DELAY);
    const String code = loginCode;
    loginCode = "";
    xSemaphoreGive(tokenLock);
    if (code.length()) {
        exchangeCode(code);
        return;
    }
    if (!loginNeeded || (linkSentAt && millis() - linkSentAt < GOOGLE_LOGIN_REMIND_MS)) return;
    linkSentAt = millis() | 1;
    const String result = sendLink(true);
    errorlog::add("Google: login expired, link to the admin: %s", result.c_str());
}

// --- Dates (local time, the device's TIMEZONE = CALENDAR_TIMEZONE) --------------------

// "YYYY-MM-DD" and "HH:MM" ("" = midnight) as local time. False if malformed or after 2037
// (32-bit time_t).
bool parseLocal(const String& date, const String& clock, time_t& out)
{
    int year, month, day, hour = 0, minute = 0;
    if (sscanf(date.c_str(), "%4d-%2d-%2d", &year, &month, &day) != 3) return false;
    if (clock.length() && sscanf(clock.c_str(), "%2d:%2d", &hour, &minute) != 2) return false;
    if (year < 1970 || year > 2037 || hour > 23 || minute > 59) return false;
    tm t{};
    t.tm_year = year - 1900;
    t.tm_mon = month - 1;
    t.tm_mday = day;
    t.tm_hour = hour;
    t.tm_min = minute;
    t.tm_isdst = -1;
    out = mktime(&t);
    return out != -1 && t.tm_mday == day && t.tm_mon == month - 1;  // no 31.02.
}

time_t addDays(time_t t, int days)
{
    tm local;
    localtime_r(&t, &local);
    local.tm_mday += days;
    local.tm_isdst = -1;
    return mktime(&local);
}

String dateStamp(time_t t) { return timeutil::format(t, "%Y-%m-%d"); }
String localStamp(time_t t) { return timeutil::format(t, "%Y-%m-%dT%H:%M:%S"); }

String utcStamp(time_t t)
{
    tm utc;
    gmtime_r(&t, &utc);
    char text[24];
    strftime(text, sizeof(text), "%Y-%m-%dT%H:%M:%SZ", &utc);
    return text;
}

// "2026-09-28" -> "Mo 28.09.2026"
String dayName(const String& date)
{
    time_t t;
    if (!parseLocal(date, "12:00", t)) return date;
    tm local;
    localtime_r(&t, &local);
    return String(WEEKDAYS[local.tm_wday]) + timeutil::format(t, " %d.%m.%Y");
}

const char* BAD_DATE = "Error: date/end_date as YYYY-MM-DD (until 2037), time as HH:MM or \"\" for all day";

// Sets start and end of `event`: at `clock` for `minutes`, or all day (clock "") until
// `lastDay` inclusive ("" = the same day).
String setTimes(JsonObject event, const String& date, const String& clock, int minutes, const String& lastDay)
{
    time_t start;
    if (!parseLocal(date, clock, start)) return BAD_DATE;
    event.remove("start");
    event.remove("end");
    JsonObject begin = event["start"].to<JsonObject>(), end = event["end"].to<JsonObject>();
    if (clock.isEmpty()) {
        time_t last = start;
        if (lastDay.length() && (!parseLocal(lastDay, "", last) || last < start)) return BAD_DATE;
        begin["date"] = dateStamp(start);
        end["date"] = dateStamp(addDays(last, 1));  // exclusive
        return "";
    }
    if (minutes <= 0 || minutes > 14 * 24 * 60) return "Error: duration_minutes between 1 and 20160";
    begin["dateTime"] = localStamp(start);
    begin["timeZone"] = CALENDAR_TIMEZONE;
    end["dateTime"] = localStamp(start + minutes * 60);
    end["timeZone"] = CALENDAR_TIMEZONE;
    return "";
}

struct Calendar;
String describe(JsonObjectConst event, const Calendar& calendar);
String describe(JsonObjectConst event);  // an event of the main calendar

// One line per event. Times of dateTime are local: the calls ask for CALENDAR_TIMEZONE.
String describeLine(JsonObjectConst event)
{
    String line = "- ";
    const char* firstDay = event["start"]["date"];
    if (firstDay) {
        line += dayName(firstDay);
        time_t end;
        if (parseLocal(event["end"]["date"] | "", "", end)) {
            const String last = dateStamp(addDays(end, -1));
            if (last != firstDay) line += " to " + dayName(last);
        }
        line += " all day";
    } else {
        const String begin = event["start"]["dateTime"] | "", end = event["end"]["dateTime"] | "";
        line += dayName(begin.substring(0, 10)) + " " + begin.substring(11, 16);
        if (end.length() >= 16)
            line += "–" + (end.substring(0, 10) != begin.substring(0, 10) ? dayName(end.substring(0, 10)) + " " : "") +
                    end.substring(11, 16);
    }
    line += ": " + String(event["summary"] | "(no title)");
    const char* location = event["location"];
    if (location && *location) line += " | place: " + String(location);
    String description = event["description"] | "";
    if (description.length()) {
        description.replace("\r", "");
        description.replace("\n", " ");
        line += " | note: " + utf8::prefix(description, 150);
    }
    if (event["recurringEventId"].is<const char*>()) line += " | recurring";
    return line;
}

bool has(JsonObjectConst arguments, const char* key) { return arguments[key].is<const char*>(); }

// --- Calendars: all the user shows in Google Calendar ------------------------------------

struct Calendar {
    String id, label;
    bool primary = false;
    bool own = false;  // owner or writer: its busy events block the user's time
};

std::vector<Calendar> calendarCache;  // guarded by calendarLock
uint32_t calendarsAt = 0;
SemaphoreHandle_t calendarLock = xSemaphoreCreateMutex();

String eventsPath(const String& calendar) { return "/calendar/v3/calendars/" + urlEncode(calendar) + "/events"; }

// The calendars shown in Google Calendar (selected, not hidden), the main one first. Subscribed
// iCal calendars are often named by their URL: then "subscription <host>".
bool calendars(std::vector<Calendar>& out, String& error)
{
    xSemaphoreTake(calendarLock, portMAX_DELAY);
    const bool fresh = calendarCache.size() && millis() - calendarsAt < CALENDARS_MS;
    if (fresh) out = calendarCache;
    xSemaphoreGive(calendarLock);
    if (fresh) return true;
    PsramSink reply(64 * 1024);
    if (call("GET", CALENDAR_HOST,
             "/calendar/v3/users/me/calendarList?fields=" +
                 urlEncode("items(id,summary,summaryOverride,primary,accessRole,selected,hidden)"),
             "", reply, error) != 200)
        return false;
    JsonDocument parsed;
    if (deserializeJson(parsed, reply.data, reply.size)) {
        error = "Error: Google's answer is not readable";
        return false;
    }
    out.clear();
    for (JsonObjectConst item : parsed["items"].as<JsonArrayConst>()) {
        const bool primary = item["primary"] | false;
        if (!primary && (!(item["selected"] | false) || (item["hidden"] | false))) continue;
        Calendar calendar;
        calendar.id = item["id"] | "";
        calendar.primary = primary;
        const String role = item["accessRole"] | "";
        calendar.own = role == "owner" || role == "writer";
        calendar.label = item["summaryOverride"] | item["summary"] | "";
        if (primary) calendar.label = "main";
        else if (calendar.label.startsWith("http")) {
            const int start = calendar.label.indexOf("//") + 2, end = calendar.label.indexOf('/', start);
            calendar.label = "subscription " + calendar.label.substring(start, end < 0 ? calendar.label.length() : end);
        }
        if (primary) out.insert(out.begin(), calendar);
        else out.push_back(calendar);
    }
    xSemaphoreTake(calendarLock, portMAX_DELAY);
    calendarCache = out;
    calendarsAt = millis();
    xSemaphoreGive(calendarLock);
    return true;
}

bool declined(JsonObjectConst event)
{
    for (JsonObjectConst attendee : event["attendees"].as<JsonArrayConst>())
        if ((attendee["self"] | false) && String(attendee["responseStatus"] | "") == "declined") return true;
    return false;
}

// Whether an event takes the user's time: busy (not marked free), in one of their own calendars,
// not declined. Holidays, school and other subscribed calendars are information only.
bool blocking(JsonObjectConst event, const Calendar& calendar)
{
    return calendar.own && String(event["transparency"] | "opaque") != "transparent" && !declined(event);
}

Calendar mainCalendar()
{
    Calendar calendar;
    calendar.id = "primary";
    calendar.label = "main";
    calendar.primary = calendar.own = true;
    return calendar;
}

// The calendar with this id (from the list), the main one if unknown.
Calendar calendarById(const String& id)
{
    std::vector<Calendar> all;
    String ignored;
    if (id != "primary" && calendars(all, ignored))
        for (const Calendar& calendar : all)
            if (calendar.id == id) return calendar;
    return mainCalendar();
}

// The event's line with its calendar (outside the main one), "info" when it does not block the
// user's time, and its id: "<calendar id>/<event id>" outside the main calendar, none for
// read-only calendars (nothing to change there).
String describe(JsonObjectConst event, const Calendar& calendar)
{
    String line = describeLine(event);
    if (!calendar.primary) line += " | calendar: " + calendar.label;
    if (!blocking(event, calendar)) line += " | info";
    if (declined(event)) line += " | declined";
    if (calendar.own) line += " [id " + (calendar.primary ? String() : calendar.id + "/") + String(event["id"] | "") + "]";
    return line + "\n";
}

String describe(JsonObjectConst event) { return describe(event, mainCalendar()); }

// "<calendar id>/<event id>" for events outside the main calendar, else the event id alone.
void splitEventId(const String& id, String& calendar, String& event)
{
    const int slash = id.lastIndexOf('/');
    calendar = slash > 0 ? id.substring(0, slash) : String("primary");
    event = slash > 0 ? id.substring(slash + 1) : id;
}

String eventPath(const String& id)
{
    String calendar, event;
    splitEventId(id, calendar, event);
    return eventsPath(calendar) + "/" + urlEncode(event);
}

Calendar calendarOf(const String& id)
{
    String calendar, event;
    splitEventId(id, calendar, event);
    return calendarById(calendar);
}

// --- Calendar -------------------------------------------------------------------------

// Every event of all shown calendars from `start` to `end` (exclusive) to `found`, optionally
// only those matching `query`. Every calendar is one request (kept connection). `more`: some
// calendar has more than MAX_EVENTS. False with `error` if the main calendar failed.
using EventFound = std::function<void(JsonObjectConst event, const Calendar& calendar)>;
bool forEachEvent(time_t start, time_t end, const String& query, const EventFound& found, bool& more, String& error)
{
    std::vector<Calendar> all;
    if (!calendars(all, error)) return false;
    more = false;
    for (const Calendar& calendar : all) {
        String path = eventsPath(calendar.id) + "?singleEvents=true&orderBy=startTime&maxResults=" + MAX_EVENTS +
                      "&timeZone=" + urlEncode(CALENDAR_TIMEZONE) + "&timeMin=" + urlEncode(utcStamp(start)) +
                      "&timeMax=" + urlEncode(utcStamp(end)) +
                      "&fields=" + urlEncode("items(id,summary,location,description,start,end,recurringEventId,"
                                             "transparency,attendees(self,responseStatus)),nextPageToken");
        if (query.length()) path += "&q=" + urlEncode(query);
        PsramSink reply(256 * 1024);
        if (call("GET", CALENDAR_HOST, path, "", reply, error) != 200) {
            if (calendar.primary) return false;
            Serial.printf("[Google] calendar %s: %s\n", calendar.label.c_str(), error.c_str());
            continue;  // a broken subscription must not hide the rest
        }
        JsonDocument parsed(PsramJson::instance());
        if (deserializeJson(parsed, reply.data, reply.size)) continue;
        for (JsonObjectConst event : parsed["items"].as<JsonArrayConst>()) found(event, calendar);
        more |= parsed["nextPageToken"].is<const char*>();
    }
    error = "";
    return true;
}

// Local start of an event ("YYYY-MM-DDTHH:MM", all day "T00:00"): sorts the lines.
String startKey(JsonObjectConst event)
{
    const String day = event["start"]["date"] | "";
    return day.length() ? day + "T00:00" : String(event["start"]["dateTime"] | "").substring(0, 16);
}

String listEvents(JsonObjectConst arguments, const tools::Context&)
{
    if (!timeutil::synced()) return NO_CLOCK;
    String from = arguments["from"] | "", to = arguments["to"] | "";
    if (from.isEmpty()) from = timeutil::today();
    if (to.isEmpty()) to = from;
    time_t start, last;
    if (!parseLocal(from, "", start) || !parseLocal(to, "", last) || last < start)
        return "Error: from and to as YYYY-MM-DD (until 2037), to not before from";
    if (last - start > 400 * 86400) return "Error: at most a good year at once";
    const String query = arguments["query"] | "";
    String error;
    std::vector<std::pair<String, String>> lines;  // sort key (local start), line
    bool more = false;
    if (!forEachEvent(start, addDays(last, 1), query,
                      [&](JsonObjectConst event, const Calendar& calendar) {
                          lines.push_back({startKey(event), describe(event, calendar)});
                      },
                      more, error))
        return error;
    const String range = dayName(from) + (to != from ? " to " + dayName(to) : "") +
                         (query.length() ? " matching \"" + query + "\"" : "");
    if (lines.empty()) return "No events " + range + ".";
    using Line = std::pair<String, String>;
    std::stable_sort(lines.begin(), lines.end(), [](const Line& a, const Line& b) { return a.first < b.first; });
    String text = "Events " + range + " (\"info\": does not take the user's time, e.g. holidays or a "
                                      "subscribed calendar):\n";
    for (const Line& line : lines) text += line.second;
    if (more) text += "(Some calendar has more than " + String(MAX_EVENTS) + " events: narrow the period.)";
    return text;
}

String createEvent(JsonObjectConst arguments, const tools::Context&)
{
    agenda::refreshSoon();  // the agenda of the next conversation shows the change
    if (!timeutil::synced()) return NO_CLOCK;
    const String title = arguments["title"] | "";
    if (title.isEmpty()) return "Error: title missing";
    JsonDocument event(PsramJson::instance());
    event["summary"] = title;
    if (has(arguments, "location") && *arguments["location"].as<const char*>()) event["location"] = arguments["location"];
    if (has(arguments, "description") && *arguments["description"].as<const char*>())
        event["description"] = arguments["description"];
    const String invalid = setTimes(event.as<JsonObject>(), arguments["date"] | "", arguments["time"] | "",
                                    arguments["duration_minutes"] | 60, arguments["end_date"] | "");
    if (invalid.length()) return invalid;
    String body;
    serializeJson(event, body);
    PsramSink reply(64 * 1024);
    String error;
    if (call("POST", CALENDAR_HOST, eventsPath("primary"), body, reply, error) != 200) return error;
    JsonDocument created;
    if (!deserializeJson(created, reply.data, reply.size)) event["id"] = created["id"] | "";
    return "Created:\n" + describe(event.as<JsonObjectConst>());
}

// Fetches the event with local times; false with `error` if there is none.
bool fetchEvent(const String& id, JsonDocument& event, String& error)
{
    if (id.isEmpty()) {
        error = "Error: id missing (from list_events)";
        return false;
    }
    PsramSink reply(64 * 1024);
    const int status = call("GET", CALENDAR_HOST, eventPath(id) + "?timeZone=" +
                                                      urlEncode(CALENDAR_TIMEZONE), "", reply, error);
    if (status == 404 || status == 410) {
        error = "Error: no event with this id (call list_events first)";
        return false;
    }
    if (status != 200) return false;
    if (deserializeJson(event, reply.data, reply.size)) {
        error = "Error: Google's answer is not readable";
        return false;
    }
    if (event["status"] == "cancelled") {
        error = "Error: this event is already deleted";
        return false;
    }
    return true;
}

String updateEvent(JsonObjectConst arguments, const tools::Context&)
{
    agenda::refreshSoon();  // the agenda of the next conversation shows the change
    if (!timeutil::synced()) return NO_CLOCK;
    const String id = arguments["id"] | "";
    JsonDocument event(PsramJson::instance());
    String error;
    if (!fetchEvent(id, event, error)) return error;

    // The backend fills the parameters it does not change with "" and 0 (strict schemas): those
    // mean unchanged. On 24.10.2026 changing only the place sent title "" and blanked the title.
    auto given = [&](const char* key) { return has(arguments, key) && *(arguments[key] | ""); };
    bool changed = false;
    for (const char* key : {"title", "location", "description"}) {
        if (!given(key)) continue;
        event[strcmp(key, "title") ? key : "summary"] = arguments[key].as<const char*>();
        changed = true;
    }
    if (given("date") || given("time") || given("end_date") || (arguments["duration_minutes"] | 0) > 0) {
        // Unchanged parts come from the event: its day, time, length (minutes or days).
        const bool allDay = event["start"]["date"].is<const char*>();
        const String begin = allDay ? event["start"]["date"] | "" : event["start"]["dateTime"] | "";
        const String end = allDay ? event["end"]["date"] | "" : event["end"]["dateTime"] | "";
        const String oldDate = begin.substring(0, 10), oldClock = allDay ? "" : begin.substring(11, 16);
        time_t oldStart = 0, oldEnd = 0;
        parseLocal(oldDate, oldClock, oldStart);
        parseLocal(end.substring(0, 10), allDay ? "" : end.substring(11, 16), oldEnd);
        const int oldMinutes = allDay ? 60 : max(1, int((oldEnd - oldStart) / 60));
        const String date = given("date") ? arguments["date"].as<const char*>() : oldDate.c_str();
        String clock = given("time") ? arguments["time"].as<const char*>() : oldClock.c_str();
        if (clock == "all_day") clock = "";
        String lastDay = arguments["end_date"] | "";
        time_t start;
        if (lastDay.isEmpty() && clock.isEmpty() && allDay && parseLocal(date, "", start)) {
            const int days = max(1, int((oldEnd - oldStart + 43200) / 86400));  // DST: +- 1 h
            lastDay = dateStamp(addDays(start, days - 1));
        }
        const int minutes = (arguments["duration_minutes"] | 0) > 0 ? arguments["duration_minutes"].as<int>() : oldMinutes;
        const String invalid = setTimes(event.as<JsonObject>(), date, clock, minutes, lastDay);
        if (invalid.length()) return invalid;
        changed = true;
    }
    if (!changed) return "Error: nothing to change given";

    String body;
    serializeJson(event, body);
    PsramSink reply(64 * 1024);
    if (call("PUT", CALENDAR_HOST, eventPath(id), body, reply, error) != 200) return error;
    return "Changed:\n" + describe(event.as<JsonObjectConst>(), calendarOf(id));
}

String deleteEvent(JsonObjectConst arguments, const tools::Context&)
{
    agenda::refreshSoon();  // the agenda of the next conversation shows the change
    const String id = arguments["id"] | "";
    JsonDocument event(PsramJson::instance());
    String error;
    if (!fetchEvent(id, event, error)) return error;
    PsramSink reply(16 * 1024);
    const int status = call("DELETE", CALENDAR_HOST, eventPath(id), "", reply, error);
    if (status != 204 && status != 200) return error;
    return "Deleted:\n" + describe(event.as<JsonObjectConst>(), calendarOf(id));
}

// Free time: gaps of at least `duration_minutes` between `earliest` and `latest` each day,
// counting only the events that block the user's time (blocking()).
String findFreeTime(JsonObjectConst arguments, const tools::Context&)
{
    if (!timeutil::synced()) return NO_CLOCK;
    String from = arguments["from"] | "", to = arguments["to"] | "";
    if (from.isEmpty()) from = timeutil::today();
    if (to.isEmpty()) to = from;
    time_t first, last;
    if (!parseLocal(from, "", first) || !parseLocal(to, "", last) || last < first)
        return "Error: from and to as YYYY-MM-DD (until 2037), to not before from";
    if (last - first > 31 * 86400) return "Error: at most a month at once";
    const int minutes = arguments["duration_minutes"] | 60;
    if (minutes < 5 || minutes > 24 * 60) return "Error: duration_minutes between 5 and 1440";
    const String earliest = arguments["earliest"] | "", latest = arguments["latest"] | "";
    const String opens = earliest.length() ? earliest : "08:00", closes = latest.length() ? latest : "20:00";
    time_t probeOpen, probeClose;
    if (!parseLocal(from, opens, probeOpen) || !parseLocal(from, closes, probeClose) || probeClose <= probeOpen)
        return "Error: earliest and latest as HH:MM, earliest before latest";

    std::vector<Calendar> all;
    String error;
    if (!calendars(all, error)) return error;
    std::vector<std::pair<time_t, time_t>> busy;
    for (const Calendar& calendar : all) {
        if (!calendar.own) continue;  // subscribed calendars never block
        const String path = eventsPath(calendar.id) + "?singleEvents=true&maxResults=250&timeZone=" +
                            urlEncode(CALENDAR_TIMEZONE) + "&timeMin=" + urlEncode(utcStamp(first)) +
                            "&timeMax=" + urlEncode(utcStamp(addDays(last, 1))) +
                            "&fields=" + urlEncode("items(start,end,transparency,attendees(self,responseStatus))");
        PsramSink reply(256 * 1024);
        if (call("GET", CALENDAR_HOST, path, "", reply, error) != 200) return error;
        JsonDocument parsed(PsramJson::instance());
        if (deserializeJson(parsed, reply.data, reply.size)) return "Error: Google's answer is not readable";
        for (JsonObjectConst event : parsed["items"].as<JsonArrayConst>()) {
            if (!blocking(event, calendar)) continue;
            time_t start = 0, end = 0;
            const String day = event["start"]["date"] | "";
            if (day.length()) {  // all day, marked busy: the whole days
                parseLocal(day, "", start);
                parseLocal(event["end"]["date"] | "", "", end);
            } else {
                const String begins = event["start"]["dateTime"] | "", ends = event["end"]["dateTime"] | "";
                parseLocal(begins.substring(0, 10), begins.substring(11, 16), start);
                parseLocal(ends.substring(0, 10), ends.substring(11, 16), end);
            }
            if (start && end > start) busy.push_back({start, end});
        }
    }
    std::sort(busy.begin(), busy.end());

    const time_t now = time(nullptr), need = time_t(minutes) * 60;
    String text;
    int slots = 0;
    for (time_t day = first; day <= last && slots < MAX_FREE_SLOTS; day = addDays(day, 1)) {
        const String date = dateStamp(day);
        time_t cursor, close;
        parseLocal(date, opens, cursor);
        parseLocal(date, closes, close);
        if (cursor < now) cursor = (now + 899) / 900 * 900;  // from the next quarter hour on
        String free;
        for (const auto& interval : busy) {
            if (interval.second <= cursor) continue;
            if (interval.first >= close) break;
            if (interval.first - cursor >= need && slots < MAX_FREE_SLOTS) {
                free += String(free.length() ? ", " : "") + timeutil::format(cursor, "%H:%M") + "–" +
                        timeutil::format(interval.first, "%H:%M");
                ++slots;
            }
            cursor = max(cursor, interval.second);
        }
        if (close - cursor >= need && slots < MAX_FREE_SLOTS) {
            free += String(free.length() ? ", " : "") + timeutil::format(cursor, "%H:%M") + "–" +
                    timeutil::format(close, "%H:%M");
            ++slots;
        }
        if (free.length()) text += "- " + dayName(date) + ": " + free + "\n";
    }
    const String frame = "at least " + String(minutes) + " min between " + opens + " and " + closes;
    if (text.isEmpty()) return "No free time " + frame + " from " + dayName(from) + " to " + dayName(to) + ".";
    return "Free time, " + frame + " (only events in the user's own calendars that are marked busy count; "
           "holidays and subscribed calendars do not):\n" + text +
           (slots >= MAX_FREE_SLOTS ? "(more: narrow the period)\n" : "");
}

// --- Contacts -------------------------------------------------------------------------

String birthday(JsonObjectConst date)
{
    const int day = date["day"] | 0, month = date["month"] | 0, year = date["year"] | 0;
    if (!day || !month) return "";
    char text[16];
    if (year) snprintf(text, sizeof(text), "%02d.%02d.%04d", day, month, year);
    else snprintf(text, sizeof(text), "%02d.%02d.", day, month);
    return text;
}

String findContact(JsonObjectConst arguments, const tools::Context&)
{
    String query = arguments["name"] | "";
    query.trim();
    if (query.isEmpty()) return "Error: name missing";
    // searchContacts needs a warm-up request with an empty query to fill its cache.
    static bool warmedUp = false;
    PsramSink reply(128 * 1024);
    String error;
    const bool warmingUp = !warmedUp;
    if (warmingUp) {
        if (call("GET", PEOPLE_HOST, "/v1/people:searchContacts?query=&readMask=names", "", reply, error) != 200)
            return error;
        warmedUp = true;
    }
    const String path = "/v1/people:searchContacts?pageSize=10&query=" + urlEncode(query) +
                        "&readMask=names,nicknames,phoneNumbers,emailAddresses,birthdays&fields=" +
                        urlEncode("results(person(resourceName,names/displayName,nicknames/value,"
                                  "phoneNumbers(value,type),emailAddresses(value,type),birthdays/date))");
    JsonDocument parsed(PsramJson::instance());
    for (int attempt = 0; attempt < 2; ++attempt) {
        if (call("GET", PEOPLE_HOST, path, "", reply, error) != 200) return error;
        if (deserializeJson(parsed, reply.data, reply.size)) return "Error: Google's answer is not readable";
        if (parsed["results"].size() || !warmingUp) break;
        delay(2000);  // the cache right after the warm-up can still be empty
    }
    JsonArrayConst results = parsed["results"];
    if (results.size() == 0) return "No contact found for \"" + query + "\" (try the first or last name).";
    String text = "Contacts for \"" + query + "\":\n";
    for (JsonObjectConst result : results) {
        JsonObjectConst person = result["person"];
        text += "- " + String(person["names"][0]["displayName"] | "(no name)");
        if (person["nicknames"][0]["value"].is<const char*>()) text += " (" + String(person["nicknames"][0]["value"].as<const char*>()) + ")";
        String details;
        for (JsonObjectConst phone : person["phoneNumbers"].as<JsonArrayConst>())
            details += String(details.length() ? ", " : "") + "phone " + (phone["type"] | "") + " " + (phone["value"] | "");
        for (JsonObjectConst mail : person["emailAddresses"].as<JsonArrayConst>())
            details += String(details.length() ? ", " : "") + "mail " + (mail["value"] | "");
        const String born = birthday(person["birthdays"][0]["date"]);
        if (born.length()) details += String(details.length() ? ", " : "") + "birthday " + born;
        text += (details.length() ? ": " + details : "") + " [id " + String(person["resourceName"] | "") + "]\n";
    }
    return text;
}

void reloadDirectorySoon();

// A new contact, or more for an existing one (id from find_contact): a phone number, an e-mail
// address, a note. Existing numbers, addresses and notes stay.
String saveContact(JsonObjectConst arguments, const tools::Context&)
{
    const String id = arguments["id"] | "", name = arguments["name"] | "", phone = arguments["phone"] | "",
                 email = arguments["email"] | "", note = arguments["note"] | "";
    String type = arguments["phone_type"] | "";
    if (type.isEmpty()) type = "mobile";
    if (phone.isEmpty() && email.isEmpty() && note.isEmpty() && (id.length() || name.isEmpty()))
        return "Error: nothing to save (name for a new contact, phone, email or note)";
    if (id.length() && !id.startsWith("people/")) return "Error: id as in find_contact (people/...)";
    JsonDocument person(PsramJson::instance());
    String error;
    PsramSink reply(64 * 1024);
    if (id.length()) {  // the current entry with its etag: an update replaces whole fields
        const int status = call("GET", PEOPLE_HOST,
                                "/v1/" + id + "?personFields=names,phoneNumbers,emailAddresses,biographies", "", reply,
                                error);
        if (status == 404) return "Error: no contact with this id (call find_contact first)";
        if (status != 200) return error;
        if (deserializeJson(person, reply.data, reply.size)) return "Error: Google's answer is not readable";
    } else {
        person["names"][0]["unstructuredName"] = name;
    }
    String fields;
    if (phone.length()) {
        JsonObject entry = person["phoneNumbers"].as<JsonArray>() ? person["phoneNumbers"].as<JsonArray>().add<JsonObject>()
                                                                   : person["phoneNumbers"].to<JsonArray>().add<JsonObject>();
        entry["value"] = phone;
        entry["type"] = type;
        fields += "phoneNumbers,";
    }
    if (email.length()) {
        JsonObject entry = person["emailAddresses"].as<JsonArray>()
                               ? person["emailAddresses"].as<JsonArray>().add<JsonObject>()
                               : person["emailAddresses"].to<JsonArray>().add<JsonObject>();
        entry["value"] = email;
        fields += "emailAddresses,";
    }
    if (note.length()) {  // one note per contact: appended to the existing one
        const String old = person["biographies"][0]["value"] | "";
        person["biographies"][0]["value"] = old.length() ? old + "\n" + note : note;
        person["biographies"][0]["contentType"] = "TEXT_PLAIN";
        fields += "biographies,";
    }
    String body;
    serializeJson(person, body);
    reply.size = 0;
    const int status =
        id.length() ? call("PATCH", PEOPLE_HOST,
                           "/v1/" + id + ":updateContact?updatePersonFields=" + fields.substring(0, fields.length() - 1) +
                               "&personFields=names,phoneNumbers,emailAddresses",
                           body, reply, error)
                    : call("POST", PEOPLE_HOST, "/v1/people:createContact?personFields=names,phoneNumbers,emailAddresses",
                           body, reply, error);
    if (status != 200) return error;
    JsonDocument saved(PsramJson::instance());
    deserializeJson(saved, reply.data, reply.size);
    reloadDirectorySoon();  // so a caller with the new number is greeted by name
    String text = (id.length() ? "Contact updated: " : "Contact created: ") +
                  String(saved["names"][0]["displayName"] | name.c_str());
    for (JsonObjectConst entry : saved["phoneNumbers"].as<JsonArrayConst>())
        text += ", phone " + String(entry["value"] | "");
    for (JsonObjectConst entry : saved["emailAddresses"].as<JsonArrayConst>())
        text += ", mail " + String(entry["value"] | "");
    return text + " [id " + String(saved["resourceName"] | "") + "]";
}

String upcomingBirthdays(JsonObjectConst arguments, const tools::Context&)
{
    if (!timeutil::synced()) return NO_CLOCK;
    const int days = constrain(int(arguments["days"] | 30), 0, 366);
    time_t today;
    parseLocal(timeutil::today(), "", today);
    tm now;
    localtime_r(&today, &now);

    struct Entry {
        int away;
        String line;
    };
    std::vector<Entry> found;
    String pageToken, error;
    for (int page = 0; page < 5; ++page) {
        String path = "/v1/people/me/connections?personFields=names,birthdays&pageSize=1000&fields=" +
                      urlEncode("connections(names/displayName,birthdays/date),nextPageToken");
        if (pageToken.length()) path += "&pageToken=" + urlEncode(pageToken);
        PsramSink reply(512 * 1024);
        if (call("GET", PEOPLE_HOST, path, "", reply, error) != 200) return error;
        JsonDocument parsed(PsramJson::instance());
        if (deserializeJson(parsed, reply.data, reply.size)) return "Error: Google's answer is not readable";
        for (JsonObjectConst person : parsed["connections"].as<JsonArrayConst>()) {
            JsonObjectConst date = person["birthdays"][0]["date"];
            const int day = date["day"] | 0, month = date["month"] | 0, year = date["year"] | 0;
            if (!day || !month) continue;
            // Next birthday from today on; 29.02. is the 01.03. in other years (mktime).
            time_t next = 0;
            for (int offset = 0; offset < 2; ++offset) {
                tm t{};
                t.tm_year = now.tm_year + offset;
                t.tm_mon = month - 1;
                t.tm_mday = day;
                t.tm_isdst = -1;
                next = mktime(&t);
                if (next >= today) break;
            }
            const int away = int((next - today + 43200) / 86400);
            if (away > days) continue;
            tm when;
            localtime_r(&next, &when);
            String line = "- " + String(away == 0 ? "today" : away == 1 ? "tomorrow" : "") +
                          (away > 1 ? dayName(dateStamp(next)).substring(0, 9) : "") + ": " +
                          String(person["names"][0]["displayName"] | "(no name)");
            if (year) line += " (turns " + String(when.tm_year + 1900 - year) + ")";
            found.push_back({away, line + "\n"});
        }
        pageToken = parsed["nextPageToken"] | "";
        if (pageToken.isEmpty()) break;
    }
    if (found.empty()) return "No birthdays in the next " + String(days) + " days (according to the Google contacts).";
    std::sort(found.begin(), found.end(), [](const Entry& a, const Entry& b) { return a.away < b.away; });
    String text = "Birthdays in the next " + String(days) + " days:\n";
    for (const Entry& entry : found) text += entry.line;
    return text;
}

// --- Caller names: every contact's numbers, "\n<number>\t<name>\n...", in PSRAM ------------

SemaphoreHandle_t directoryLock = nullptr;
char* directory = nullptr;
size_t directoryEntries = 0;
uint32_t directoryDueAt = 0;          // next refresh (millis)
volatile bool directoryMissed = false;  // a caller was not found: maybe a new contact

void reloadDirectorySoon() { directoryDueAt = millis(); }  // by the worker, between conversations

// 0171 1234567 / +49 171 1234567 / 0049171... -> 01711234567 (as phone/ writes the caller).
String normalizedNumber(const String& number)
{
    String digits;
    for (size_t i = 0; i < number.length(); ++i)
        if (isdigit(uint8_t(number[i])) || (number[i] == '+' && digits.isEmpty())) digits += number[i];
    if (digits.startsWith("+")) digits = "00" + digits.substring(1);
    if (digits.startsWith("0049")) digits = "0" + digits.substring(4);
    return digits;
}

bool loadDirectory(String& error)
{
    PsramSink text(2 * 1024 * 1024);
    text.print("\n");
    size_t entries = 0;
    String pageToken;
    for (int page = 0; page < 5; ++page) {
        String path = "/v1/people/me/connections?personFields=names,phoneNumbers&pageSize=1000&fields=" +
                      urlEncode("connections(names/displayName,phoneNumbers(value,canonicalForm)),nextPageToken");
        if (pageToken.length()) path += "&pageToken=" + urlEncode(pageToken);
        PsramSink reply(1024 * 1024);
        if (call("GET", PEOPLE_HOST, path, "", reply, error) != 200) return false;
        JsonDocument parsed(PsramJson::instance());
        if (deserializeJson(parsed, reply.data, reply.size)) {
            error = "Google's answer is not readable";
            return false;
        }
        for (JsonObjectConst person : parsed["connections"].as<JsonArrayConst>()) {
            String name = person["names"][0]["displayName"] | "";
            name.replace("\t", " ");
            name.replace("\n", " ");
            if (name.isEmpty()) continue;
            for (JsonObjectConst phone : person["phoneNumbers"].as<JsonArrayConst>()) {
                const String canonical = normalizedNumber(phone["canonicalForm"] | ""),
                             written = normalizedNumber(phone["value"] | "");
                for (const String& number : {canonical, written}) {
                    if (number.length() < 6 || (&number == &written && written == canonical)) continue;
                    text.print(number + "\t" + name + "\n");
                    ++entries;
                }
            }
        }
        pageToken = parsed["nextPageToken"] | "";
        if (pageToken.isEmpty()) break;
    }
    char* next = reinterpret_cast<char*>(text.release());
    if (!next) {
        error = "not enough memory";
        return false;
    }
    xSemaphoreTake(directoryLock, portMAX_DELAY);
    std::swap(directory, next);
    directoryEntries = entries;
    xSemaphoreGive(directoryLock);
    free(next);
    return true;
}

// Background work of the registry's worker: the directory at the start and every
// GOOGLE_DIRECTORY_MS, between conversations; after a failure again in 5 minutes.
void refreshDirectory()
{
    static uint32_t lastLoad = 0;
    // An unknown caller: reload after the call (at most every 10 minutes), so a contact made
    // meanwhile is known at the next call.
    if (directoryMissed && lastLoad && millis() - lastLoad > 10 * 60 * 1000) directoryDueAt = millis();
    if (int32_t(millis() - directoryDueAt) < 0 || live::open() || !timeutil::synced()) return;
    tls_gate::TryHold gate;  // one background TLS job at a time
    if (!gate) return;
    String error;
    directoryMissed = false;
    lastLoad = millis();
    if (loadDirectory(error)) {
        Serial.printf("[Google] address book for callers: %u numbers\n", unsigned(directoryEntries));
        directoryDueAt = millis() + GOOGLE_DIRECTORY_MS;
    } else {
        Serial.printf("[Google] address book for callers: %s\n", error.c_str());
        directoryDueAt = millis() + 5 * 60 * 1000;
    }
}

}  // namespace

String callerName(const String& number)
{
    const String key = "\n" + normalizedNumber(number) + "\t";
    if (key.length() < 8 || !directoryLock) return "";
    String name;
    xSemaphoreTake(directoryLock, portMAX_DELAY);
    const char* hit = directory ? strstr(directory, key.c_str()) : nullptr;
    if (hit) {
        hit += key.length();
        const char* end = strchr(hit, '\n');
        name = String(hit).substring(0, end ? end - hit : strlen(hit));
    }
    xSemaphoreGive(directoryLock);
    if (name.isEmpty()) directoryMissed = true;
    return name;
}

bool events(time_t from, time_t to, std::vector<Event>& out, String& error)
{
    out.clear();
    std::vector<std::pair<String, Event>> found;  // sort key (local start), event
    bool more = false;
    const bool ok = forEachEvent(from, to, "", [&](JsonObjectConst event, const Calendar& calendar) {
        if (declined(event)) return;
        Event e;
        const char* day = event["start"]["date"];
        e.allDay = day;
        if (e.allDay) {
            parseLocal(day, "", e.start);
            if (!parseLocal(event["end"]["date"] | "", "", e.end)) e.end = addDays(e.start, 1);
        } else {
            const String begin = event["start"]["dateTime"] | "", end = event["end"]["dateTime"] | "";
            if (!parseLocal(begin.substring(0, 10), begin.substring(11, 16), e.start)) return;
            if (!parseLocal(end.substring(0, 10), end.substring(11, 16), e.end)) e.end = e.start;
        }
        e.info = !blocking(event, calendar);
        e.id = calendar.id + "/" + String(event["id"] | "");
        e.title = event["summary"] | "(no title)";
        e.line = describeLine(event).substring(2);  // without "- "
        if (!calendar.primary) e.line += " | calendar: " + calendar.label;
        if (e.info) e.line += " | info";
        found.push_back({startKey(event), e});
    }, more, error);
    if (!ok) return false;
    using Found = std::pair<String, Event>;
    std::stable_sort(found.begin(), found.end(), [](const Found& a, const Found& b) { return a.first < b.first; });
    for (const Found& f : found) out.push_back(f.second);
    return true;
}

int request(const char* method, const char* host, const String& path, const char* contentType, const uint8_t* body,
            size_t length, PsramSink& reply, String& error, const String& refreshToken)
{
    return call(method, host, path, contentType, body, length, reply, error, refreshToken);
}

bool configured() { return (*GOOGLE_CLIENT_ID && *GOOGLE_CLIENT_SECRET && *GOOGLE_REFRESH_TOKEN) || renewable(); }

void loginCallback(const String& code, const String& state)
{
    if (code.isEmpty() || code.length() > 512 || !renewable()) return;
    xSemaphoreTake(tokenLock, portMAX_DELAY);
    loadPendingLocked();
    const bool ours = loginState.length() && state == loginState;
    if (ours) loginCode = code;
    xSemaphoreGive(tokenLock);
    Serial.println(ours ? "[Google] login code received" : "[Google] login answer with an unknown state ignored");
}

void addTools()
{
    if (!configured()) return;
    directoryLock = xSemaphoreCreateMutex();
    tools::addBackgroundWork(refreshDirectory);
    if (renewable()) {
        if (INBOUND) tools::addBackgroundWork(loginWork);  // the main the assistant renews the login
        tools::add(tools::Tool{
            "google_login",
            "Sends the admin a link (Telegram) to renew the assistant's Google login. Only when the admin asks for it; when "
            "the login has expired the link goes out by itself.",
            R"json({"type": "object", "additionalProperties": false, "properties": {}})json",
            [](JsonObjectConst, const tools::Context&) {
                const String result = sendLink(loginNeeded);
                return result == "sent" ? String("Link sent to the admin.") : result;
            },
            tools::Slow | tools::Private, tools::Everywhere});
    }
    using tools::Tool;
    // All slow (TLS to Google, seconds): in a conversation they run in the registry's worker.
    tools::add(Tool{
        "list_events",
        "Lists the events of all calendars the user shows in Google Calendar from one day to another (both "
        "inclusive), optionally only those matching query (words in title, notes, location). For 'what's on "
        "tomorrow', 'when is the dentist', a daily overview. To find one event without a date search from today a "
        "year ahead. One line per event with its calendar (outside the main one); lines marked 'info' (holidays, "
        "subscribed calendars, events marked free) are information, not the user's appointments. Events the user "
        "can change carry an id (for update_event and delete_event).",
        R"json({"type": "object", "additionalProperties": false,
            "properties": {"from": {"type": "string", "description": "YYYY-MM-DD, '' = today"},
                           "to": {"type": "string", "description": "YYYY-MM-DD, '' = same day as from"},
                           "query": {"type": "string", "description": "search words, '' = all events"}},
            "required": ["from", "to"]})json",
        listEvents, tools::Slow | tools::Private, tools::Everywhere});
    tools::add(Tool{
        "create_event",
        "Adds an event to the user's Google Calendar, after you read out title, day and time and the user "
        "confirmed them. Without time it is an all-day event (end_date for several days). Calendar reminders "
        "come from Google; for a spoken reminder by the assistant use create_task as well if the user wants one.",
        R"json({"type": "object", "additionalProperties": false,
            "properties": {"title": {"type": "string", "description": "short German title, e.g. 'Zahnarzt Dr. Müller'"},
                           "date": {"type": "string", "description": "YYYY-MM-DD"},
                           "time": {"type": "string", "description": "start HH:MM (24 h), '' = all day"},
                           "duration_minutes": {"type": "integer", "description": "default 60"},
                           "end_date": {"type": "string", "description": "all-day events: last day YYYY-MM-DD"},
                           "location": {"type": "string"},
                           "description": {"type": "string", "description": "notes"}},
            "required": ["title", "date", "time"]})json",
        createEvent, tools::Slow | tools::Private, tools::Everywhere});
    tools::add(Tool{
        "update_event",
        "Changes an event in the user's Google Calendar (id from list_events), after you read out the change and "
        "the user confirmed it. Give only what changes, '' or 0 for the rest (unchanged); a new date or time keeps "
        "the length. The id of a recurring event's occurrence changes only that occurrence.",
        R"json({"type": "object", "additionalProperties": false,
            "properties": {"id": {"type": "string"},
                           "title": {"type": "string"},
                           "date": {"type": "string", "description": "YYYY-MM-DD"},
                           "time": {"type": "string", "description": "start HH:MM (24 h), 'all_day' = make it an all-day event"},
                           "duration_minutes": {"type": "integer"},
                           "end_date": {"type": "string", "description": "all-day events: last day YYYY-MM-DD"},
                           "location": {"type": "string"},
                           "description": {"type": "string", "description": "notes (replaces the old ones)"}},
            "required": ["id"]})json",
        updateEvent, tools::Slow | tools::Private, tools::Everywhere});
    tools::add(Tool{
        "delete_event",
        "Deletes an event from the user's Google Calendar (id from list_events), after you read it out and the "
        "user confirmed. For a recurring event only this occurrence.",
        R"json({"type": "object", "additionalProperties": false,
            "properties": {"id": {"type": "string"}}, "required": ["id"]})json",
        deleteEvent, tools::Slow | tools::Private, tools::Everywhere});
    tools::add(Tool{
        "find_free_time",
        "Finds free time in the user's calendars: gaps of at least duration_minutes between earliest and latest "
        "each day, from one day to another. For 'wann habe ich nächste Woche zwei Stunden Zeit', 'passt ein "
        "Termin am Donnerstag'. Only events that take the user's time count, not holidays, subscribed calendars "
        "or events marked free.",
        R"json({"type": "object", "additionalProperties": false,
            "properties": {"from": {"type": "string", "description": "YYYY-MM-DD, '' = today"},
                           "to": {"type": "string", "description": "YYYY-MM-DD, '' = same day as from"},
                           "duration_minutes": {"type": "integer", "description": "default 60"},
                           "earliest": {"type": "string", "description": "HH:MM, '' = 08:00"},
                           "latest": {"type": "string", "description": "HH:MM, '' = 20:00"}},
            "required": ["from", "to", "duration_minutes"]})json",
        findFreeTime, tools::Slow | tools::Private, tools::Everywhere});
    tools::add(Tool{
        "find_contact",
        "Looks up people in the user's Google contacts by name (first name, last name or nickname): phone numbers, "
        "e-mail addresses, birthday. Use it before phone_call or send_email to someone the user knows, and for "
        "questions like 'wann hat Anna Geburtstag'. Several matches: ask which one is meant.",
        R"json({"type": "object", "additionalProperties": false,
            "properties": {"name": {"type": "string"}}, "required": ["name"]})json",
        findContact, tools::Slow | tools::Private, tools::Everywhere});
    tools::add(Tool{
        "save_contact",
        "Saves a contact in the user's Google contacts, ONLY after you read out name and number (or address) and "
        "the user agreed: a new one (id '', name required), or a phone number, e-mail address or note added to an "
        "existing one (id from find_contact; search first so no duplicate is made). For 'speicher die Nummer vom "
        "Handwerker'. Numbers as said, e.g. '0171 1234567'.",
        R"json({"type": "object", "additionalProperties": false,
            "properties": {"id": {"type": "string", "description": "'' = new contact"},
                           "name": {"type": "string", "description": "full name or e.g. 'Elektriker Maier'"},
                           "phone": {"type": "string"},
                           "phone_type": {"type": "string", "enum": ["mobile", "home", "work", "other"]},
                           "email": {"type": "string"},
                           "note": {"type": "string", "description": "e.g. what they did for the user"}},
            "required": ["id", "name"]})json",
        saveContact, tools::Slow | tools::Private, tools::Everywhere});
    tools::add(Tool{
        "upcoming_birthdays",
        "Lists the birthdays in the user's Google contacts in the next days (with the new age where the year is "
        "known), e.g. 'wer hat diese Woche Geburtstag', or in a morning overview.",
        R"json({"type": "object", "additionalProperties": false,
            "properties": {"days": {"type": "integer", "description": "how many days ahead, 0 = today only, default 30"}},
            "required": ["days"]})json",
        upcomingBirthdays, tools::Slow | tools::Private, tools::Everywhere});
    // Only for tests over serial: removes the test contact again.
    tools::add(Tool{
        "delete_contact", "Deletes a contact (id from find_contact).",
        R"json({"type": "object", "additionalProperties": false,
            "properties": {"id": {"type": "string"}}, "required": ["id"]})json",
        [](JsonObjectConst arguments, const tools::Context&) {
            const String id = arguments["id"] | "";
            if (!id.startsWith("people/")) return String("Error: id as in find_contact (people/...)");
            PsramSink reply(16 * 1024);
            String error;
            const int status = call("DELETE", PEOPLE_HOST, "/v1/" + id + ":deleteContact", "", reply, error);
            return status == 200 ? String("Deleted.") : error;
        },
        tools::Slow, tools::Console});
    // Only for tests over serial: GPT Live gets the name with the call itself.
    tools::add(Tool{
        "caller_name", "Name of the contact with this phone number (address book copy for callers).",
        R"json({"type": "object", "additionalProperties": false,
            "properties": {"number": {"type": "string"}}, "required": ["number"]})json",
        [](JsonObjectConst arguments, const tools::Context&) {
            const String name = callerName(arguments["number"] | "");
            return name.length() ? name : String("unknown");
        },
        0, tools::Console});
}

}  // namespace google
