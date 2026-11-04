#include "ui/display.h"

#include <M5Unified.h>
#include <WiFi.h>

#include "audio/audio.h"
#include "audio/volume.h"
#include "audio/wakeword.h"
#include "config.h"
#include "system/network.h"
#include "ui/mute.h"
#include "ui/status.h"
#include "voice/live_session.h"
#include "voice/transcript.h"

namespace display {
namespace {

constexpr uint32_t REDRAW_MS = 200;
constexpr size_t TRANSCRIPT_CHARS = 120;  // six lines of 21 characters

M5Canvas canvas(&M5.Display);

struct Title {
    const char* text;
    uint16_t color;
};

bool offline() { return !network::connected() && !live::open(); }

Title title()
{
    if (status::fatal()) return {"FEHLER", TFT_RED};
    switch (live::state()) {
        case live::State::Idle:
            if (offline()) return {"WLAN...", TFT_YELLOW};
            return mute::on() ? Title{"Mikro aus", TFT_RED} : Title{"Bereit", TFT_GREEN};
        case live::State::Listening:  // the first words after the wake word (voice/first_words)
            return {"Hoere zu", TFT_CYAN};
        case live::State::Connecting:
        case live::State::Starting:
            // After the wake word the microphone already records.
            if (live::onPhone()) return {"Anruf", TFT_YELLOW};
            return live::startedByWakeWord() ? Title{"Hoere zu", TFT_CYAN} : Title{"Verbinde", TFT_YELLOW};
        case live::State::Live:
            if (live::onPhone()) return {"Telefonat", TFT_GREEN};
            if (audio::speaking()) return {"Spricht", TFT_GREEN};
            if (live::thinking()) return {"Denke...", TFT_VIOLET};
            return {"Hoere zu", TFT_CYAN};
        case live::State::Closing:
            return {"Beende", TFT_DARKGREY};
    }
    return {"Bereit", TFT_WHITE};
}

float cost(int seconds) { return seconds * 3.0f / 3600; }  // GPT Live: $3 per hour

const char* hint()
{
    if (offline()) return "Bitte warten...";
    if (live::open()) return "Taste: beenden";
    if (mute::on()) return "Taste 2 s: Mikro an";
    return wakeword::ready() ? "Sag: Hey Jarvis" : "Taste: Gespraech starten";
}

void drawWifi()
{
    canvas.setCursor(2, 22);
    if (offline()) {
        canvas.setTextColor(TFT_YELLOW);
        canvas.printf("Verbinde mit %s", network::ssid());
    } else {
        canvas.setTextColor(TFT_GREEN);
        canvas.printf("WLAN ok  %d dBm", int(WiFi.RSSI()));
    }
}

void drawVolumeAndCost()
{
    canvas.setTextColor(TFT_LIGHTGREY);
    canvas.setCursor(2, 32);
    canvas.printf("Vol %d%%", volume::level());
    const int usage = live::usageSeconds();
    if (live::open()) canvas.printf("  %us  $%.3f", unsigned(live::elapsedSeconds()), cost(usage));
    else if (usage) canvas.printf("  Letzte %ds $%.3f", usage, cost(usage));
}

// An error, otherwise the end of the conversation.
void drawMessage()
{
    canvas.setCursor(0, 46);
    if (status::errorText().length()) {
        canvas.setTextColor(TFT_RED);
        canvas.print(status::errorText());
        return;
    }
    String text = transcript::text();
    text.trim();
    if (text.length() > TRANSCRIPT_CHARS) text.remove(0, text.length() - TRANSCRIPT_CHARS);
    canvas.setTextColor(TFT_WHITE);
    canvas.print(text);
}

}  // namespace

void begin()
{
    M5.Display.setRotation(DISPLAY_ROTATION);
    canvas.createSprite(M5.Display.width(), M5.Display.height());
    canvas.setTextWrap(true);
}

void update()
{
    static uint32_t lastDraw = 0;
    if (millis() - lastDraw < REDRAW_MS) return;
    lastDraw = millis();

    canvas.fillSprite(TFT_BLACK);
    const Title t = title();
    canvas.setTextSize(2);
    canvas.setTextColor(t.color);
    canvas.setCursor(2, 2);
    canvas.print(t.text);
    canvas.setTextSize(1);
    drawWifi();
    drawVolumeAndCost();
    drawMessage();
    canvas.setTextColor(TFT_DARKGREY);
    canvas.setCursor(2, 118);
    canvas.print(hint());
    canvas.pushSprite(0, 0);
}

}  // namespace display
