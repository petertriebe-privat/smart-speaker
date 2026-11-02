#include "services/telegram.h"

#include <ArduinoJson.h>
#include <HTTPClient.h>
#include <algorithm>
#include <Preferences.h>
#include <esp_heap_caps.h>
#include <initializer_list>
#include <mbedtls/base64.h>
#include <time.h>
#include <vector>

#include "assistant/agent.h"
#include "assistant/files.h"
#include "assistant/memory.h"
#include "assistant/people.h"
#include "assistant/tools.h"
#include "ca_certs.h"
#include "config.h"
#include "prompts.h"
#include "secrets.h"
#include "services/https.h"
#include "services/openai.h"
#include "services/psram_json.h"
#include "system/file_util.h"
#include "system/time_util.h"
#include "system/tls_gate.h"
#include "system/utf8.h"

// Optional in secrets.h (older copies do not have them).
#ifndef TELEGRAM_BOT_TOKEN
#define TELEGRAM_BOT_TOKEN ""
#endif
#ifndef TELEGRAM_OWNER_ID
#define TELEGRAM_OWNER_ID 0
#endif

namespace telegram {
namespace {

constexpr int64_t OWNER = TELEGRAM_OWNER_ID;

constexpr const char* HELP =
    "Ich bin " ASSISTANT_NAME ". Schreib mir einfach, was du brauchst: Licht und Geräte schalten, Fragen und "
    "Websuche, Mails, Bilder, Erinnerungen und Aufgaben, Skills. Ich weiß dasselbe wie zu Hause am "
    "Lautsprecher.\n\n"
    "/neu – neues Gespräch beginnen\n"
    "/aufgaben – geplante Aufgaben\n"
    "/skills – was du mir beigebracht hast\n\n"
    "Auf Sprachnachrichten antworte ich auch mit Sprache. Fotos schaue ich mir an (schreib dazu, was "
    "ich damit tun soll). Andere Dateien verstehe ich noch nicht.";

String number(int64_t value)
{
    char text[24];
    snprintf(text, sizeof(text), "%lld", static_cast<long long>(value));
    return text;
}

// --- Bot API ---------------------------------------------------------------------------

struct Part {
    const uint8_t* data;
    size_t length;
};

Part part(const String& text) { return Part{reinterpret_cast<const uint8_t*>(text.c_str()), text.length()}; }

// One request over `client` (connecting if needed): `head` without the final blank line, then
// the body in parts. Returns the HTTP status or a negative HTTPClient error. A kept connection
// the server closed meanwhile gets one more try on a new one. The token is in the path: never
// log it.
int exchange(https::Client& client, const String& head, std::initializer_list<Part> parts, uint32_t timeoutMs,
             PsramSink& sink)
{
    int status = 0;
    for (int attempt = 0; attempt < 2; ++attempt) {
        const bool reused = client.connected();
        client.setTimeout(timeoutMs);
        if (!client.connect()) return HTTPC_ERROR_CONNECTION_REFUSED;
        bool written = client.write(head + "\r\n");
        for (const Part& p : parts) written = written && client.write(p.data, p.length);
        if (written) {
            status = client.read(sink);
        } else {
            client.stop();
            status = HTTPC_ERROR_SEND_PAYLOAD_FAILED;
        }
        if (status > 0 || !reused) break;
        free(sink.release());
    }
    return status;
}

// One Bot API request; `reply` gets Telegram's answer ({"ok": true, "result": ...}). Returns
// "" or an error text.
String call(https::Client& client, const char* method, const char* contentType, std::initializer_list<Part> parts,
            uint32_t timeoutMs, JsonDocument& reply)
{
    size_t length = 0;
    for (const Part& p : parts) length += p.length;
    const String head = String("POST /bot") + TELEGRAM_BOT_TOKEN + "/" + method + " HTTP/1.1\r\nHost: " +
                        TELEGRAM_HOST + "\r\nContent-Type: " + contentType + "\r\nContent-Length: " + length +
                        "\r\nConnection: keep-alive\r\n";
    PsramSink sink(512 * 1024);
    const int status = exchange(client, head, parts, timeoutMs, sink);
    if (status == HTTPC_ERROR_CONNECTION_REFUSED) return "no connection to Telegram";
    if (status <= 0) return "Telegram not reachable (" + HTTPClient::errorToString(status) + ")";
    reply.clear();
    if (deserializeJson(reply, sink.data, sink.size))
        return "answer from Telegram not readable (HTTP " + String(status) + ")";
    if (reply["ok"] | false) return "";
    const char* description = reply["description"];
    return description ? String(description) : "HTTP " + String(status);
}

String callJson(https::Client& client, const char* method, const JsonDocument& body, JsonDocument& reply,
                uint32_t timeoutMs = TELEGRAM_TIMEOUT_MS)
{
    String payload;
    serializeJson(body, payload);
    utf8::repair(payload);
    return call(client, method, "application/json", {part(payload)}, timeoutMs, reply);
}

String escape(String text)
{
    text.replace("&", "&amp;");
    text.replace("<", "&lt;");
    text.replace(">", "&gt;");
    return text;
}

// Telegram HTML as plain text, for when Telegram rejects the markup ("can't parse entities").
// Links keep their address: <a href="u">Text</a> -> "Text (u)"; not `withLinks` (read out): "Text".
String plain(const String& html, bool withLinks = true)
{
    String text, link;
    for (int i = 0; i < int(html.length());) {
        if (html[i] != '<') {
            text += html[i++];
            continue;
        }
        const int end = html.indexOf('>', i);
        if (end < 0) {
            text += html.substring(i);
            break;
        }
        const String tag = html.substring(i, end + 1);
        const int href = tag.indexOf("href=\"");
        if (tag.startsWith("<a ") && href > 0) link = tag.substring(href + 6, tag.indexOf('"', href + 6));
        else if (tag == "</a>" && link.length()) {
            if (withLinks) text += " (" + link + ")";
            link = "";
        }
        i = end + 1;
    }
    text.replace("&lt;", "<");
    text.replace("&gt;", ">");
    text.replace("&quot;", "\"");
    text.replace("&amp;", "&");
    return text;
}

// Markdown links as the web search adds its sources ("([timeanddate.de](https://...))") as
// Telegram HTML; Telegram would show them raw.
String linksToHtml(const String& text)
{
    String out;
    int from = 0;
    for (int open; (open = text.indexOf('[', from)) >= 0;) {
        const int middle = text.indexOf("](", open);
        const int close = middle < 0 ? -1 : text.indexOf(')', middle + 2);
        String url = close < 0 ? String() : text.substring(middle + 2, close);
        const String label = middle < 0 ? String() : text.substring(open + 1, middle);
        if (!url.startsWith("http") || label.indexOf('\n') >= 0 || label.indexOf('[') >= 0) {
            out += text.substring(from, open + 1);
            from = open + 1;
            continue;
        }
        url.replace("&amp;", "&");
        url.replace("&", "&amp;");
        url.replace("\"", "%22");
        out += text.substring(from, open) + "<a href=\"" + url + "\">" + label + "</a>";
        from = close + 1;
    }
    return out + text.substring(from);
}

// Messages of at most TELEGRAM_MESSAGE_CHARS bytes, split at line breaks where possible.
std::vector<String> split(String text)
{
    std::vector<String> messages;
    while (text.length() > TELEGRAM_MESSAGE_CHARS) {
        String head = utf8::prefix(text, TELEGRAM_MESSAGE_CHARS);
        const int cut = head.lastIndexOf('\n');
        if (cut > int(TELEGRAM_MESSAGE_CHARS / 2)) head = head.substring(0, cut);
        messages.push_back(head);
        text = text.substring(head.length());
        while (text.startsWith("\n")) text.remove(0, 1);
    }
    if (text.length()) messages.push_back(text);
    return messages;
}

String sendText(https::Client& client, int64_t chat, const String& html)
{
    for (const String& message : split(linksToHtml(html))) {
        JsonDocument body, reply;
        body["chat_id"] = chat;
        body["text"] = message;
        body["parse_mode"] = "HTML";
        String error = callJson(client, "sendMessage", body, reply);
        if (error.indexOf("can't parse entities") >= 0) {
            Serial.printf("[Telegram] HTML refused (%s), sending as text\n", error.c_str());
            body.remove("parse_mode");
            body["text"] = plain(message);
            error = callJson(client, "sendMessage", body, reply);
        }
        if (error.length()) return "Error: " + error;
    }
    return "sent";
}

// A file to a chat as multipart/form-data: sendPhoto (field "photo", JPEG) or sendVoice (field
// "voice", Ogg Opus).
String sendFile(https::Client& client, int64_t chat, const char* method, const char* field, const char* filename,
                const char* contentType, const uint8_t* data, size_t length, const String& caption)
{
    constexpr const char* BOUNDARY = "AssistantBoundary7f3a9c";
    String before = String("--") + BOUNDARY + "\r\nContent-Disposition: form-data; name=\"chat_id\"\r\n\r\n" +
                    number(chat) + "\r\n";
    if (caption.length())
        before += String("--") + BOUNDARY + "\r\nContent-Disposition: form-data; name=\"caption\"\r\n\r\n" +
                  utf8::prefix(caption, 1000) + "\r\n";
    before += String("--") + BOUNDARY + "\r\nContent-Disposition: form-data; name=\"" + field + "\"; filename=\"" +
              filename + "\"\r\nContent-Type: " + contentType + "\r\n\r\n";
    const String after = String("\r\n--") + BOUNDARY + "--\r\n";
    JsonDocument reply;
    const String error = call(client, method, (String("multipart/form-data; boundary=") + BOUNDARY).c_str(),
                              {part(before), Part{data, length}, part(after)}, 2 * TELEGRAM_TIMEOUT_MS, reply);
    return error.length() ? "Error: " + error : "sent";
}

// Downloads a file the owner sent (voice note, photo) by its file_id into `data` (a sink that
// takes at least `maxBytes`). Returns "" or an error.
String download(https::Client& client, const char* fileId, size_t maxBytes, PsramSink& data)
{
    JsonDocument body, reply;
    body["file_id"] = fileId;
    const String error = callJson(client, "getFile", body, reply);
    if (error.length()) return error;
    const char* path = reply["result"]["file_path"];
    if (!path) return "file not available at Telegram";
    if ((reply["result"]["file_size"] | 0) > int(maxBytes)) return "file too large";
    const int status = exchange(client, String("GET /file/bot") + TELEGRAM_BOT_TOKEN + "/" + path + " HTTP/1.1\r\nHost: " +
                                            TELEGRAM_HOST + "\r\nConnection: keep-alive\r\n",
                                {}, 2 * TELEGRAM_TIMEOUT_MS, data);
    if (status != 200) return "Download fehlgeschlagen (" + (status > 0 ? "HTTP " + String(status) : HTTPClient::errorToString(status)) + ")";
    return "";
}

// --- Polling and the chat (all in the registry's worker) ------------------------------

volatile bool idle = false;
SemaphoreHandle_t sendLock = xSemaphoreCreateMutex();  // send() from several tasks: one TLS at a time

// The polling connection, kept between polls while this holds the TLS gate.
https::Client& poller()
{
    static https::Client client(TELEGRAM_HOST, TELEGRAM_CA);
    return client;
}
bool holding = false;     // the TLS gate
uint32_t retryAt = 0;     // after handing the gate over or an error: not before
int failures = 0;
bool commandsSet = false;
int64_t offset = -1;      // next update_id; in the NVS, so no message runs twice after a restart

// One chat per person: a Responses thread until it is quiet for TELEGRAM_THREAD_MS.
struct Chat {
    String person;  // assistant/people id
    int64_t account = 0;  // the person's Telegram user id = the private chat's id
    String thread, transcript;
    bool chatting = false;
    time_t started = 0;
    uint32_t lastMessageAt = 0;
};
std::vector<Chat> chats;
Chat* current = nullptr;  // the chat of the message being handled

Chat& chatOf(const String& person, int64_t account)
{
    for (Chat& chat : chats)
        if (chat.person == person) {
            chat.account = account;
            return chat;
        }
    Chat chat;
    chat.person = person;
    chat.account = account;
    chats.push_back(chat);
    return chats.back();
}

// Telegram accounts not in the register that wrote to the bot: the admin hears of each once,
// and its next message to the assistant carries them, so it can have them registered.
std::vector<int64_t> strangersReported;
String strangers;

// The Telegram account of a person ("" = the admin), 0 if none.
int64_t accountOf(const String& person)
{
    people::Person p;
    if (person.isEmpty() || !people::find(person, p)) return OWNER;
    return p.telegram;
}

void release()
{
    poller().stop();
    if (!holding) return;
    xSemaphoreGive(tls_gate::mutex());
    holding = false;
}

void saveOffset(int64_t next)
{
    offset = next;
    Preferences prefs;
    if (!prefs.begin("telegram", false)) return;
    prefs.putLong64("offset", next);
    prefs.end();
}

void reply(const String& html)
{
    const String sent = sendText(poller(), current->account, html);
    if (sent != "sent") Serial.printf("[Telegram] answer not delivered: %s\n", sent.c_str());
}

// The chats not consolidated yet, in the flash: a restart (flashing, crash) must not lose them.
constexpr const char* CHAT_FILE = "/telegram_chat.json";

void saveChats()
{
    JsonDocument doc(PsramJson::instance());
    JsonArray list = doc["chats"].to<JsonArray>();
    for (const Chat& chat : chats) {
        if (chat.transcript.isEmpty()) continue;
        JsonObject entry = list.add<JsonObject>();
        entry["person"] = chat.person;
        entry["started"] = chat.started;
        entry["transcript"] = chat.transcript;
    }
    if (list.size() == 0) LittleFS.remove(CHAT_FILE);
    else if (!fileutil::saveJson(CHAT_FILE, doc)) Serial.println("[Telegram] chat could not be saved");
}

// After a restart: chats left in the flash go to the memory (their threads are gone).
void restoreChats()
{
    if (!LittleFS.exists(CHAT_FILE)) return;
    File file = LittleFS.open(CHAT_FILE, "r");
    JsonDocument doc(PsramJson::instance());
    const bool read = file && !deserializeJson(doc, file);
    file.close();
    if (read && doc["transcript"].is<const char*>()) {  // one chat, the admin's (before 28.09.)
        memory::consolidate(doc["transcript"] | "", doc["started"].as<long long>(), people::admin(), memory::TELEGRAM);
    } else if (read) {
        for (JsonObjectConst chat : doc["chats"].as<JsonArrayConst>())
            memory::consolidate(chat["transcript"] | "", chat["started"].as<long long>(), chat["person"] | "",
                                memory::TELEGRAM);
    }
    if (read) Serial.println("[Telegram] chats of the last start handed to the memory");
    LittleFS.remove(CHAT_FILE);
}

// The chat so far goes to the memory like a conversation.
void archive(Chat& chat)
{
    if (chat.transcript.length()) memory::consolidate(chat.transcript, chat.started, chat.person, memory::TELEGRAM);
    chat.transcript = "";
    chat.started = time(nullptr);
    memory::setOngoing(chat.person, "");
    saveChats();
}

// Until the chat is consolidated, every prompt with this person (also a voice conversation's)
// gets its end.
void showOngoing(const Chat& chat)
{
    constexpr size_t TAIL = 3000;
    String tail = chat.transcript;
    if (tail.length() > TAIL) {
        const int line = tail.indexOf('\n', tail.length() - TAIL);
        tail = "…\n" + tail.substring(line < 0 ? tail.length() - TAIL : line + 1);
    }
    memory::setOngoing(chat.person, "Ongoing Telegram chat with " + people::name(chat.person) + " since " +
                                        timeutil::format(chat.started, "%H:%M") + " (not summarized yet):\n" + tail);
}

void endChat(Chat& chat)
{
    archive(chat);
    chat.thread = "";
    chat.chatting = false;
}

void setCommands()
{
    JsonDocument body, answer;
    JsonArray commands = body["commands"].to<JsonArray>();
    const char* const list[][2] = {{"neu", "Neues Gespräch beginnen"},
                                   {"aufgaben", "Geplante Aufgaben"},
                                   {"skills", "Was du mir beigebracht hast"},
                                   {"hilfe", "Was ich per Telegram kann"}};
    for (const auto& entry : list) {
        JsonObject command = commands.add<JsonObject>();
        command["command"] = entry[0];
        command["description"] = entry[1];
    }
    const String error = callJson(poller(), "setMyCommands", body, answer);
    if (error.length()) Serial.printf("[Telegram] command menu: %s\n", error.c_str());
}

// "/neu", "/neu@JarvisBot": true if handled here, without the model.
bool command(const String& text)
{
    String name = text.substring(1);
    for (const char stop : {' ', '@', '\n'})
        if (name.indexOf(stop) >= 0) name = name.substring(0, name.indexOf(stop));
    name.toLowerCase();
    if (name == "start" || name == "hilfe" || name == "help") {
        reply(HELP);
    } else if (name == "neu") {
        const bool had = current->chatting;
        endChat(*current);
        reply(had ? "Neues Gespräch. Das bisherige habe ich mir gemerkt." : "Neues Gespräch.");
    } else if (name == "aufgaben") {
        reply("<b>Geplante Aufgaben</b>\n" + escape(tools::run("list_tasks", "{}", tools::Agent, current->person)));
    } else if (name == "skills") {
        const String skills = memory::skills();
        String text;
        for (int start = 0; start < int(skills.length());) {
            int end = skills.indexOf('\n', start);
            if (end < 0) end = skills.length();
            text += "• " + escape(skills.substring(start, end)) + "\n";
            start = end + 1;
        }
        reply(skills.length() ? "<b>Skills</b>\n" + text
                              : String("Noch keine Skills. Bring mir einen bei, etwa: „Wenn ich Guten Morgen "
                                       "schreibe, dann …“"));
    } else {
        return false;
    }
    Serial.printf("[Telegram] command /%s\n", name.c_str());
    return true;
}

void typing()
{
    JsonDocument body, ignored;
    body["chat_id"] = current->account;
    body["action"] = "typing";
    callJson(poller(), "sendChatAction", body, ignored);
}

// The answer read out for a voice reply: without markup and links, cut when long (the text
// comes as well). "" if it has nothing to say.
String spoken(const String& answer)
{
    // The web search's sources "([site](url))" are not read out, other links only by their text.
    String text = answer;
    for (int at; (at = text.indexOf("([")) >= 0;) {
        const int end = text.indexOf("))", at);
        if (end < 0) break;
        text.remove(at, end + 2 - at);
    }
    text = plain(linksToHtml(text), false);
    // Bare addresses neither.
    for (const char* scheme : {"https://", "http://"}) {
        for (int at; (at = text.indexOf(scheme)) >= 0;) {
            int end = at;
            while (end < int(text.length()) && !isspace(uint8_t(text[end]))) ++end;
            text.remove(at, end - at);
        }
    }
    text.replace(" .", ".");
    text.replace(" ,", ",");
    text.replace("• ", "");
    text.trim();
    if (text.length() > TELEGRAM_SPOKEN_CHARS)
        text = utf8::prefix(text, TELEGRAM_SPOKEN_CHARS) + " … Den Rest findest du im Chat.";
    return text;
}

// A message of a person of the household (the current chat): `text` (typed, transcribed or a
// photo's caption), `voice` (it came as a voice note: the answer comes as one, too), `image` (a
// photo as data URL), `file` (a document).
void chat(const String& text, bool voice, const String& image, const agent::File* file = nullptr)
{
    Chat& chat = *current;
    poller().stop();  // the agent's TLS (OpenAI, mail) needs the internal RAM
    if (!chat.chatting) {
        chat.chatting = true;
        chat.started = time(nullptr);
    }
    Serial.printf("\n[Telegram] %s%s%s: %.300s\n", chat.person.c_str(), voice ? " (voice)" : "",
                  image.length() ? " (photo)" : "", text.c_str());
    const uint32_t started = millis();
    String task = voice ? "[Sprachnachricht] " + text : text;
    if (strangers.length() && chat.person == people::admin()) {  // he can have them registered now
        task = "(System note, not from " + people::name(chat.person) + ": these Telegram accounts are not in the "
               "people register and wrote to the bot; not answered. Register or assign them only when asked, with "
               "add_person or update_person:\n" + strangers + ")\n\n" + task;
        strangers = "";
    }
    const String answer = agent::run(task, CHAT_INSTRUCTIONS, &chat.thread, image, chat.person, file, "Telegram");
    Serial.printf("[Telegram] answer after %lu s: %.300s\n", (millis() - started) / 1000, answer.c_str());
    const bool failed = answer.startsWith("Error");
    PsramSink audio(4 * 1024 * 1024);
    String audioError;
    if (voice && !failed) {
        audioError = openai::speech(spoken(answer), "opus", audio);  // before the reply: poller is closed
        if (audioError.length()) Serial.printf("[Telegram] %s\n", audioError.c_str());
    }
    chat.lastMessageAt = millis();
    const String how = voice ? String("per Telegram, Sprachnachricht")
                       : image.length() ? String("per Telegram, mit Foto")
                       : file ? "per Telegram, mit Datei " + file->name + " (" + String(file->size / 1024) + " KB)"
                              : String("per Telegram");
    chat.transcript += String("Du (") + how + "): " + utf8::prefix(text, MEMORY_TRANSCRIPT_CHARS / 4) + "\nJarvis: " +
                       utf8::prefix(answer, MEMORY_TRANSCRIPT_CHARS / 4) + "\n";
    // Voice: what the assistant understood first, so a misheard word shows.
    reply((voice ? "<i>„" + escape(text) + "“</i>\n\n" : String()) + (failed ? escape(answer) : answer));
    if (voice && !failed && audioError.isEmpty()) {
        const String sent =
            sendFile(poller(), chat.account, "sendVoice", "voice", "assistant.ogg", "audio/ogg", audio.data, audio.size, "");
        if (sent != "sent") Serial.printf("[Telegram] voice answer: %s\n", sent.c_str());
    }
    if (chat.transcript.length() > MEMORY_TRANSCRIPT_CHARS / 2) {
        archive(chat);  // a long chat goes to the memory in parts
    } else {
        showOngoing(chat);
        saveChats();
    }
}

// A document (text file or PDF) into `data`. Returns "" or an error.
String loadDocument(JsonObjectConst document, PsramSink& data)
{
    const String name = document["file_name"] | "Datei", type = document["mime_type"] | "";
    if (!agent::readable(type, name))
        return "Mit dieser Datei (" + name + ") kann ich nichts anfangen. Ich lese Textdateien (JSON, CSV, TXT, …) und PDFs.";
    if ((document["file_size"] | 0) > int(TELEGRAM_DOCUMENT_BYTES))
        return "Die Datei ist zu groß (höchstens " + String(TELEGRAM_DOCUMENT_BYTES / 1024) + " KB)";
    const String error = download(poller(), document["file_id"] | "", TELEGRAM_DOCUMENT_BYTES, data);
    Serial.printf("[Telegram] file %s (%s), %u bytes\n", name.c_str(), type.c_str(), unsigned(data.size));
    return error;
}

// A voice note as text. Returns "" with `error` set if that failed.
String transcribeVoice(JsonObjectConst voice, String& error)
{
    if ((voice["duration"] | 0) > TELEGRAM_VOICE_MAX_S) {
        error = "Die Sprachnachricht ist zu lang (höchstens " + String(TELEGRAM_VOICE_MAX_S / 60) + " Minuten)";
        return "";
    }
    constexpr size_t MAX_BYTES = 3 * 1024 * 1024;
    PsramSink audio(MAX_BYTES + 1);
    error = download(poller(), voice["file_id"] | "", MAX_BYTES, audio);
    poller().stop();  // OpenAI's TLS next
    if (error.length()) return "";
    const uint32_t started = millis();
    const String text = openai::transcribe(audio.data, audio.size, "voice.ogg", error);
    Serial.printf("[Telegram] voice note %d s, %u bytes, recognized after %lu ms\n", voice["duration"] | 0,
                  unsigned(audio.size), millis() - started);
    return text;
}

// The largest size of a photo (Telegram sends several, smallest first) that fits
// TELEGRAM_PHOTO_BYTES, as data URL for the model. "" with `error` set if that failed.
String loadPhoto(JsonArrayConst sizes, String& error)
{
    const char* fileId = nullptr;
    for (JsonObjectConst size : sizes)
        if ((size["file_size"] | 0) <= int(TELEGRAM_PHOTO_BYTES)) fileId = size["file_id"];
    if (!fileId) {
        error = "Das Foto ist zu groß";
        return "";
    }
    PsramSink jpeg(TELEGRAM_PHOTO_BYTES + 1);
    error = download(poller(), fileId, TELEGRAM_PHOTO_BYTES, jpeg);
    if (error.length()) return "";
    size_t length = 0;
    const size_t capacity = (jpeg.size + 2) / 3 * 4 + 1;
    auto* encoded = static_cast<unsigned char*>(heap_caps_malloc(capacity, MALLOC_CAP_SPIRAM | MALLOC_CAP_8BIT));
    if (!encoded || mbedtls_base64_encode(encoded, capacity, &length, jpeg.data, jpeg.size)) {
        free(encoded);
        error = "zu wenig Speicher für das Foto";
        return "";
    }
    String url;
    url.reserve(length + 32);
    url = "data:image/jpeg;base64,";
    url.concat(reinterpret_cast<const char*>(encoded), length);
    free(encoded);
    Serial.printf("[Telegram] photo %u bytes\n", unsigned(jpeg.size));
    return url;
}

// Someone not in the register wrote: the admin hears of it once (with the id he needs to register
// them); the message stays unanswered.
void reportStranger(JsonObjectConst message)
{
    const int64_t from = message["from"]["id"].as<int64_t>();
    String who = String(message["from"]["first_name"] | "") + " " + (message["from"]["last_name"] | "");
    who.trim();
    const String user = message["from"]["username"] | "";
    if (user.length()) who += " (@" + user + ")";
    Serial.printf("[Telegram] message from unknown id %s (%s)\n", number(from).c_str(), who.c_str());
    if (std::find(strangersReported.begin(), strangersReported.end(), from) != strangersReported.end()) return;
    strangersReported.push_back(from);
    String text = message["text"] | "";
    if (text.isEmpty()) text = message["caption"] | "(ohne Text)";
    text = utf8::prefix(text, 300);
    strangers += "- " + who + ", Telegram-ID " + number(from) + ", schrieb: „" + text + "“\n";
    memory::logEvent("", memory::TELEGRAM, "Unbekanntes Telegram-Konto: " + who, "ID " + number(from) + ", schrieb: „" + text + "“");
    const String sent = sendText(poller(), OWNER,
                                 "<b>Unbekanntes Telegram-Konto</b>\n" + escape(who) + " (ID <code>" + number(from) +
                                     "</code>) hat mir geschrieben: „" + escape(text) +
                                     "“\nIch antworte erst, wenn du die Person eingetragen hast. Sag mir einfach, wer "
                                     "das ist (z. B. „Das ist Anna“).");
    if (sent != "sent") Serial.printf("[Telegram] report to the admin: %s\n", sent.c_str());
}

void handle(JsonObjectConst update)
{
    saveOffset(update["update_id"].as<int64_t>() + 1);  // before acting: a crash must not run it twice
    JsonObjectConst message = update["message"];
    if (message.isNull()) return;
    const int64_t from = message["from"]["id"].as<int64_t>(), chatId = message["chat"]["id"].as<int64_t>();
    if (!OWNER) {  // setup: tell the sender his id
        Serial.printf("[Telegram] message from id %s (TELEGRAM_OWNER_ID not set)\n", number(from).c_str());
        sendText(poller(), chatId,
                 "Hallo! Deine Telegram-ID ist <code>" + number(from) +
                     "</code>. Trag sie in secrets.h als TELEGRAM_OWNER_ID ein und spiel die Firmware neu auf; "
                     "danach höre ich nur noch auf dich.");
        return;
    }
    String person = people::byTelegram(from);
    if (chatId != from) {  // a group: nobody's own chat
        Serial.printf("[Telegram] message from group %s ignored\n", number(chatId).c_str());
        return;
    }
    const String text0 = message["text"] | "";
    if (person.isEmpty() && text0.startsWith("/start ")) {  // the link of a welcome mail
        person = people::claimInvite(text0.substring(7), from);
        if (person.length()) {
            current = &chatOf(person, from);
            reply("Hallo " + escape(people::name(person)) + "! Ab jetzt erkenne ich Sie hier. Schreiben Sie mir "
                  "einfach, schicken Sie Sprachnachrichten, Fotos oder Dateien.");
            sendText(poller(), OWNER, escape(people::name(person)) + " hat Telegram mit " ASSISTANT_NAME " verbunden.");
            return;
        }
    }
    if (person.isEmpty()) {
        reportStranger(message);
        return;
    }
    current = &chatOf(person, from);
    const time_t sent = message["date"].as<long long>();
    if (timeutil::synced() && time(nullptr) - sent > TELEGRAM_MAX_AGE_S) {
        reply("Deine Nachricht von " + timeutil::format(sent, "%d.%m. %H:%M") +
              " kam an, als ich nicht erreichbar war. Schick sie bitte noch einmal, wenn sie noch gilt.");
        return;
    }
    String text = message["text"] | "";
    if (text.startsWith("/") && command(text)) return;
    const bool voice = !message["voice"].isNull(), photo = !message["photo"].isNull();
    const bool document = !message["document"].isNull();
    if (!voice && !photo && !document && text.isEmpty()) {
        reply("Das kann ich noch nicht lesen. Schick mir Text, eine Sprachnachricht, ein Foto oder eine Datei.");
        return;
    }
    typing();
    String image, error;
    if (voice) {
        text = transcribeVoice(message["voice"], error);
    } else if (photo) {
        image = loadPhoto(message["photo"], error);
        text = message["caption"] | "Was ist auf diesem Bild?";
    }
    PsramSink data(TELEGRAM_DOCUMENT_BYTES + 1);
    agent::File file{message["document"]["file_name"] | "Datei", message["document"]["mime_type"] | "", nullptr, 0};
    if (document) {
        error = loadDocument(message["document"], data);
        file.data = data.data;
        file.size = data.size;
        // Kept in Drive, so it can be printed or passed on later ("druck das PDF aus").
        poller().stop();  // Drive's TLS next
        if (error.isEmpty()) {
            const String kept = files::store(file.name, file.type, data.data, data.size, "Telegram von " + people::name(person));
            if (kept.length()) Serial.printf("[Telegram] %s not kept: %s\n", file.name.c_str(), kept.c_str());
        }
        text = message["caption"] | "Schau dir diese Datei an und fasse zusammen, was drinsteht.";
    }
    if (error.length()) {
        poller().stop();
        reply(escape("Fehler: " + error));
        return;
    }
    chat(text, voice, image, document ? &file : nullptr);
}

// Background work of the registry's worker (about once a second while no call waits).
void poll()
{
    static bool restored = false;
    if (!restored) {
        restored = true;
        restoreChats();
    }
    for (Chat& chat : chats)
        if (chat.chatting && millis() - chat.lastMessageAt > TELEGRAM_THREAD_MS) endChat(chat);
    // During a conversation Telegram goes on (its TLS is in PSRAM since 28.09.), without the TLS
    // gate: the other background jobs wait for the end of the conversation anyway.
    if (!idle && holding) {
        xSemaphoreGive(tls_gate::mutex());
        holding = false;
    }
    if (idle && holding && tls_gate::wanted()) {  // memory, mail check or a task wants the gate
        release();
        retryAt = millis() + TELEGRAM_YIELD_MS;
        return;
    }
    if (retryAt && int32_t(millis() - retryAt) < 0) return;
    if (idle && !holding) {
        // Only if it is free: the others' jobs are more urgent than the next poll.
        if (xSemaphoreTake(tls_gate::mutex(), 0) != pdTRUE) return;
        holding = true;
        tls_gate::wanted() = false;
    }
    if (offset < 0) {
        Preferences prefs;
        offset = prefs.begin("telegram", true) ? prefs.getLong64("offset", 0) : 0;
        prefs.end();
    }

    JsonDocument body;
    body["offset"] = offset;
    body["timeout"] = TELEGRAM_POLL_S;
    body["allowed_updates"].to<JsonArray>().add("message");
    JsonDocument updates(PsramJson::instance());
    const String error = callJson(poller(), "getUpdates", body, updates, (TELEGRAM_POLL_S + 10) * 1000);
    if (error.length()) {
        if (++failures == 1 || failures % 20 == 0) Serial.printf("[Telegram] %s (%d. time)\n", error.c_str(), failures);
        release();
        retryAt = millis() + TELEGRAM_RETRY_MS;
        return;
    }
    if (failures) Serial.println("[Telegram] reachable again");
    failures = 0;
    if (!commandsSet) {
        commandsSet = true;
        setCommands();
        Serial.println(OWNER ? "[Telegram] ready" : "[Telegram] ready, TELEGRAM_OWNER_ID still missing");
    }
    for (JsonObjectConst update : updates["result"].as<JsonArrayConst>()) handle(update);
    retryAt = 0;
}

// Telegram's own task, made at start while the internal RAM is still free: an agent run takes
// up to a minute, and in the registry's worker it held up the slow tools of a conversation.
void task(void*)
{
    for (;;) {
        if (INBOUND) poll();  // a second speaker leaves the chats to the main the assistant
        tools::runConversationWork();  // between polls, during a conversation (mail check)
        vTaskDelay(pdMS_TO_TICKS(200));
    }
}

}  // namespace

bool configured() { return *TELEGRAM_BOT_TOKEN && OWNER; }

String inviteLink(const String& code)
{
    static SemaphoreHandle_t lock = xSemaphoreCreateMutex();
    static String botName;  // from getMe, once
    if (!configured() || code.isEmpty()) return "";
    xSemaphoreTake(lock, portMAX_DELAY);
    if (botName.isEmpty()) {
        https::Client client(TELEGRAM_HOST, TELEGRAM_CA);
        JsonDocument body, reply;
        body.to<JsonObject>();
        const String error = callJson(client, "getMe", body, reply);
        if (error.isEmpty()) botName = reply["result"]["username"] | "";
        else Serial.printf("[Telegram] getMe: %s\n", error.c_str());
    }
    const String name = botName;
    xSemaphoreGive(lock);
    return name.length() ? "https://t.me/" + name + "?start=" + code : String();
}

void setIdle(bool on) { idle = on; }

String send(const String& html, const String& person)
{
    if (!configured()) return "Error: Telegram is not set up";
    const int64_t account = accountOf(person);
    if (!account) return "Error: no Telegram account registered for " + people::name(person) + ".";
    xSemaphoreTake(sendLock, portMAX_DELAY);
    https::Client client(TELEGRAM_HOST, TELEGRAM_CA);
    const String result = sendText(client, account, html);
    client.stop();
    xSemaphoreGive(sendLock);
    return result;
}

String sendPhoto(const uint8_t* jpeg, size_t length, const String& caption, const String& person)
{
    if (!configured()) return "Error: Telegram is not set up";
    const int64_t account = accountOf(person);
    if (!account) return "Error: no Telegram account registered for " + people::name(person) + ".";
    xSemaphoreTake(sendLock, portMAX_DELAY);
    https::Client client(TELEGRAM_HOST, TELEGRAM_CA);
    const String result = sendFile(client, account, "sendPhoto", "photo", "assistant.jpg", "image/jpeg", jpeg, length, caption);
    client.stop();
    xSemaphoreGive(sendLock);
    return result;
}

String sendDocument(const uint8_t* data, size_t length, const String& filename, const String& mime,
                    const String& caption, const String& person)
{
    if (!configured()) return "Error: Telegram is not set up";
    const int64_t account = accountOf(person);
    if (!account) return "Error: no Telegram account registered for " + people::name(person) + ".";
    xSemaphoreTake(sendLock, portMAX_DELAY);
    https::Client client(TELEGRAM_HOST, TELEGRAM_CA);
    const String result = sendFile(client, account, "sendDocument", "document", filename.c_str(), mime.c_str(), data,
                                   length, caption);
    client.stop();
    xSemaphoreGive(sendLock);
    return result;
}

void addTools()
{
    if (!*TELEGRAM_BOT_TOKEN) return;
    // Also without an owner: it tells the sender his id. Stack like the registry's worker (agent,
    // TLS, JSON); CPU 1 like the other TLS tasks.
    if (xTaskCreatePinnedToCore(task, "telegram", TASK_STACK_BYTES, nullptr, 1, nullptr, 1) != pdPASS)
        Serial.println("[Telegram] task could not start");
    if (!configured()) return;
    tools::add(tools::Tool{
        "send_telegram",
        "Sends a message to the user's Telegram chat on their phone, or to another person of the household: "
        "links, lists, recipes, addresses, anything to read later ('schick mir das aufs Handy', 'per Telegram', "
        "'schick Anna den Link'). Format with Telegram HTML only: <b>, <i>, <a href=\"...\">text</a>, <code>; no "
        "other tags; write <, > and & in text as &lt; &gt; &amp;. Images go by create_image / send_last_image "
        "with telegram true.",
        R"json({"type": "object", "additionalProperties": false,
            "properties": {"text": {"type": "string"},
                           "to": {"type": "string", "description": "name of a person of the household, '' = the user"}},
            "required": ["text", "to"]})json",
        [](JsonObjectConst arguments, const tools::Context& context) {
            const String to = arguments["to"] | "";
            const String person = to.length() ? people::byName(to) : context.person;
            if (to.length() && person.isEmpty()) return String("Error: " + to + " is not registered as a person.");
            const String from = tools::fromLine(context, person);
            return send((from.length() ? "<i>" + escape(from) + "</i>\n" : String()) + (arguments["text"] | ""), person);
        },
        tools::Slow, tools::Everywhere});
}

}  // namespace telegram
