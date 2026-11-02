#pragma once

#include <Arduino.h>

// Whether a conversation is over. GPT Live says goodbye ("Mach's gut, Sir", "ich lege auf") but
// mostly does not delegate end_call, not even with a clear rule in its instructions (28.09.).
// So after each answer of the assistant, once it is quiet, the backend model (DELEGATION_MODEL) reads
// the end of the transcript and says whether the conversation is finished: no fixed phrase list,
// questions and "eine Sache noch" stay open. The request runs in the tool registry's worker
// over a connection opened when the session starts and kept alive (~1 s per check).
namespace conversation_end {

void begin();  // registers the background work (keeps the connection while a session is open)
// Main loop: asks about `transcript` (the end of the conversation); `stamp` identifies the
// moment (the session's last activity), a later answer for another stamp is ignored.
void check(const String& transcript, uint32_t stamp);
bool over(uint32_t stamp);  // main loop: the answer for `stamp` was "finished"
void reset();               // a session starts or ends

}  // namespace conversation_end
