#pragma once

#include <Arduino.h>

// Quick home commands (#75), as on the Echo (core/cmd/assistant/home.go): "Jarvis, Licht
// aus" takes a moment and no GPT Live answer. The rules are Home Assistant's own sentence
// matching (Assist, in the LAN), which knows the areas, names and aliases: the device only
// decides that a short first utterance is meant for the house (a home word) and hands the words
// to HA. What HA does not understand goes on to GPT Live, which has HA's tools.
namespace quick_home {

enum class Result { Done, Failed, Unknown };  // Unknown: HA did nothing, GPT Live answers

// Short (up to 10 words without fillers), no question, a home word (Licht, Rollo, Heizung, ...),
// not the TV - or a bare room ("Küche aus").
bool candidate(const String& text);
// A room and nothing but an/aus/ein ("Küche aus"); HA gets it as "Licht Küche aus".
bool bareRoom(const String& said);
// The words name a room ("in der Küche", "im Flur").
bool namesPlace(const String& text);
// The user's words for HA: without the wake word, the assistant's own "Ja, Sir" and the final full stop.
String words(const String& text);
// Hands the command to HA: without a room named this speaker's (DEVICE_ROOM) first, then the
// words alone. Blocks the caller up to QUICK_HOME_TIMEOUT_MS per try. `detail`: HA's answer.
Result run(const String& text, String& detail);
// The rules on fixed sentences (serial Q, tests/device): prints "[Quick test] ok" or the misses.
void selfTest();

}  // namespace quick_home
