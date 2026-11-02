#pragma once

// The microphone switched off by the user, as the Echo's mute button: holding the screen button
// MUTE_HOLD_MS toggles it. While muted the wake word does not listen and no conversation starts at
// the speaker (the button gives a low tone instead); announcements still play. The state is kept
// in NVS and survives a restart, as the Echo's hardware latch does.
namespace mute {

void begin();  // reads the kept state
bool on();
void set(bool muted);  // keeps it, logs it, gives a tone

}  // namespace mute
