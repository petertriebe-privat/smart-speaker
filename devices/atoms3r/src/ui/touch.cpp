#include "ui/touch.h"

#include <M5EchoPyramid.h>

#include "audio/volume.h"

namespace touch {
namespace {

constexpr int PADS = 4;
constexpr uint32_t POLL_MS = 15;
constexpr uint32_t RELEASE_MS = 30;  // debounce release only: touch reacts immediately
constexpr uint32_t REPEAT_MS = 180;
constexpr int VOLUME_STEP = 5;

uint8_t pressedBits = 0;  // debounced state, bit 0 = TP1
uint32_t releaseSince[PADS] = {}, lastRepeat[PADS] = {};

// Reads all four pads in one transaction; the library masks failed reads poorly.
bool read(uint8_t& held)
{
    Wire1.beginTransmission(STM32_I2C_ADDR);
    Wire1.write(TOUCH_BUTTON_STATUS_REG_ADDR);
    if (Wire1.endTransmission(false) != 0 ||
        Wire1.requestFrom(uint8_t(STM32_I2C_ADDR), uint8_t(PADS)) != PADS)
        return false;
    held = 0;
    for (int i = 0; i < PADS; ++i)
        if (Wire1.read()) held |= 1 << i;
    return true;
}

void onPad(int pad) { volume::change(pad == 2 || pad == 3 ? VOLUME_STEP : -VOLUME_STEP); }

}  // namespace

void update()
{
    static uint32_t lastPoll = 0;
    const uint32_t now = millis();
    if (now - lastPoll < POLL_MS) return;
    lastPoll = now;
    uint8_t held = 0;
    if (!read(held)) return;
    for (int i = 0; i < PADS; ++i) {
        const uint8_t bit = 1 << i;
        const bool pressed = held & bit;
        const bool rising = pressed && !(pressedBits & bit);
        if (pressed) {
            pressedBits |= bit;
            releaseSince[i] = 0;
        } else if (pressedBits & bit) {
            if (!releaseSince[i]) releaseSince[i] = now;
            if (now - releaseSince[i] >= RELEASE_MS) pressedBits &= ~bit;
        }
        const bool repeat = pressed && now - lastRepeat[i] >= REPEAT_MS;
        if (rising || repeat) {
            onPad(i + 1);
            lastRepeat[i] = now;
        }
    }
}

}  // namespace touch
