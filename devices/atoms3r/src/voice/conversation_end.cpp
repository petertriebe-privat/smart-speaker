#include "voice/conversation_end.h"

#include <ArduinoJson.h>

#include "assistant/tools.h"
#include "ca_certs.h"
#include "config.h"
#include "prompts.h"
#include "secrets.h"
#include "services/https.h"
#include "services/psram_json.h"
#include "services/psram_sink.h"
#include "system/utf8.h"
#include "voice/live_session.h"

namespace conversation_end {
namespace {

constexpr const char* HOST = "api.openai.com";

SemaphoreHandle_t lock = nullptr;
String pendingText;
uint32_t pendingStamp = 0, answeredStamp = 0;
bool answeredOver = false, busy = false;

https::Client& client()
{
    static https::Client connection(HOST, CA_CERTS);
    return connection;
}

// One request over the kept connection; a stale one (the server closed it) is opened again once.
int post(const char* path, const String& payload, PsramSink& reply)
{
    for (int attempt = 0; attempt < 2; ++attempt) {
        https::Client& connection = client();
        connection.setTimeout(15000);
        if (!connection.connect()) return -1;
        const String head = String("POST ") + path + " HTTP/1.1\r\nHost: " + HOST + "\r\nAuthorization: Bearer " +
                            OPENAI_API_KEY + "\r\nContent-Type: application/json\r\nContent-Length: " +
                            payload.length() + "\r\nConnection: keep-alive\r\n\r\n";
        reply.size = 0;
        const int status = connection.write(head) && connection.write(payload) ? connection.read(reply) : -1;
        if (status > 0) return status;
        connection.stop();
    }
    return -1;
}

// The Decisions API: 0.2-0.9 s and 10 of 10 right on test endings, Responses with reasoning
// 1.1-2.6 s (19.10.2026). False: no answer, Responses decides.
bool decide(const String& text, bool& over)
{
    JsonDocument body(PsramJson::instance());
    body["model"] = DECISIONS_MODEL;
    body["input"] = text;
    JsonObject question = body["questions"].add<JsonObject>();
    question["type"] = "choice";
    question["name"] = "over";
    question["instructions"] = END_CHECK_INSTRUCTIONS;
    JsonObject yes = question["choices"].add<JsonObject>();
    yes["value"] = "yes";
    yes["description"] = "the conversation is over";
    JsonObject no = question["choices"].add<JsonObject>();
    no["value"] = "no";
    no["description"] = "it goes on";
    String payload;
    serializeJson(body, payload);
    utf8::repair(payload);
    PsramSink reply(16 * 1024);
    if (post("/v1/decisions", payload, reply) != 200) return false;
    JsonDocument response(PsramJson::instance());
    if (deserializeJson(response, reply.data, reply.size)) return false;
    JsonObjectConst answer = response["answers"][0];
    if (strcmp(answer["type"] | "", "choice")) return false;
    over = !strcmp(answer["choice"] | "", "yes");
    return true;
}

// In the registry's worker.
void judge()
{
    String text;
    uint32_t stamp;
    xSemaphoreTake(lock, portMAX_DELAY);
    text = pendingText;
    stamp = pendingStamp;
    xSemaphoreGive(lock);

    JsonDocument body(PsramJson::instance());
    body["model"] = DELEGATION_MODEL;
    body["instructions"] = END_CHECK_INSTRUCTIONS;
    body["input"] = text;
    body["store"] = false;
    // "none" answered at random on a real goodbye (2-3 of 6 wrong); "low" 8 of 8 right, ~1.6 s.
    body["max_output_tokens"] = 200;  // includes the reasoning
    body["reasoning"]["effort"] = "low";
    String payload;
    serializeJson(body, payload);
    utf8::repair(payload);

    const uint32_t started = millis();
    PsramSink reply(64 * 1024);
    // Nothing transcribed yet (The assistant's first words still on their way): the API rejects an empty
    // input with HTTP 400, and nothing said is no goodbye.
    text.trim();
    bool over = false;
    if (text.length() && decide(text, over)) {
        Serial.printf("[End?] %s (decisions, %lu ms)\n", over ? "yes" : "no", millis() - started);
        xSemaphoreTake(lock, portMAX_DELAY);
        answeredStamp = stamp;
        answeredOver = over;
        busy = false;
        xSemaphoreGive(lock);
        return;
    }
    const int status = text.length() ? post("/v1/responses", payload, reply) : 0;
    JsonDocument response(PsramJson::instance());
    if (!status) {
        Serial.println("[End?] no (nothing said yet)");
    } else if (status == 200 && !deserializeJson(response, reply.data, reply.size)) {
        live::countBackend(response["usage"], true);
        String answer;
        for (JsonObjectConst item : response["output"].as<JsonArrayConst>())
            if (!strcmp(item["type"] | "", "message"))
                for (JsonObjectConst part : item["content"].as<JsonArrayConst>()) answer += part["text"] | "";
        answer.toUpperCase();
        over = answer.indexOf("YES") >= 0;
        Serial.printf("[End?] %s (%lu ms)\n", over ? "yes" : "no", millis() - started);
    } else {
        Serial.printf("[End?] error (HTTP %d, %lu ms)\n", status, millis() - started);
    }
    xSemaphoreTake(lock, portMAX_DELAY);
    answeredStamp = stamp;
    answeredOver = over;
    busy = false;
    xSemaphoreGive(lock);
}

// Background work of the worker: opens the connection early in a session (the handshake takes
// ~2.5 s, the first check is then fast), drops it after the session.
void keepConnection()
{
    static bool opened = false;
    if (live::open()) {
        if (!opened && live::state() == live::State::Live) {
            opened = true;
            client().setTimeout(15000);
            client().connect();
        }
    } else if (opened) {
        opened = false;
        client().stop();
    }
}

}  // namespace

void begin()
{
    lock = xSemaphoreCreateMutex();
    tools::addBackgroundWork(keepConnection);
}

void check(const String& transcript, uint32_t stamp)
{
    if (!lock || !DELEGATION_MODEL) return;
    xSemaphoreTake(lock, portMAX_DELAY);
    const bool start = !busy;
    if (start) {
        busy = true;
        pendingText = utf8::prefix(transcript.substring(transcript.length() > 800 ? transcript.length() - 800 : 0), 900);
        pendingStamp = stamp;
    }
    xSemaphoreGive(lock);
    if (start && !tools::post(judge)) {
        xSemaphoreTake(lock, portMAX_DELAY);
        busy = false;
        xSemaphoreGive(lock);
    }
}

bool over(uint32_t stamp)
{
    if (!lock) return false;
    xSemaphoreTake(lock, portMAX_DELAY);
    const bool result = answeredOver && answeredStamp == stamp;
    xSemaphoreGive(lock);
    return result;
}

void reset()
{
    if (!lock) return;
    xSemaphoreTake(lock, portMAX_DELAY);
    answeredOver = false;
    answeredStamp = 0;
    xSemaphoreGive(lock);
}

}  // namespace conversation_end
