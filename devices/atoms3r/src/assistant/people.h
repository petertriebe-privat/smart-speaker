#pragma once

#include <Arduino.h>
#include <vector>

// The people of the household who use the assistant: who someone is and what it may
// do. Profiles (preferences, birthday) live in the memory, not here. The admin (OWNER_NAME with
// PHONE_OWNER, TELEGRAM_OWNER_ID and OWNER_EMAIL) comes from the firmware on every start, so
// nobody can make himself admin and he is always there, even with empty storage. Members are
// added by the admin (add_person, ...). Stored in LittleFS (/people.json), backed up to the
// mailbox and restored from there when the file is missing.
//
// A person is known for sure on the phone (number), on Telegram (account) and by mail (address);
// at home only from the conversation (nobody = unknown, "", and the house is trusted).
namespace people {

struct Person {
    String id, name, role;  // role: "admin" or "member"
    std::vector<String> phones;  // normalized (0171...)
    int64_t telegram = 0;        // 0 = not known yet
    String email;
    std::vector<String> blocked;  // tools this person may not use
    int humor = -1;               // The assistant's cheekiness with this person (shared/prompts/persona.h), -1 = PERSONA_LEVEL
    bool child = false;           // The assistant talks child-friendly
    String invite;                // code of the Telegram link in the welcome mail, "" once used
    String notify;                // channel for notices Jarvis starts: "telegram", "whatsapp", "" = automatic
    bool admin() const { return role == "admin"; }
};

// Sends `text` (plain text, no markup) to a person ("" = the admin) on their channel for notices:
// the one set in `notify`, else Telegram when their account is known, else WhatsApp; the other
// one if that fails (WhatsApp only within 24 hours of their last message). From any task;
// blocks for seconds. Returns "telegram" / "whatsapp" (the channel used) or an error.
String message(const String& person, const String& text);

// A person who opened the Telegram link of their welcome mail ("/start <code>"): their account
// is linked and the code used up. Returns the person's id, "" if no person has this code.
String claimInvite(const String& code, int64_t telegram);

void begin();  // after memory::begin() (mounts LittleFS)
void setIdle(bool on);  // the backup only runs between conversations

bool find(const String& id, Person& person);  // by id; false if unknown ("" = nobody)
std::vector<Person> all();
String admin();                              // the admin's id
String name(const String& id);               // "" if unknown
String byName(const String& name);           // id by name or id (ignoring case), "" if none
String byPhone(const String& number);        // "" if none
String byTelegram(int64_t account);
String byEmail(const String& address);

// Whether `person` may run `tool` (`privateTool`: the admin's own accounts). Unknown people
// ("", at home) may run everything. Returns "" or the error for the model.
String denied(const String& person, const char* tool, bool privateTool);

// For the prompts: who lives here, "" without members.
String household();

// The assistant's cheekiness with a person (0..PERSONA_MAX_LEVEL): theirs, else PERSONA_LEVEL.
int humor(const Person& person);

// Registers list_people, add_person, update_person, remove_person, set_humor (assistant/tools).
void addTools();

void print();  // serial

}  // namespace people
