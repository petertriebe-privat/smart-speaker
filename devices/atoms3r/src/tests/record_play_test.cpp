#include <Arduino.h>
#include <M5Unified.h>
#include <M5EchoPyramid.h>
#include <esp_heap_caps.h>

// Pin assignment and audio format from the official AtomS3R RecordPlay demo.
constexpr int SAMPLE_RATE = 44100;
constexpr int FRAME_SIZE = 256; // Library's internal buffers hold at most 256 frames.
constexpr size_t TOTAL_SAMPLES = SAMPLE_RATE * 5;
constexpr uint32_t REPEAT_MS = 180;
M5EchoPyramid ep;
enum class State { Idle, Recording, Playback, Draining };
State state = State::Idle;
int16_t* recording = nullptr;
size_t position = 0;
bool hasRecording = false, fatalError = false;
int volume = 50;
uint8_t previousTouch = 0, heldTouch = 0;
uint32_t lastRepeat[2] = {}, lastDraw = 0, drainStarted = 0;
uint32_t feedbackStarted = 0;
bool errorFeedback = false;
int volumeFeedback = 0;
String touchText = "No touch yet", actionText = "Ready";
uint32_t touch2Count = 0, playStarts = 0, playDone = 0, touchErrors = 0;
uint32_t lastAudioProgress = 0, lastTouchPoll = 0;
uint32_t releaseSince[4] = {};
int peak = 0;
String diagnostic = "BtnA = play";

// The installed Arduino/IDF4 library waits forever in ep.read()/ep.write().
// Keep its initialization and codec control, but use bounded I2S transfers.
#if ESP_IDF_VERSION_MAJOR >= 5
#error "Revisit bounded audio transfers before upgrading this project to IDF5"
#endif

bool busy() { return state != State::Idle; }

void drawDisplay()
{
    M5.Display.fillScreen(TFT_BLACK);
    M5.Display.setTextSize(1);
    M5.Display.setTextColor(TFT_CYAN);
    M5.Display.setCursor(3, 1);
    M5.Display.printf("DIAG2 %c R:%c P:%d", (millis() / 500) % 2 ? '*' : '.',
                         hasRecording ? 'Y' : 'N', peak);
    M5.Display.setTextColor(TFT_WHITE);
    M5.Display.setCursor(3, 13);
    M5.Display.printf("Touch: %c %c %c %c", heldTouch & 1 ? '1' : '-',
        heldTouch & 2 ? '2' : '-', heldTouch & 4 ? '3' : '-', heldTouch & 8 ? '4' : '-');
    M5.Display.setCursor(3, 25);
    M5.Display.println(touchText);
    M5.Display.setCursor(3, 37);
    M5.Display.println(actionText);
    const char* status = fatalError ? "ERROR" : state == State::Recording ? "Recording" :
        busy() ? "Playback" : errorFeedback ? "ERROR" : "Ready";
    M5.Display.setCursor(3, 49);
    M5.Display.printf("Status: %s", status);
    M5.Display.setCursor(3, 61);
    M5.Display.printf("Volume: %d%%", volume);
    M5.Display.setCursor(3, 73);
    if (busy()) M5.Display.printf("%.1f / 5.0s  %u%%", position / float(SAMPLE_RATE),
                                  unsigned(position * 100 / TOTAL_SAMPLES));
    else M5.Display.print(hasRecording ? "Recording available" : "No recording yet");
    M5.Display.setCursor(3, 85);
    M5.Display.printf("T2:%lu Play:%lu/%lu", touch2Count, playStarts, playDone);
    M5.Display.setCursor(3, 97);
    M5.Display.printf("I2C errors: %lu", touchErrors);
    M5.Display.setCursor(3, 113);
    M5.Display.print(diagnostic);
    lastDraw = millis();
}

void showLeds()
{
    // Cache pixels: only changed LEDs generate I2C traffic during audio.
    static uint32_t previous[14];
    static bool initialized = false;
    int count = 14;
    uint32_t color = 0x000008;
    if (fatalError || errorFeedback) color = 0xFF0000;
    else if (volumeFeedback) {
        count = (volume * 14 + 99) / 100;
        color = volumeFeedback == 3 ? 0x0080FF : 0xFF6000;
    } else if (busy()) {
        count = max(1, int((position * 14 + TOTAL_SAMPLES - 1) / TOTAL_SAMPLES));
        color = state == State::Recording ? 0xFF0000 : 0x00FF00;
    }
    for (int i = 0; i < 14; ++i) {
        uint32_t pixel = i < count ? color : 0;
        if (!initialized || previous[i] != pixel) {
            for (int channel = 1; channel <= 2; ++channel)
                ep.ctrl().setRGB(channel, i, pixel >> 16, pixel >> 8, pixel);
            previous[i] = pixel;
        }
    }
    initialized = true;
}

void fail(const char* message, bool fatal = false)
{
    actionText = message;
    diagnostic = message; // Keep the last failure visible after the red feedback ends.
    errorFeedback = true;
    fatalError = fatal;
    feedbackStarted = millis();
    Serial.println(message);
    drawDisplay();
    showLeds();
}

void onTouch(int tp, bool physicalButton = false)
{
    touchText = physicalButton ? "BtnA pressed" : "TP" + String(tp) + " touched";
    errorFeedback = false;
    volumeFeedback = 0;
    feedbackStarted = millis();
    if (tp >= 3) {
        int next = constrain(volume + (tp == 3 ? 5 : -5), 0, 100);
        if (!ep.codec().setVolume(next)) { fail("Volume error"); return; }
        volume = next;
        volumeFeedback = tp;
        actionText = tp == 3 ? "Volume UP" : "Volume DOWN";
    } else if (busy()) {
        actionText = "Busy - try again";
    } else if (tp == 1) {
        hasRecording = false;
        position = 0;
        state = State::Recording;
        peak = 0;
        lastAudioProgress = millis();
        actionText = "Start 5s recording";
    } else if (!hasRecording) {
        fail("No recording yet");
        return;
    } else {
        if (!ep.codec().setVolume(volume) || !ep.codec().mute(false)) {
            fail("Codec play error"); return;
        }
        position = 0;
        state = State::Playback;
        ++playStarts;
        lastAudioProgress = millis();
        actionText = "Start playback";
    }
    drawDisplay(); // Visible before the next audio block is processed.
    showLeds();
}

void setup()
{
    auto cfg = M5.config();
    cfg.internal_spk = false;
    cfg.internal_mic = false;
    M5.begin(cfg);
    Serial.begin(115200);
    M5.Display.setRotation(0);
    M5.Display.setTextWrap(false);
    actionText = "Initializing...";
    drawDisplay();
    Wire1.end();
    ep.begin(&Wire1, 38, 39, 6, 8, 5, 7, SAMPLE_RATE);
    Wire1.setTimeOut(20);
    ep.ctrl().setBrightness(1, 60);
    ep.ctrl().setBrightness(2, 60);
    // begin() in this library always returns true; probe the controller explicitly.
    Wire1.beginTransmission(STM32_I2C_ADDR);
    if (Wire1.endTransmission() != 0) { fail("Pyramid not found", true); return; }
    if (!ep.codec().setVolume(volume) || !ep.codec().mute(false)) {
        fail("Codec init error", true); return;
    }
    recording = static_cast<int16_t*>(heap_caps_malloc(
        TOTAL_SAMPLES * sizeof(int16_t), MALLOC_CAP_SPIRAM | MALLOC_CAP_8BIT));
    if (!recording) recording = static_cast<int16_t*>(malloc(TOTAL_SAMPLES * sizeof(int16_t)));
    if (!recording) { fail("Not enough memory", true); return; }
    actionText = "Ready";
    drawDisplay();
    showLeds();
}

void loop()
{
    M5.update();
    if (fatalError) { delay(20); return; }
    const uint32_t now = millis();
    if (M5.BtnA.wasPressed()) onTouch(2, true);
    if (now - lastTouchPoll >= 15) {
        lastTouchPoll = now;
        // Read all four keys in one transaction. The library masks failed reads poorly.
        Wire1.beginTransmission(STM32_I2C_ADDR);
        Wire1.write(TOUCH_BUTTON_STATUS_REG_ADDR);
        if (Wire1.endTransmission(false) != 0 ||
        Wire1.requestFrom(uint8_t(STM32_I2C_ADDR), uint8_t(4)) != 4) {
            ++touchErrors;
            if (touchErrors == 1) fail("Touch I2C retry");
        } else {
            heldTouch = 0;
            for (int i = 0; i < 4; ++i) if (Wire1.read()) heldTouch |= 1 << i;
            for (int tp = 1; tp <= 4; ++tp) {
                const uint8_t bit = 1 << (tp - 1);
                const bool pressed = heldTouch & bit;
                const bool rising = pressed && !(previousTouch & bit);
                // Recognize touch immediately; debounce only release to prevent retriggers.
                if (pressed) { previousTouch |= bit; releaseSince[tp - 1] = 0; }
                else if (previousTouch & bit) {
                    if (!releaseSince[tp - 1]) releaseSince[tp - 1] = now;
                    if (now - releaseSince[tp - 1] >= 30) previousTouch &= ~bit;
                }
                const bool repeat = tp >= 3 && pressed && now - lastRepeat[tp - 3] >= REPEAT_MS;
                if (rising || repeat) {
                    if (tp == 2 && rising) ++touch2Count;
                    onTouch(tp);
                    if (tp >= 3) lastRepeat[tp - 3] = now;
                }
            }
        }
    }
    if (millis() - feedbackStarted >= 700 && !heldTouch) {
        volumeFeedback = 0;
        errorFeedback = false;
    }

    int16_t stereo[FRAME_SIZE * 2];
    size_t transferred = 0;
    esp_err_t audioResult = ESP_OK;
    if (state == State::Playback) {
        const int frames = min(size_t(FRAME_SIZE), TOTAL_SAMPLES - position);
        for (int i = 0; i < frames; ++i)
            stereo[2 * i] = stereo[2 * i + 1] = recording[position + i];
        audioResult = i2s_write(I2S_NUM_0, stereo, frames * 4, &transferred, pdMS_TO_TICKS(20));
        position += transferred / 4;
        if (transferred) lastAudioProgress = millis();
        if (position == TOTAL_SAMPLES) {
            state = State::Draining;
            drainStarted = millis();
        }
    } else {
        // Drain RX while idle too, so TP1 starts with fresh microphone samples.
        const int frames = state == State::Recording ?
            min(size_t(FRAME_SIZE), TOTAL_SAMPLES - position) : FRAME_SIZE;
        audioResult = i2s_read(I2S_NUM_0, stereo, frames * 4, &transferred, pdMS_TO_TICKS(20));
        if (transferred) lastAudioProgress = millis();
        if (state == State::Recording) {
            for (size_t i = 0; i < transferred / 4; ++i) {
                const int16_t sample = stereo[2 * i];
                recording[position++] = sample;
                peak = max(peak, abs(int(sample)));
            }
            if (position == TOTAL_SAMPLES) {
                showLeds();
                hasRecording = true;
                state = State::Idle;
                actionText = "Saved 5s - TP2 play";
                drawDisplay();
            }
        } else if (state == State::Draining && millis() - drainStarted >= 40) {
            // Six DMA buffers of 256 frames need up to 35 ms to finish playing.
            state = State::Idle;
            ++playDone;
            actionText = "Playback finished";
            drawDisplay();
        }
    }
    if ((audioResult != ESP_OK && audioResult != ESP_ERR_TIMEOUT) ||
        (busy() && millis() - lastAudioProgress > 500)) {
        const bool wasRecording = state == State::Recording;
        state = State::Idle;
        i2s_zero_dma_buffer(I2S_NUM_0);
        fail(wasRecording ? "REC audio timeout" : "Audio I/O error");
    }
    showLeds();
    if (millis() - lastDraw >= 100) drawDisplay();
    yield();
}
