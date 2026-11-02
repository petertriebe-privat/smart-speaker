#pragma once

#include <Arduino.h>

// What display and LEDs report besides the conversation state: errors and volume changes.
namespace status {

// The text stays on the display until cleared (during a conversation: until it expires);
// the LEDs are red for `ms`.
void showError(const String& text, uint32_t ms = 3000);
void clearError();
void setFatal(const String& text);  // startup failed: stays red, nothing else runs
void showVolume();                  // LED level bar for VOLUME_DISPLAY_MS
void update();                      // drops expired errors during a conversation

bool fatal();
bool errorActive();                 // LEDs red
bool volumeVisible();
const String& errorText();

}  // namespace status
