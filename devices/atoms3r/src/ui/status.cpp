#include "ui/status.h"

#include "config.h"
#include "voice/live_session.h"

namespace status {
namespace {

String error;
uint32_t errorUntil = 0, volumeUntil = 0;
bool fatalError = false;

}  // namespace

void showError(const String& text, uint32_t ms)
{
    error = text;
    errorUntil = millis() + ms;
}

void clearError() { error = ""; }

void setFatal(const String& text)
{
    Serial.println(text);
    error = text;
    fatalError = true;
}

void showVolume() { volumeUntil = millis() + VOLUME_DISPLAY_MS; }

void update()
{
    if (error.length() && !errorActive() && live::open()) error = "";
}

bool fatal() { return fatalError; }
bool errorActive() { return fatalError || millis() < errorUntil; }
bool volumeVisible() { return millis() < volumeUntil; }
const String& errorText() { return error; }

}  // namespace status
