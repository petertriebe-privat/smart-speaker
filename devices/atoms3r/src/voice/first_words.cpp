#include "voice/first_words.h"

#include <ArduinoJson.h>
#include <esp_heap_caps.h>
#include <time.h>

#include <HTTPClient.h>
#include <initializer_list>
#include <utility>

#include "assistant/tools.h"
#include "audio/audio.h"
#include "ca_certs.h"
#include "config.h"
#include "system/name.h"
#include "prompts.h"
#include "secrets.h"
#include "services/https.h"
#include "services/openai.h"
#include "services/psram_json.h"
#include "services/psram_sink.h"
#include "system/utf8.h"
#include "voice/quick_home.h"

namespace first_words {
namespace {

// One connection to OpenAI for the transcription, the Decisions question and a spoken answer,
// opened at the wake word while the user speaks (prepare()) and closed after the verdict (its
// ~40 KB of internal RAM): a TLS handshake costs the ESP32 1-2 s, and with a new connection per
// request the decision took 4.4 s after the words (28.10.2026).
constexpr const char* HOST = "api.openai.com";
constexpr const char* BOUNDARY = "AssistantFormBoundary5d2e81";

https::Client& client()
{
    static https::Client connection(HOST, CA_CERTS);
    return connection;
}

void preconnect()
{
    const uint32_t started = millis();
    client().setTimeout(OPENAI_TIMEOUT_MS);
    const bool ok = client().connect();
    Serial.printf("[Quick] connection to OpenAI %s (%lu ms)\n", ok ? "open" : "failed", millis() - started);
}

// One request over the kept connection; a stale one (the server closed it) is opened again once.
int request(const char* path, const String& contentType, std::initializer_list<std::pair<const uint8_t*, size_t>> body,
            PsramSink& reply)
{
    size_t length = 0;
    for (const auto& part : body) length += part.second;
    for (int attempt = 0; attempt < 2; ++attempt) {
        https::Client& connection = client();
        connection.setTimeout(OPENAI_TIMEOUT_MS);
        if (!connection.connect()) return HTTPC_ERROR_CONNECTION_REFUSED;
        const String head = String("POST ") + path + " HTTP/1.1\r\nHost: " + HOST + "\r\nAuthorization: Bearer " +
                            OPENAI_API_KEY + "\r\nContent-Type: " + contentType + "\r\nContent-Length: " + length +
                            "\r\nConnection: keep-alive\r\n\r\n";
        reply.size = 0;
        bool written = connection.write(head);
        for (const auto& part : body) written = written && connection.write(part.first, part.second);
        const int status = written ? connection.read(reply) : -1;
        if (status > 0) return status;
        connection.stop();
    }
    return -1;
}

int requestJson(const char* path, const JsonDocument& body, PsramSink& reply)
{
    String payload;
    serializeJson(body, payload);
    utf8::repair(payload);
    return request(path, "application/json", {{reinterpret_cast<const uint8_t*>(payload.c_str()), payload.length()}},
                   reply);
}

// Speech to text with the commands as a hint; "" with `error` set on failure, "" without for
// nothing understood.
String transcribe(const uint8_t* wavData, size_t length, String& error)
{
    String before;
    const char* const fields[] = {"model", TRANSCRIBE_MODEL, "language", "de", "response_format", "json",
                                  "prompt", QUICK_TRANSCRIBE_HINT, nullptr};
    for (const char* const* field = fields; *field; field += 2)
        before += String("--") + BOUNDARY + "\r\nContent-Disposition: form-data; name=\"" + field[0] + "\"\r\n\r\n" +
                  field[1] + "\r\n";
    before += String("--") + BOUNDARY +
              "\r\nContent-Disposition: form-data; name=\"file\"; filename=\"words.wav\"\r\nContent-Type: audio/wav\r\n\r\n";
    const String after = String("\r\n--") + BOUNDARY + "--\r\n";
    PsramSink reply(64 * 1024);
    const int status = request("/v1/audio/transcriptions", String("multipart/form-data; boundary=") + BOUNDARY,
                               {{reinterpret_cast<const uint8_t*>(before.c_str()), before.length()},
                                {wavData, length},
                                {reinterpret_cast<const uint8_t*>(after.c_str()), after.length()}},
                               reply);
    JsonDocument parsed;
    if (status != 200 || deserializeJson(parsed, reply.data, reply.size)) {
        error = "speech recognition failed (" + openai::errorMessage(status, reply) + ")";
        return "";
    }
    String text = parsed["text"] | "";
    text.trim();
    return text;
}

// The assistant's voice (TTS_MODEL, 24 kHz PCM16) into `audio`; "" or an error text.
String speech(const String& text, PsramSink& audio)
{
    JsonDocument body;
    body["model"] = TTS_MODEL;
    body["voice"] = LIVE_VOICE;
    body["input"] = text;
    body["instructions"] = TTS_INSTRUCTIONS;
    body["response_format"] = "pcm";
    const int status = requestJson("/v1/audio/speech", body, audio);
    if (status != 200 || audio.size < 2) return "speech synthesis failed (" + openai::errorMessage(status, audio) + ")";
    return "";
}

// The job and its result, between the main loop and the worker.
portMUX_TYPE mux = portMUX_INITIALIZER_UNLOCKED;
const int16_t* jobPcm = nullptr;
size_t jobSamples = 0;
volatile bool running = false, ready = false, cancelled = false;
Result result;

// The commands the M5 runs itself; everything else goes to GPT Live (the Echo knows more: music,
// video, timers). Descriptions as the Echo's intentChoices.
struct Choice {
    const char* value;
    const char* description;
};
constexpr Choice CHOICES[] = {
    {"none", "Anything else: a question, a conversation, more than one command, or unsure."},
    {"home", "Switch or set one thing in the house: lights, lamps, plugs, blinds or shutters, fans, heating, a "
             "scene ('Licht in der Küche aus', 'Rollos runter'). Not the TV, not music."},
    {"time", "Ask only for the current time ('Wie spät ist es?', 'Wie viel Uhr haben wir?')."},
    {"date", "Ask only for today's date or weekday ('Welcher Tag ist heute?', 'Den Wievielten haben wir?')."},
};
constexpr float INTENT_SURE = 0.35f;  // as the Echo's intentSure

// A 24 kHz mono WAV of pcm in PSRAM (the caller frees it).
uint8_t* wav(const int16_t* pcm, size_t samples, size_t& length)
{
    const uint32_t data = samples * 2;
    length = 44 + data;
    auto* out = static_cast<uint8_t*>(heap_caps_malloc(length, MALLOC_CAP_SPIRAM | MALLOC_CAP_8BIT));
    if (!out) return nullptr;
    auto put32 = [&](size_t at, uint32_t v) { memcpy(out + at, &v, 4); };
    auto put16 = [&](size_t at, uint16_t v) { memcpy(out + at, &v, 2); };
    memcpy(out, "RIFF", 4);
    put32(4, 36 + data);
    memcpy(out + 8, "WAVEfmt ", 8);
    put32(16, 16);
    put16(20, 1);  // PCM
    put16(22, 1);  // mono
    put32(24, audio::API_RATE);
    put32(28, audio::API_RATE * 2);
    put16(32, 2);
    put16(34, 16);
    memcpy(out + 36, "data", 4);
    put32(40, data);
    memcpy(out + 44, pcm, data);
    return out;
}

// Nothing understood: empty, the wake word alone (its rest before the words), or the hint read
// back (the model returns its prompt on noise, the Echo 16.10.2026).
bool nothing(const String& text)
{
    String t = text;
    t.toLowerCase();
    if (t.indexOf("stopp") >= 0 && t.indexOf("zurück") >= 0 && t.indexOf("leiser") >= 0) return true;
    t.replace("hey", "");
    // the wake word or the name alone is nothing either
    int start = 0;
    String rest;
    while (start < (int)t.length()) {
        int end = t.indexOf(' ', start);
        if (end < 0) end = t.length();
        const String word = t.substring(start, end);
        if (!name::isName(word)) rest += word;
        start = end + 1;
    }
    for (const char* c : {".", ",", "!", "?", " "}) rest.replace(c, "");
    return rest.isEmpty();
}

// The Decisions API: which command, and (after the wake word) were the words meant for the assistant.
// `addressed` -1 without an answer.
String decide(const String& text, float& addressed)
{
    addressed = -1;
    JsonDocument body(PsramJson::instance());
    body["model"] = DECISIONS_MODEL;
    body["input"] = text;
    JsonObject command = body["questions"].add<JsonObject>();
    command["type"] = "choice";
    command["name"] = "command";
    command["instructions"] = QUICK_INTENT_INSTRUCTIONS;
    for (const Choice& c : CHOICES) {
        JsonObject choice = command["choices"].add<JsonObject>();
        choice["value"] = c.value;
        choice["description"] = c.description;
    }
    JsonObject meant = body["questions"].add<JsonObject>();
    meant["type"] = "predicate";
    meant["name"] = "addressed";
    meant["instructions"] = QUICK_ADDRESSED_QUESTION;
    const uint32_t started = millis();
    PsramSink reply(16 * 1024);
    const int status = requestJson("/v1/decisions", body, reply);
    JsonDocument response(PsramJson::instance());
    if (status != 200 || deserializeJson(response, reply.data, reply.size)) {
        Serial.printf("[Quick] intent of \"%s\": no answer (%s)\n", text.c_str(), openai::errorMessage(status, reply).c_str());
        return "";
    }
    String choice;
    for (JsonObjectConst answer : response["answers"].as<JsonArrayConst>()) {
        const char* type = answer["type"] | "";
        if (!strcmp(type, "choice")) {
            const float sure = answer["confidence"] | 0.0f;
            Serial.printf("[Quick] intent of \"%s\": %s (%.2f, %lu ms)\n", text.c_str(), (const char*)(answer["choice"] | ""),
                          sure, millis() - started);
            if (sure >= INTENT_SURE) choice = answer["choice"] | "";
        } else if (!strcmp(type, "predicate") && !strcmp(answer["name"] | "", "addressed") && answer["probability"].is<float>()) {
            addressed = answer["probability"];
            Serial.printf("[Quick] meant for the assistant: %.2f\n", addressed);
        }
    }
    return choice;
}

// A home command through HA's own sentences; Unknown: HA did not understand, GPT Live answers.
Verdict home(const String& text, String& detail)
{
    const quick_home::Result r = quick_home::run(text, detail);
    if (r == quick_home::Result::Done) return Verdict::Done;
    if (r == quick_home::Result::Failed) return Verdict::Failed;
    return Verdict::Live;
}

void work()
{
    portENTER_CRITICAL(&mux);
    const int16_t* pcm = jobPcm;
    const size_t samples = jobSamples;
    portEXIT_CRITICAL(&mux);
    Result r;
    size_t length = 0;
    uint8_t* file = wav(pcm, samples, length);
    String error;
    const uint32_t started = millis();
    if (file) r.text = transcribe(file, length, error);
    free(file);
    if (!file || error.length()) {
        // the transcription failed: GPT Live hears the words itself
        Serial.printf("[Quick] transcription: %s\n", file ? error.c_str() : "out of memory");
        r.verdict = Verdict::Live;
    } else if (nothing(r.text)) {
        Serial.printf("[Quick] transcription: nothing understood\n");
        r.verdict = Verdict::NoWords;
    } else {
        Serial.printf("[Quick] heard \"%s\" (%lu ms, %.1f s audio)\n", r.text.c_str(), millis() - started,
                      float(samples) / audio::API_RATE);
        if (quick_home::candidate(r.text)) {
            r.verdict = home(r.text, r.detail);
        } else {
            float addressed;
            const String choice = decide(r.text, addressed);
            if (choice == "home") r.verdict = home(r.text, r.detail);
            else if (choice == "time" || choice == "date") {
                PsramSink spoken(2 * 1024 * 1024);
                const String failed = speech(clockAnswer(choice == "date"), spoken);
                if (failed.isEmpty()) {
                    r.verdict = Verdict::Spoken;
                    r.speechSamples = spoken.size / 2;
                    r.speech = reinterpret_cast<int16_t*>(spoken.release());
                } else {
                    Serial.printf("[Quick] %s\n", failed.c_str());
                    r.verdict = Verdict::Live;  // GPT Live knows the time too
                }
            } else if (addressed >= 0 && addressed < FIRST_WORDS_ADDRESSED_NOT) {
                r.verdict = Verdict::NotMeant;
            } else {
                r.verdict = Verdict::Live;
            }
        }
    }
    if (r.verdict != Verdict::NoWords) client().stop();  // its internal RAM, for GPT Live and the tools
    portENTER_CRITICAL(&mux);
    const bool drop = cancelled;
    if (!drop) result = r;
    running = false;
    ready = !drop;
    portEXIT_CRITICAL(&mux);
    if (drop) free(r.speech);
}

}  // namespace

void prepare() { tools::post(preconnect); }

void release()
{
    tools::post([] { client().stop(); });
}

bool judge(const int16_t* pcm, size_t samples)
{
    portENTER_CRITICAL(&mux);
    if (running) {
        portEXIT_CRITICAL(&mux);
        return false;
    }
    jobPcm = pcm;
    jobSamples = samples;
    running = true;
    ready = cancelled = false;
    portEXIT_CRITICAL(&mux);
    if (tools::post(work)) return true;
    running = false;
    return false;
}

bool busy() { return running; }

bool take(Result& out)
{
    portENTER_CRITICAL(&mux);
    if (!ready) {
        portEXIT_CRITICAL(&mux);
        return false;
    }
    ready = false;
    portEXIT_CRITICAL(&mux);
    out = result;  // the worker is done with it
    result = Result{};
    return true;
}

void cancel()
{
    portENTER_CRITICAL(&mux);
    cancelled = running;
    const bool waiting = ready;
    ready = false;
    portEXIT_CRITICAL(&mux);
    if (waiting) {
        free(result.speech);
        result = Result{};
    }
}

String clockAnswer(bool date)
{
    static const char* const WEEKDAYS[] = {"Sonntag", "Montag", "Dienstag", "Mittwoch", "Donnerstag", "Freitag", "Samstag"};
    static const char* const MONTHS[] = {"Januar", "Februar", "März", "April", "Mai", "Juni", "Juli", "August",
                                         "September", "Oktober", "November", "Dezember"};
    const time_t now = time(nullptr);
    struct tm t;
    localtime_r(&now, &t);
    char text[64];
    if (date) snprintf(text, sizeof(text), "Heute ist %s, der %d. %s.", WEEKDAYS[t.tm_wday], t.tm_mday, MONTHS[t.tm_mon]);
    else if (t.tm_min == 0) snprintf(text, sizeof(text), "Es ist %d Uhr.", t.tm_hour);
    else snprintf(text, sizeof(text), "Es ist %d Uhr %d.", t.tm_hour, t.tm_min);
    return text;
}

}  // namespace first_words
