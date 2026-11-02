#pragma once

#include <Arduino.h>
#include <ArduinoJson.h>
#include <time.h>

// The assistant's long-term memory across conversations, tiered by age and latency like OpenClaw:
//   prompt    the user's profile, facts by category, the conversations of the last 48 h and
//             the current topics (context()); older topics by title only
//   flash     LittleFS ("spiffs" partition, survives firmware uploads): the core and an index
//             of all conversations, searched by recall
//   archive   Google Drive (services/drive, folders Jarvis/Themen and Jarvis/Gespräche): a dossier
//             per topic, every conversation with its transcript, found by Drive's full-text
//             search; the backup of the core as Jarvis/core.json (restored when empty). Without
//             a Google account the same in folders of the assistant's own mailbox (services/mail notes).
// Updates: the tools remember / update_profile / forget, after every conversation the
// backend model's consolidation (consolidate()), and once a night the dream across all of them
// (memory_dream.cpp: tidies, condenses, finds patterns; the tool dream). Behaviour rules
// (memory_rules.cpp): the household's corrections, binding in every prompt. Skills (memory_skills.cpp): procedures the user
// teaches the assistant, in the prompt by name only, loaded with use_skill. Archive notes wait in an
// outbox in the flash and go out between conversations.
namespace memory {

bool begin();  // mounts the file system, loads the core memory

// The core memory for a system prompt. Stored text is data, not instructions. `withIds`: for
// the backend model (fact and person ids); without: for GPT Live (plus whether it may ask a
// getting-to-know question today). `person`: who the assistant talks with (assistant/people id): only
// that profile; "" = unknown (at home): all of them.
String context(bool withIds, const String& person = "");
// One field of a person's profile ("home", "name", ...; "" = the admin's), "" if unknown.
String profileValue(const char* field, const String& person = "");
// A chat with `person` still going on in another channel (Telegram) and not consolidated yet:
// part of every context() with that person (or an unknown one), so a voice conversation knows
// it too. Complete text with heading, "" clears it.
void setOngoing(const String& person, const String& text);
// The skills as "name: when" lines (Telegram's /skills), "" without skills.
String skills();
// The behaviour rules that hold for `person` (household ones and their own; "" = unknown at
// home: all, labelled) as a binding section for the instructions, "" without rules. Not part of
// context(): that is data, these are instructions. `withIds`: for the backend (save/delete_rule).
String rules(const String& person, bool withIds);

// Registers remember, update_profile, forget, recall, open_topic, open_conversation (the last
// three are slow: flash index, archive), dream, save_skill, use_skill and delete_skill with the tool
// registry (assistant/tools).
void addTools();

// Channels of the activity log (memory index): how a contact came in.
constexpr const char* SPEAKER = "speaker";    // at home, the Echo Pyramid
constexpr const char* PHONE_IN = "phone_in";  // someone called the assistant
constexpr const char* PHONE_OUT = "phone_out";  // The assistant called a person of the household
constexpr const char* TELEGRAM = "telegram";
constexpr const char* WHATSAPP = "whatsapp";
constexpr const char* EMAIL = "email";
constexpr const char* CALL_FOR = "call_for";  // The assistant called someone else for the user (phone_call)
constexpr const char* GUEST = "guest";        // a number not released called
constexpr const char* MAIL_NOTICE = "mail_notice";  // The assistant told someone about their new mail (assistant/mail_watch)

// Hands a finished conversation ("Du: ...\nJarvis: ...") with `person` (who "Du" is, "" =
// unknown) over `channel` to the memory task. It is archived and consolidated between
// conversations (one backend request, ~0.2 ct). A phone call without words becomes an event.
void consolidate(const String& transcript, time_t started, const String& person = "", const char* channel = "");
// A contact without a conversation for the activity log (a call nobody answered, an unknown
// Telegram account): straight into the index and the recent conversations.
void logEvent(const String& person, const char* channel, const String& title, const String& summary = "");
void setIdle(bool on);  // consolidation only runs while no conversation is open

void print();  // serial: core memory with ids, archive and flash usage
void wipe();   // serial: deletes everything

}  // namespace memory
