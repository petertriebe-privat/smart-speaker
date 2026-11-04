#pragma once

#include <Arduino.h>
#include <ArduinoJson.h>

// Image jobs: generate an image with the OpenAI Images API (JPEG, ~30-60 s), then print it
// (services/printer), mail it as an attachment (services/mail) and/or send it to the owner's
// Telegram chat (services/telegram). Tools: create_image,
// send_last_image (deliver the last image again without generating it).
// Jobs wait in a queue and run in their own task: the tool answers right away. The image is
// generated during a conversation, but printed / mailed only after it (the upload dropped
// the GPT Live connection). The last image stays in PSRAM (~100-300 KB of 8 MB).
namespace images {

// Registers create_image and send_last_image (assistant/tools) if a printer, a mailbox or
// Telegram is set.
void addTools();
// Outcome of a finished job for the main loop, reported once. `spoken`: a failure was already
// announced on the speaker (no conversation was open).
bool takeResult(String& message, bool& ok, bool& spoken);

}  // namespace images
