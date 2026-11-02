#include "system/board.h"

#include <M5Unified.h>

#include "audio/audio.h"
#include "audio/volume.h"
#include "config.h"
#include "ui/status.h"

namespace board {
namespace {

constexpr uint32_t SILENT_MIC_MS = 3000;

M5EchoPyramid echoPyramid;

}  // namespace

String begin()
{
    auto cfg = M5.config();
    cfg.internal_spk = false;
    cfg.internal_mic = false;
    M5.begin(cfg);
    Serial.setRxBufferSize(2048);  // the ">tool {json}" test command (system/console) is long
    Serial.begin(115200);

    Wire1.end();
    echoPyramid.begin(&Wire1, 38, 39, 6, 8, 5, 7, audio::CODEC_RATE);
    Wire1.setTimeOut(20);
    echoPyramid.ctrl().setBrightness(1, 60);
    echoPyramid.ctrl().setBrightness(2, 60);
    // begin() in this library always returns true; probe the controller explicitly.
    Wire1.beginTransmission(STM32_I2C_ADDR);
    if (Wire1.endTransmission() != 0) return "Pyramid nicht gefunden";
    if (!echoPyramid.codec().setVolume(DEFAULT_VOLUME) || !echoPyramid.codec().mute(false))
        return "Codec-Fehler";
    return "";
}

M5EchoPyramid& pyramid() { return echoPyramid; }

// A live microphone never delivers exact zeros for long.
void checkMicrophone()
{
    static uint32_t lastSignal = 0;
    static bool warned = false;
    const uint32_t now = millis();
    const auto stats = audio::echoStats();
    if (stats.rawLeft || stats.rawRight) {
        lastSignal = now;
        if (warned) {
            warned = false;
            status::clearError();
        }
    } else if (!warned && now - lastSignal > SILENT_MIC_MS) {
        warned = true;
        const char* warning = "Kein Mikrofonsignal - Pyramide ohne Strom?";
        status::showError(warning, 0);
        Serial.println(warning);
    }
}

// The amplifier (AW87559) is set up only at boot. On 29.09. The assistant was silent while everything
// else worked, until the power was replugged: check it, set it up again if it lost its setup.
void checkAmplifier()
{
    constexpr uint8_t ADDRESS = 0x5B;
    constexpr uint8_t ENABLED = AW87559_SYS_EN_SW_MASK | AW87559_SYS_EN_BOOST_MASK | AW87559_SYS_EN_PA_MASK;
    static uint32_t lastCheck = 0;
    static bool warned = false;
    if (millis() - lastCheck < AMPLIFIER_CHECK_MS) return;
    lastCheck = millis();
    Wire1.beginTransmission(ADDRESS);
    Wire1.write(AW87559_REG_SYSCTRL);
    const bool answered = Wire1.endTransmission(false) == 0 && Wire1.requestFrom(ADDRESS, uint8_t(1)) == 1;
    const uint8_t control = answered ? Wire1.read() : 0;
    if (!answered) {
        if (!warned) {
            warned = true;
            const char* warning = "Verstaerker antwortet nicht - Pyramide ohne Strom?";
            status::showError(warning, 0);
            Serial.println(warning);
        }
        return;
    }
    if ((control & ENABLED) != ENABLED) {
        Serial.printf("[Board] amplifier was off (SYSCTRL 0x%02X), setting it up again\n", control);
        const bool ready = echoPyramid.pa().begin();
        echoPyramid.codec().setVolume(volume::level());
        echoPyramid.codec().mute(false);
        Serial.println(ready ? "[Board] amplifier ready again" : "[Board] amplifier: setup failed");
    }
    if (warned) {
        warned = false;
        status::clearError();
    }
}

}  // namespace board
