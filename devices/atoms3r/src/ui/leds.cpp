#include "ui/leds.h"

#include <Arduino.h>

#include "audio/audio.h"
#include "audio/volume.h"
#include "config.h"
#include "system/board.h"
#include "system/network.h"
#include "ui/mute.h"
#include "ui/status.h"
#include "voice/live_session.h"

namespace leds {
namespace {

constexpr int COUNT = 14;

struct Pattern {
    uint32_t color;
    int lit;  // LEDs lit from the bottom
};

int levelBar(int level) { return 1 + level * (COUNT - 1) / 100; }

Pattern pattern()
{
    const bool pulse = (millis() / 250) % 2;
    if (status::errorActive()) return {0xFF0000, COUNT};
    // Volume as a level bar: grows when louder, shrinks when quieter.
    if (status::volumeVisible()) return {0xC0C0C0, (volume::level() * COUNT + 50) / 100};
    switch (live::state()) {
        case live::State::Idle:
            if (!network::connected()) return {pulse ? 0x201000u : 0u, COUNT};
            if (mute::on()) return {0x200000, COUNT};  // dim red: the microphone is off
            return {0x000008, COUNT};
        case live::State::Listening:  // the first words after the wake word (voice/first_words)
            return {0x00A0FF, levelBar(audio::micLevel())};
        case live::State::Connecting:
        case live::State::Starting:
            // After the wake word the user has spoken (voice/first_words): GPT Live connects to answer.
            if (live::startedByWakeWord()) return {pulse ? 0x8000FFu : 0x200040u, COUNT};
            return {pulse ? 0x806000u : 0x201800u, COUNT};
        case live::State::Live:
            if (audio::speaking()) return {0x00FF40, levelBar(audio::speakerLevel())};
            if (live::thinking()) return {pulse ? 0x8000FFu : 0x200040u, COUNT};
            return {0x00A0FF, levelBar(audio::micLevel())};
        case live::State::Closing:
            return {0x000020, COUNT};
    }
    return {0x000008, COUNT};
}

}  // namespace

void update()
{
    // Cache pixels: only changed LEDs generate I2C traffic.
    static uint32_t shown[COUNT];
    static bool initialized = false;
    const Pattern p = pattern();
    for (int i = 0; i < COUNT; ++i) {
        const uint32_t pixel = i < p.lit ? p.color : 0;
        if (initialized && shown[i] == pixel) continue;
        const int led = LED_FIRST_AT_BOTTOM ? i : COUNT - 1 - i;
        for (int strip = 1; strip <= 2; ++strip)
            board::pyramid().ctrl().setRGB(strip, led, pixel >> 16, pixel >> 8, pixel);
        shown[i] = pixel;
    }
    initialized = true;
}

}  // namespace leds
