#pragma once

#include <Arduino.h>

namespace prompt {

// Date, time and location for the system prompt, "" before the clock is set. Part of the
// startup instructions on purpose: injecting it mid-session made the model voice a made-up
// user question ("Wie spaet ist es?") and answer it.
String context();

// The assistant's character (shared/prompts/persona.h) for `person` (assistant/people id): their level, or
// child-friendly; "" = unknown (at home): the default level and how to talk with each person.
String persona(const String& person);

}  // namespace prompt
