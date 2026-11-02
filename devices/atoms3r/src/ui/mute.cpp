#include "ui/mute.h"

#include <Arduino.h>
#include <Preferences.h>

#include "audio/audio.h"

namespace mute {
namespace {

bool muted = false;

}  // namespace

void begin()
{
    Preferences prefs;
    muted = prefs.begin("mute", true) && prefs.getBool("on", false);
    prefs.end();
    if (muted) Serial.println("[Mute] the microphone is off (hold the button to switch it on)");
}

bool on() { return muted; }

void set(bool now)
{
    muted = now;
    Preferences prefs;
    if (prefs.begin("mute", false)) prefs.putBool("on", now);
    prefs.end();
    Serial.printf("\n[Mute] microphone %s\n", now ? "off" : "on");
    audio::queueTone(now ? 330 : 660, 150);  // the Echo's tones: low = off, high = on
}

}  // namespace mute
