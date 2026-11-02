#pragma once

#include <Arduino.h>

#include <vector>

#include "services/mail.h"
#include "services/psram_sink.h"

// The assistant watches the private mailboxes of the household (WATCHED_MAILBOXES in secrets.h: IMAP or
// Gmail, any person of the register), and tells their owner what matters (issue #37). Reading
// never changes a mailbox; only sort_mail and the owner's rules move mails (assistant/mail_sort).
// Between conversations the registry's worker checks every MAILWATCH_POLL_MS for new mail; one
// backend request per mailbox and check sorts it with the person's memory as context (category,
// now / digest / none, a German summary, deadline, parcels) and no tools: the mails come from
// anyone. "now" goes to the person right away, "digest" in one message at MAILWATCH_DIGEST_HOUR
// (people::message: Telegram or WhatsApp, as set for the person). During a conversation that runs
// MAILWATCH_LIVE_AFTER_MS, the listener's mailboxes are checked too (every MAILWATCH_LIVE_POLL_MS,
// while the backend does not work; in the Telegram/WhatsApp task, never the tools worker the
// conversation's calls need: tools::addConversationWork), and a "now" mail is told
// there as a notice (live::notice), framed as data, instead of a message (issue #38). Every notice is logged in the
// memory (channel MAIL_NOTICE), so a chat can refer to it ("so etwas nicht melden" becomes a
// preference the sorting follows). The sorted mails and the parcels stay in /mailwatch.json.
// Tools: mail_news, search_private_mail, read_private_mail; moving mails into folders and the
// owner's rules for that: assistant/mail_sort (sort_mail, mail_rule).
namespace mailwatch {

bool configured();  // at least one mailbox in WATCHED_MAILBOXES
// Registers mail_news (assistant/tools) and the check as background work of the registry's worker.
void addTools();
void setIdle(bool on);  // between conversations: all mailboxes, the digests, the index

// A due date one of the person's sorted mails set (a bill, a return, an appointment), for the agenda.
struct Deadline {
    String key;      // stable per mail ("mail:<mailbox>:<ref>")
    String day;      // YYYY-MM-DD
    String subject;  // for spotting it in a transcript
    String line;     // category and the assistant's summary
};
// The deadlines of `person`'s mails from `from` to `to` (YYYY-MM-DD, both included), by day; one per
// subject and day (reminder mails repeat it); none from newsletters, advertising, social or spam.
std::vector<Deadline> deadlines(const String& person, const String& from, const String& to);

// One attachment of a mail in `person`'s own watched mailbox `label` (ref as read_private_mail
// takes it): the first whose file name contains `name` ("" = the first), decoded into `data`, at
// most `maxBytes`. Read only. Returns "" or an error. Blocks for seconds (IMAP or Gmail).
String attachment(const String& person, const String& label, const String& ref, const String& name, size_t maxBytes,
                  PsramSink& data, mail::AttachmentInfo& found);

}  // namespace mailwatch
