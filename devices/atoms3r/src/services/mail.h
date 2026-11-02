#pragma once

#include <Arduino.h>
#include <ArduinoJson.h>
#include <vector>

#include "services/psram_sink.h"

// E-mail for the assistant over the mailbox in secrets.h (SMTP 465 / IMAP 993, TLS with the
// T-TeleSec root). Tools: send_email, print_document (HTML mail to the printer's Epson Connect
// address; Epson renders HTML and prints attachments), list_emails, read_email (inbox or the
// sent folder: every sent mail gets a copy there, as a mail program would do).
// The tools are slow (TLS, IMAP take seconds): in a conversation they run in the tool
// registry's worker while the WebSocket keeps going; the same worker checks the inbox.
namespace mail {

bool configured();
const char* address();  // The assistant's own address, "" without a mailbox
bool takesCommands();   // mails from household members are carried out (MAIL_AUTHSERV_ID set)
// Registers send_email, print_document, list_emails and read_email (assistant/tools); they
// are slow (seconds of TLS): in a conversation they run in the registry's worker.
void addTools();
// Sends a mail right away from any task, `data` (optional) as attachment `filename` of type
// `mime`. Blocks for seconds (longer while another task uses the mailbox). Returns "sent" or an
// error.
String send(const String& to, const String& subject, const String& html, const uint8_t* data = nullptr,
            size_t length = 0, const String& filename = "", const String& mime = "image/jpeg");
// Between conversations the registry's worker checks the inbox every MAIL_POLL_MS for mails
// from the people of the household (assistant/people; DMARC-verified by the provider), runs them
// through the agent for that person and replies.
void setIdle(bool on);

// The assistant's notes (assistant/memory) in folders of its own mailbox, created on first use. Only
// The assistant puts mails there (IMAP APPEND); anyone can send to the inbox, so notes never live there.
// Newest `count` notes per folder matching the IMAP search `criteria` (e.g. SUBJECT "[mosel]"),
// newest first, as plain text of at most `limit` characters (0 = NOTE_READ_LIMIT; below 64 KB:
// a String corrupts content beyond); one login for all folders. Blocks for seconds; `error` is
// set when the mailbox was unreachable.
std::vector<String> readNotes(const std::vector<const char*>& folders, const String& criteria, size_t count,
                              String& error, size_t limit = 0);
// Stores a note, then deletes the older notes matching `replaceCriteria` ("" = keep all).
// Returns "stored" or an error.
String storeNote(const char* folder, const String& subject, const String& text, const String& replaceCriteria);
// Deletes all notes in `folder`. Returns "cleared" or an error.
String clearNotes(const char* folder);

// A JSON backup (people register, scheduled tasks) in MEMORY_FOLDER_CORE: `summary` readable for
// a person, then the document; replaces the older one with the same subject. Returns "stored"
// or an error. Blocks for seconds.
String storeBackup(const char* subject, const String& summary, const JsonDocument& doc);
// The newest backup with `subject` into `doc`: false with `error` set if the mailbox was not
// reachable; else true, with `doc` null if there is none (or it is not readable).
bool readBackup(const char* subject, JsonDocument& doc, String& error);

// Plain text of an HTML body: block tags as line breaks, list items as dashes, no style or
// script, entities decoded, whitespace collapsed. Keep `html` below 64 KB (Arduino String).
String htmlToText(const String& html);
// `text` without invisible characters (zero-width spaces and joiners, soft hyphens, which
// newsletters fill their previews with), runs of spaces collapsed, trimmed.
String cleanText(const String& text);

// A mail of a watched mailbox (assistant/mail_watch), as text for the classifier.
struct Incoming {
    String ref;  // IMAP UID or Gmail message id
    String from, to, subject, date;
    time_t at = 0;  // when it was sent (from the Date header / Gmail), 0 if unknown
    String hints;        // signals of bulk mail (mailing list, automatic), labels
    String attachments;  // "name (type), ..."
    String text;         // the start of the body as plain text
};
// New mails in the inbox of another IMAP account (a household member's mailbox, same port and
// CA as the assistant's own; issue #30), read only: EXAMINE, so the owner's seen flags stay. `validity`
// / `last`: the state of the last check (UIDVALIDITY, highest UID seen); on the first check (or
// after the server renumbered) they are set to the newest mail and nothing is returned. At most
// `max` mails, oldest first, each with up to `textLimit` bytes of text; `last` moves past them.
// Returns "" or an error. Blocks for seconds.
String fetchNew(const char* host, const char* user, const char* password, uint32_t& validity, uint32_t& last,
                size_t max, size_t textLimit, std::vector<Incoming>& mails);
// Mails of the last `days` days in that inbox, then in `folders` (server names), containing every
// word of `words` (headers or text, the server's IMAP search; "" = all), newest first per folder,
// at most `max` per folder, headers only. A mail outside the inbox has the ref "<uid>@<folder>".
// Read only.
String searchOther(const char* host, const char* user, const char* password, const String& words, int days,
                   size_t max, std::vector<Incoming>& mails, const std::vector<String>& folders = {});
// One mail of that account's `folder` (server name) with up to `textLimit` bytes of text. Read
// only. Returns "" or an error.
String readOther(const char* host, const char* user, const char* password, const String& folder, uint32_t uid,
                 size_t textLimit, Incoming& mail);

// An attachment downloaded by attachment() / attachmentOther().
struct AttachmentInfo {
    String filename, mime;
};
// One attachment of mail `uid` in the assistant's inbox (or sent folder): the first whose file name
// contains `name` (any case; "" = the first attachment), decoded into `data`, at most `maxBytes`.
// Read only (the mail stays unread). Returns "" or an error naming the attachments there are.
String attachment(uint32_t uid, bool sent, const String& name, size_t maxBytes, PsramSink& data, AttachmentInfo& found);
// The same in `folder` (server name) of another IMAP account. Read only.
String attachmentOther(const char* host, const char* user, const char* password, const String& folder, uint32_t uid,
                       const String& name, size_t maxBytes, PsramSink& data, AttachmentInfo& found);

// A folder of another IMAP account.
struct Folder {
    String name;    // for people: decoded (modified UTF-7), the hierarchy separated by "/", without "INBOX/"
    String server;  // as the server names it, for commands
    String use;     // special use (RFC 6154): "trash", "archive", "junk", "sent", "drafts" or ""
};
// That account's folders (IMAP LIST); `separator` is the server's hierarchy delimiter. Read only.
String foldersOther(const char* host, const char* user, const char* password, std::vector<Folder>& folders,
                    String& separator);
// A folder name as the server needs it (modified UTF-7).
String folderName(const String& name);

// One mail to move (moveOther).
struct Moved {
    uint32_t uid = 0;    // in the source folder; 0 = look it up by messageId there
    String messageId;    // filled in when empty
    uint32_t newUid = 0;  // in the target folder after the move, 0 if the server did not say
};
// Moves mails of another IMAP account from folder `from` to `to` (server names; `to` is created
// first when `create`). Mails not found are left out of `mails`. The only write to a household
// member's mailbox: on their own request or rule (assistant/mail_sort). Never deletes: no
// EXPUNGE of anything but the moved mails (UID EXPUNGE). Returns "" or an error. Blocks for seconds.
String moveOther(const char* host, const char* user, const char* password, const String& from, const String& to,
                 bool create, std::vector<Moved>& mails);
// The UIDs of that inbox's mails of the last `days` days, oldest first. Read only.
String listOther(const char* host, const char* user, const char* password, int days, std::vector<uint32_t>& uids);
// Several mails of that inbox in one session, each with up to `textLimit` bytes of text. Read only.
String fetchOthers(const char* host, const char* user, const char* password, const std::vector<uint32_t>& uids,
                   size_t textLimit, std::vector<Incoming>& mails);
// An RFC 5322 date ("Sat, 3 Oct 2026 21:03:26 +0200") as time, 0 if unreadable.
time_t parseDate(const String& date);

}  // namespace mail
