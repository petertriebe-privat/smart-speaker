#pragma once

#include <Arduino.h>
#include <time.h>

#include "assistant/tools.h"

// The shared state of a network of devices (config.h MEMBER, #66), as the Echos'
// (core/cmd/assistant/shared.go): the leader (an Echo) owns memory, people and tasks.
//   - The tools that change that state run on the leader; the ones that only read too, with the
//     local copy as the fallback.
//   - The instructions' context (notes, rules, planned tasks and agenda) comes from the leader,
//     fetched in the background and kept, since the main loop must not wait for the network.
//   - Finished conversations go to the leader for its memory and agenda.
// Without a reachable leader the local emergency memory: a journal in the flash of what the
// leader must still get (state changes, conversations), shown in the instructions and replayed
// in order once a leader is back (each entry with an id, so the leader runs it once). Reminders
// made meanwhile are kept and rung here (assistant/tasks, a file of their own) until then.
// A standalone device (no PEER_KEY) is not affected: everything stays local.
namespace shared {

void begin();  // after tools::begin(): registers the background work (refresh, replay)
// A member: list_affairs and update_affair, the leader's open matters (#71); they always run there
// (a standalone device has no affairs).
void addTools();

// A tool whose state is the leader's: true with its result in `output`. False: run it here
// (standalone, a tool that is not shared, or the leader is away and the local copy serves).
bool forward(const char* name, const char* argumentsJson, const tools::Context& context, String& output);
bool remote(const char* name);  // forwarded (then slow: the worker runs it, not the main loop)

// The instructions' context: the leader's for a member (local until it was fetched once), else
// the local one.
String notes(bool withIds, const String& person);
String rules(const String& person, bool withIds);
String planned(const String& local);  // the leader's tasks and agenda plus local emergency tasks

// A finished conversation (`text` with the note on its kind) for a member: into the journal and
// on to the leader. False for a standalone device: consolidate it here.
bool conversationOver(const String& text, time_t started, const String& person, const char* channel,
                      const String& transcript);

void print();  // serial: cache, journal

}  // namespace shared
