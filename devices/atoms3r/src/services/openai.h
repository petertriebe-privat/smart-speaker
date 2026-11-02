#pragma once

#include <Arduino.h>
#include <ArduinoJson.h>

#include "services/psram_sink.h"

// HTTPS requests to api.openai.com (Responses, speech, transcription, images), checked against
// ca_certs.h. The voice conversation uses its own WebSocket (voice/live_session).
namespace openai {

// POSTs `body` to `path` (e.g. "/v1/responses"); the reply lands in `reply`. Returns the HTTP
// status, or a negative HTTPClient error (HTTPC_ERROR_READ_TIMEOUT after OPENAI_TIMEOUT_MS).
// Blocks until then: reasoning, web search and image generation take a while.
int post(const char* path, const JsonDocument& body, PsramSink& reply);

// A file for postForm().
struct Upload {
    const char* field;
    const char* filename;
    const char* contentType;
    const uint8_t* data;
    size_t length;
};
// POSTs multipart/form-data: text fields ({name, value} pairs, nullptr-terminated) and one file.
int postForm(const char* path, const char* const* fields, const Upload& file, PsramSink& reply);

// Error text for a failed post(): the API's message if there is one.
String errorMessage(int status, const PsramSink& reply);

// Text to speech in the assistant's voice (TTS_MODEL, LIVE_VOICE, TTS_INSTRUCTIONS). `format`: "pcm"
// (24 kHz PCM16) or "opus" (Ogg Opus, e.g. a Telegram voice note). Returns "" or an error text.
String speech(const String& text, const char* format, PsramSink& audio);
// Speech to text (TRANSCRIBE_MODEL, German). `filename` tells the format ("voice.ogg"). Returns
// the text, or "" with `error` set.
String transcribe(const uint8_t* audio, size_t length, const char* filename, String& error);

// Adds a Responses function tool to `tools` (parameters: an object without extra properties);
// the caller fills in `properties` and `required`.
void addFunction(JsonArray tools, const char* name, const char* description, JsonObject& properties,
                 JsonArray& required);

}  // namespace openai
