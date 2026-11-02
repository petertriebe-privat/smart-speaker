#pragma once

#include <Arduino.h>

// Jarvis answers the wake word right away with a short clip in its own voice ("Ja, Sir?"),
// while the connection to GPT Live is still being built (~3.5 s) and GPT Live would need another
// ~3.5 s for a greeting. The clips are made once with OpenAI's TTS (the voice of the live
// session), kept in LittleFS and loaded into PSRAM; the registry's worker makes or loads them in
// the background after the start. Without them the wake word only beeps, as before.
namespace acknowledge {

void begin();  // registers the background work (after tools::begin())
// Starts a random clip; false if none is loaded yet.
bool start();
void update();       // feeds the speaker from the main loop
bool playing();      // still playing, including the audible tail

}  // namespace acknowledge
