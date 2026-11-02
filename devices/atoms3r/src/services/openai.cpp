#include "services/openai.h"

#include <HTTPClient.h>
#include <initializer_list>
#include <utility>

#include "ca_certs.h"
#include "config.h"
#include "prompts.h"
#include "secrets.h"
#include "services/https.h"
#include "services/psram_json.h"
#include "system/utf8.h"

namespace openai {
namespace {

constexpr const char* HOST = "api.openai.com";
constexpr const char* BOUNDARY = "AssistantFormBoundary5d2e81";

// One request: header, then the body in pieces.
int send(const char* path, const String& contentType, std::initializer_list<std::pair<const uint8_t*, size_t>> body,
         PsramSink& reply)
{
    size_t length = 0;
    for (const auto& part : body) length += part.second;
    https::Client client(HOST, CA_CERTS);
    client.setTimeout(OPENAI_TIMEOUT_MS);
    if (!client.connect()) return HTTPC_ERROR_CONNECTION_REFUSED;
    const String head = String("POST ") + path + " HTTP/1.1\r\nHost: " + HOST +
                        "\r\nAuthorization: Bearer " + OPENAI_API_KEY + "\r\nContent-Type: " + contentType +
                        "\r\nContent-Length: " + length + "\r\nConnection: close\r\n\r\n";
    bool written = client.write(head);
    for (const auto& part : body) written = written && client.write(part.first, part.second);
    if (!written) return HTTPC_ERROR_SEND_PAYLOAD_FAILED;
    return client.read(reply);
}

const uint8_t* bytes(const String& text) { return reinterpret_cast<const uint8_t*>(text.c_str()); }

}  // namespace

int post(const char* path, const JsonDocument& body, PsramSink& reply)
{
    PsramText payload(body);  // an agent's request (tools, memory) is over 64 KB: not a String
    if (!payload.data) return HTTPC_ERROR_TOO_LESS_RAM;
    // Safety net: invalid UTF-8 or a control character anywhere fails the whole request.
    if (const size_t replaced = utf8::repair(payload.data, payload.size))
        Serial.printf("[OpenAI] %u invalid bytes replaced (UTF-8, control characters)\n", unsigned(replaced));
    return send(path, "application/json", {{reinterpret_cast<const uint8_t*>(payload.data), payload.size}}, reply);
}

int postForm(const char* path, const char* const* fields, const Upload& file, PsramSink& reply)
{
    String before;
    for (const char* const* field = fields; field && *field; field += 2)
        before += String("--") + BOUNDARY + "\r\nContent-Disposition: form-data; name=\"" + field[0] + "\"\r\n\r\n" +
                  field[1] + "\r\n";
    before += String("--") + BOUNDARY + "\r\nContent-Disposition: form-data; name=\"" + file.field +
              "\"; filename=\"" + file.filename + "\"\r\nContent-Type: " + file.contentType + "\r\n\r\n";
    const String after = String("\r\n--") + BOUNDARY + "--\r\n";
    return send(path, String("multipart/form-data; boundary=") + BOUNDARY,
                {{bytes(before), before.length()}, {file.data, file.length}, {bytes(after), after.length()}}, reply);
}

String errorMessage(int status, const PsramSink& reply)
{
    JsonDocument parsed;
    if (status > 0 && reply.data && !deserializeJson(parsed, reply.data, reply.size)) {
        // The safety system's message is long English text: short German instead.
        if (parsed["error"]["code"] == "moderation_blocked") return "rejected by the content filter";
        const char* message = parsed["error"]["message"];
        if (message) return message;
    }
    if (status == HTTPC_ERROR_READ_TIMEOUT)
        return "no answer after " + String(OPENAI_TIMEOUT_MS / 1000) + " s";
    return status < 0 ? HTTPClient::errorToString(status) : "HTTP " + String(status);
}

String speech(const String& text, const char* format, PsramSink& audio)
{
    JsonDocument body;
    body["model"] = TTS_MODEL;
    body["voice"] = LIVE_VOICE;
    body["input"] = text;
    body["instructions"] = TTS_INSTRUCTIONS;
    body["response_format"] = format;
    const int status = post("/v1/audio/speech", body, audio);
    if (status != 200) return "speech synthesis failed (" + errorMessage(status, audio) + ")";
    if (audio.size < 2) return "no audio data";
    return "";
}

String transcribe(const uint8_t* audio, size_t length, const char* filename, String& error)
{
    const char* const fields[] = {"model", TRANSCRIBE_MODEL, "language", "de", "response_format", "json", nullptr};
    PsramSink reply(256 * 1024);
    const int status = postForm("/v1/audio/transcriptions", fields,
                                Upload{"file", filename, "application/octet-stream", audio, length}, reply);
    JsonDocument parsed;
    if (status != 200 || deserializeJson(parsed, reply.data, reply.size)) {
        error = "speech recognition failed (" + errorMessage(status, reply) + ")";
        return "";
    }
    String text = parsed["text"] | "";
    text.trim();
    if (text.isEmpty()) error = "nothing understandable in the voice note";
    return text;
}

void addFunction(JsonArray tools, const char* name, const char* description, JsonObject& properties,
                 JsonArray& required)
{
    JsonObject tool = tools.add<JsonObject>();
    tool["type"] = "function";
    tool["name"] = name;
    tool["description"] = description;
    JsonObject parameters = tool["parameters"].to<JsonObject>();
    parameters["type"] = "object";
    parameters["additionalProperties"] = false;
    properties = parameters["properties"].to<JsonObject>();
    required = parameters["required"].to<JsonArray>();
}

}  // namespace openai
