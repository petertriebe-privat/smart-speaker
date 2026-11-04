#pragma once

// Announcements of the mail agent (its "speak" tool). The agent runs in the mail task and hands
// the audio over (24 kHz PCM in PSRAM); the speaker belongs to the main loop, which plays it
// between conversations. A conversation that starts meanwhile cuts it off.
namespace announcer {

void update();
bool active();  // playing, including the audible tail
// The button during an announcement: silence at once, the waiting ones dropped too.
void stop();

}  // namespace announcer
