#pragma once

#include <Arduino.h>

// What is ahead for the admin until tomorrow night: the events of their Google calendars and the
// to-dos of their Google Tasks lists that are due (overdue ones included), so the assistant can bring
// them up on his own ("Übrigens, Sir ..."), issue #38. Fetched by the registry's worker between
// conversations (every AGENDA_REFRESH_MS, at a new day, and soon after the assistant changed an event or
// a to-do) and kept in RAM: a conversation never waits for Google. The admin's accounts: only in
// conversations with them, or with nobody named at the speaker (live::privateFor). Deadlines from
// the sorted mails (mailwatch::deadlines, today and tomorrow) go to their owner, any household member.
//
// The model picks the moment to mention something. The device remembers per day what came up in
// a conversation (a word of its title in the transcript), so later conversations list it as
// already mentioned instead of bringing it up again. During a conversation an appointment coming
// closer than AGENDA_SOON_MIN is passed on as a notice (live::notice).
namespace agenda {

void addTools();  // registers the refresh as background work (no tools of its own)

// The agenda for a prompt, "" if there is none or `person` may not see it (the calendar: the
// admin's; mail deadlines: the person's own). `atHome`: a
// conversation at the speaker (nobody named there counts as the admin). `voice`: split into not
// mentioned yet and already mentioned today; otherwise one list (chats: information only).
String context(const String& person, bool atHome, bool voice);

// Main loop: notices for an appointment coming close during a conversation.
void update();

// The device is about to hang up: the entries of this conversation's agenda that matter now
// (today's, overdue to-dos, tomorrow's from AGENDA_EVENING_HOUR on), have not come up today or in
// `transcript`, and were not offered before today; one line each, "" if none. Marks them offered.
String closing(const String& transcript);

// A conversation that had the agenda ended: what of it came up (main loop).
void conversationOver(const String& transcript);

// Fetch again soon (an event or a to-do was changed).
void refreshSoon();

void print();  // serial

}  // namespace agenda
