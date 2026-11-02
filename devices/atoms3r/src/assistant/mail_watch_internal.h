#pragma once

// Shared by assistant/mail_watch (the check, the index, the digest, mail_news and the search) and
// assistant/mail_sort (folders, moving and rules). Not for other modules: they use mail_watch.h.

#include <Arduino.h>
#include <ArduinoJson.h>
#include <vector>

#include "assistant/tools.h"
#include "system/scoped_lock.h"

namespace mailwatch {

struct Mailbox {
    const char* person;  // name or id in the register, "" = the admin
    const char* label;   // how the assistant names it ("Gmail", "Work")
    const char* host;    // IMAP host or "gmail"
    const char* user;
    const char* secret;  // IMAP password, or the Gmail account's refresh token ("" = the admin's)
};
extern const std::vector<Mailbox> MAILBOXES;

// /mailwatch.json (layout in mail_watch.cpp) and the lock that guards it.
extern JsonDocument state;
extern SemaphoreHandle_t lock;
struct Locked : ScopedLock {
    Locked() : ScopedLock(lock) {}
};
void save();  // caller holds the lock

bool gmail(const Mailbox& box);
String owner(const Mailbox& box);  // the owner's id, "" while they are not in the register
// Whose mailboxes a request may use: the asking person's; nobody named (at home) = the admin's.
String reader(const tools::Context& context);
// The person's mailbox named `label` (ignoring case), "" = all of them; empty with `error` if none.
std::vector<const Mailbox*> pick(const String& person, const String& label, String& error);
// A Gmail REST call for the mailbox's account; returns the HTTP status (0: answer not readable).
int gmailCall(const Mailbox& box, const char* method, const String& path, const String& body, JsonDocument& doc,
              String& error);

// Files the inbox's mails of the last `days` days the assistant has not sorted yet, in the background
// between conversations (the index; nobody is told); the owner's rules move what matches. Lists
// the mailbox (seconds). Returns a text for the model.
String fileOlder(const Mailbox& box, int days);

// --- assistant/mail_sort -----------------------------------------------------------------

void addSortTools();  // sort_mail, mail_rule
// The person's rules for the mails of `box` just logged (`refs`, not the index's old ones): what
// matches is moved right away. Between conversations (the check), without the lock held.
void applyRules(const Mailbox& box, const String& person, const std::vector<String>& refs);
// For the digest: what Jarvis moved by rule since the last digest ("" if nothing); `markReported`
// once the digest went out. And one suggestion for a rule when a kind of mail piles up ("" if
// none; noted, so it comes at most every MAILSORT_SUGGEST_DAYS). Caller holds the lock.
String movedReport(const String& person, bool markReported);
String ruleSuggestion(const String& person);
// The folders besides the inbox where the assistant put mails of `box` (IMAP server names, no trash):
// the search looks there too. Caller holds the lock.
std::vector<String> sortFolders(const Mailbox& box);
// Where an IMAP mail of the log is now: its folder (server name) and UID there. False if it was
// not moved (still in the inbox under `ref`). Caller holds the lock.
bool movedTo(const Mailbox& box, const String& ref, String& folder, uint32_t& uid);

}  // namespace mailwatch
