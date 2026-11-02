#pragma once

// Shared between memory.cpp (core, tools, consolidation, memory task), memory_archive.cpp (the
// flash index and the mailbox tier) and memory_skills.cpp. Not part of the module's interface
// (memory.h).

#include <Arduino.h>
#include <ArduinoJson.h>
#include <vector>

#include "config.h"
#include "system/scoped_lock.h"

namespace memory {
namespace internal {

constexpr const char* NO_FILES = "Error: memory not available (file system)";
constexpr const char* DIR = "/memory";
constexpr const char* CORE_FILE = "/memory/core.json";
constexpr const char* INDEX_FILE = "/memory/sessions.jsonl";  // one line per conversation
constexpr const char* OUTBOX_DIR = "/memory/outbox";          // notes waiting for the mailbox
constexpr const char* PENDING_DIR = "/memory/pending";        // conversations not yet consolidated
constexpr const char* CORE_JOB = "zz-core.json";              // last in the outbox, one at a time
constexpr const char* CORE_SUBJECT = "Gedächtnis (Kern)";
constexpr const char* JSON_MARKER = "\n---JSON---\n";          // backup mail: text, then the core
constexpr const char* HISTORY_MARKER = "\nVerlauf:\n";         // dossier: header, then entries

// A person's profile: always in the prompt, never merged away or capped like the facts.
struct ProfileField {
    const char* key;
    const char* label;
};
constexpr ProfileField PROFILE[] = {{"name", "Name"},
                                    {"home", "Wohnort"},
                                    {"birthday", "Geburtstag"},
                                    {"family", "Familie und Haushalt"},
                                    {"pets", "Haustiere"},
                                    {"work", "Beruf und Alltag"},
                                    {"interests", "Interessen und Vorlieben"},
                                    {"wishes", "Wünsche an " ASSISTANT_NAME}};

// Core memory and files are used by the main loop (remember / forget), the memory task and the
// mail agent's task. Never held during network requests.
extern SemaphoreHandle_t lock;
// {"next_id": 1,
//  "profiles": {"<person id>": {"name": ..., "home": ..., ...}},   (assistant/people ids)
//  "profile_asked": {"<person id>": "YYYY-MM-DD"} (last getting-to-know question),
//  "facts":  [{"id", "category", "text", "date"}],
//  "topics": [{"key", "title", "status", "keywords", "created", "updated", "t", "done"}],
//  "recent": [{"at", "t", "title", "summary", "minor"}],   (recent: oldest first)
//  "skills": [{"name", "when", "instructions", "updated"}],
//  "rules":  [{"id", "person" ("" = household), "text", "date"}]}   (ids shared with the facts)
extern JsonDocument core;
extern std::vector<std::pair<String, String>> ongoing;  // person, chat not consolidated yet (setOngoing); not saved
extern bool mounted;
extern volatile bool idle;            // no conversation: background work may run
extern volatile bool restorePending;  // empty core at boot: look for the backup mail
extern volatile bool outboxPending;
extern uint32_t outboxRetryAt, restoreRetryAt;

struct Locked : ScopedLock {
    Locked() : ScopedLock(lock) {}
};

bool mailboxUsable();
bool coreBackupUsable();  // Drive (assistant/backup) or the mailbox
bool archiveUsable();     // dossiers and transcripts: Drive when set up, else the mailbox
constexpr const char* CORE_BACKUP_FILE = "core.json";  // in Drive
constexpr const char* DRIVE_TOPICS = "Themen";                 // Drive subfolders of the archive
constexpr const char* DRIVE_CONVERSATIONS = "Gespräche";
constexpr const char* ARCHIVE_MIME = "text/plain";

// memory.cpp (caller holds the lock)
// `memoryWork`: for consolidation and dreaming (without the last dream's diary and ideas).
String contextLocked(bool withIds, const String& person = "", bool memoryWork = false);
JsonObject findTopic(const String& title);
bool coreEmpty();
void normalizeCore();
void saveCore();
const char* category(const char* value);  // a valid category key ("Sonstiges" if unknown)
int addFact(const char* categoryKey, const String& text, const String& date);
JsonObject findFact(int id);
bool setProfile(const String& person, const char* key, String value);  // "" clears the field
bool removeFact(int id);
// Updates (or creates) a topic from a change {title, status, keywords[], notes, done} and adds
// an entry "--- at (heading) ---" with the notes to its dossier. `touch`: a conversation was about
// it, so it counts as recent; not for the dream, which must not bring old topics back.
void applyTopic(JsonObjectConst change, const String& at, const String& heading, bool touch = true);
bool outsider(const String& channel);  // a call with someone outside the household
String speaker(const String& person);  // who Jarvis talks with ("" = unknown)
String outputText(const JsonDocument& response);  // the answer text of a Responses reply

// memory_dream.cpp: the nightly review (dreaming)
String dreamContext(const String& person);  // the last dream and its suggestions for the prompt (lock held)
bool dreamDue();                            // also from the main loop (setIdle): flags only
void requestDream();                        // the dream tool: as soon as the memory task is free
String undoDream();                         // the core as before the last dream (once)
void dream();                               // memory task, holding the TLS gate

// memory_rules.cpp (caller holds the lock, except addRuleTools)
JsonObject findRule(int id);
// Sets a rule of `owner` ("" = the household): replaces rule `replaces` if it exists, else a new
// one. No permission check (the caller decides). Returns its id, 0 if RULE_MAX is reached.
int setRule(const String& owner, const String& text, int replaces);
String rulesContext(const String& person, bool withIds);  // for the prompts, "" without rules
String rulesList();  // all rules with id, owner and date (consolidation, dream)
void mergeRules(JsonArrayConst local);  // restore: rules saved since the start stay
void addRuleTools();

// memory_skills.cpp (caller holds the lock, except addSkillTools)
String skillsContext(bool live);   // "" without skills
void mergeSkills(JsonArrayConst local);  // restore: skills saved since the start win
void addSkillTools();

// memory_archive.cpp
String folded(String text);  // lower case for the search
std::vector<String> listDir(const char* path);  // sorted, oldest first
String nextJob();
// Notes for the mailbox wait in the outbox (caller holds the lock).
void queueNote(const char* folder, const String& subject, const String& replace, const String& text,
               const String& entry = "");
void queueCoreBackup();
void flushOutbox();  // one job; memory task, between conversations
void addToIndex(const String& at, time_t t, const String& title, const String& summary, const String& keywords,
                const String& topics, const String& person, const char* channel);  // caller holds the lock
const char* channelLabel(const char* channel);  // German, "" if unknown
String activities(const String& person, const String& date, const String& channel);  // list_activities
String recall(const String& query, const String& date);
String openTopic(const String& title);
String openConversation(const String& at);
void restore();  // the core from its backup mail

}  // namespace internal
}  // namespace memory
