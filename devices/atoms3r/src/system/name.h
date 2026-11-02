#pragma once

// The assistant's name (#29): ASSISTANT_NAME from secrets.h, "Jarvis" by default
// (shared/prompts/assistant_name.h). The same rules as the Echo's core/internal/prompts/name.go.

#include <Arduino.h>

namespace name {

// The name as an identifier: lower case, a-z, 0-9 and _ ("Jarvis" -> "jarvis"). The prefix of the
// Home Assistant events and entities (jarvis_announce, binary_sensor.jarvis_presence).
const String& slug();

// A spoken word (lower case, punctuation allowed) is the name, a word of it, or the built-in wake
// word's "jarvis": speech filters drop it like a filler.
bool isName(String word);

}  // namespace name
