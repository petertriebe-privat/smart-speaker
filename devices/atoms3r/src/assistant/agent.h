#pragma once

#include <Arduino.h>

// The assistant without a voice session: a task (e.g. an e-mail from the owner) goes to the backend
// model over the Responses API with the same tools as in a conversation (assistant/tools.h)
// plus speak. Runs in the mail task and blocks it.
namespace agent {

// Runs the task to the end and returns the model's final answer (or an error text).
// `instructions`: AGENT_INSTRUCTIONS (a mail from the owner), TASK_INSTRUCTIONS (scheduled) or
// CHAT_INSTRUCTIONS (Telegram). `thread` (optional): continues that conversation (the server
// keeps it, previous_response_id; "" = new one) and gets its new state; "" after an error.
// `image` (optional): a picture that comes with the task, as data URL ("data:image/jpeg;base64,...").
// `person`: for whom (assistant/people id; "" = unknown): their profile, their tools, their chat.
// `file` (optional): a document that comes with the task: text files as text, PDFs as file.
struct File {
    String name, type;  // file name, MIME type
    const uint8_t* data;
    size_t size;
};
// `origin`: how the task came in ("Telegram", "E-Mail", "geplante Aufgabe"), for sender lines.
String run(const String& task, const char* instructions = nullptr, String* thread = nullptr,
           const String& image = "", const String& person = "", const File* file = nullptr,
           const String& origin = "");
// Whether run() takes a file of this MIME type / name (text, PDF, or a JPEG/PNG/GIF/WebP image).
bool readable(const String& type, const String& name);
// One question about `file` to the backend model, without tools: what a tool needs from a file it
// cannot give the model itself (a mail attachment in a voice conversation). The answer in German,
// or an error. Blocks for seconds.
String readFile(const File& file, const String& question);

// Says `text` on the speaker in the assistant's voice once no conversation is open (e.g. a failed
// image job). Blocks for the speech request (~2 s): call it from a background task. Returns an
// error text starting with "Error" if it failed.
String announce(const String& text);

// Registers the tool speak (agent runs only) with the tool registry (assistant/tools).
void addTools();

// For the main loop (voice/announcer): next announcement as 24 kHz PCM16 in PSRAM, the caller
// frees it with free().
bool takeSpeech(int16_t*& pcm, size_t& samples);
// Drops the announcements still waiting (the user stopped them). Returns how many.
int dropSpeech();

}  // namespace agent
