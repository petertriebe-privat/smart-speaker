// Echo canceller measurement: plays speech-like noise and logs how much echo remains.
//   pio run -e aec-test -t upload && pio device monitor
// ERLE = echo attenuation in dB. In the "sprechen" phase the residual should rise
// clearly above the quiet phases: that is the user's voice GPT Live will hear.
#include <Arduino.h>
#include <M5EchoPyramid.h>
#include <M5Unified.h>

#include "audio/audio.h"

M5EchoPyramid ep;

struct Average {
    float mic = 0, ref = 0, echo = 0, residual = 0;
    uint32_t micros = 0, maxMicros = 0;
    int n = 0;
    void add(const audio::EchoStats& s)
    {
        mic += s.micDb; ref += s.refDb; echo += s.echoDb; residual += s.residualDb;
        micros += s.aecMicros; maxMicros = max(maxMicros, s.aecMicros);
        ++n;
    }
};

// Low-passed noise with a 4 Hz syllable envelope, roughly speech-shaped.
void queueSpeechNoise(float amplitude)
{
    static float lowpass = 0, phase = 0;
    int16_t buffer[240];
    for (int i = 0; i < 240; ++i) {
        lowpass = 0.7f * lowpass + 0.3f * (random(-32768, 32767) / 32768.0f);
        phase += 4.0f / audio::API_RATE;
        const float envelope = max(0.0f, sinf(2 * PI * phase));
        buffer[i] = int16_t(amplitude * 32767 * 2.5f * lowpass * envelope);
    }
    audio::queueSpeaker(buffer, 240);
}

void runPhase(const char* name, uint32_t ms, int volume, float amplitude)
{
    ep.codec().setVolume(volume);
    Serial.printf("\n== %s (Volume %d) ==\n", name, volume);
    Serial.println("  mic   ref  echo resid  ERLE delay  us(max)   rawL  rawR");
    const uint32_t start = millis();
    uint32_t lastPrint = start, lastSample = start;
    Average avg;
    int16_t discard[480];
    while (millis() - start < ms) {
        if (amplitude > 0 && audio::speakerQueued() < 4800) queueSpeechNoise(amplitude);
        while (audio::readMic(discard, 480)) {}
        if (millis() - lastSample >= 10) {
            lastSample = millis();
            avg.add(audio::echoStats());
        }
        if (millis() - lastPrint >= 500 && avg.n) {
            lastPrint = millis();
            const auto s = audio::echoStats();
            Serial.printf("%5.1f %5.1f %5.1f %5.1f %5.1f %5d %4lu(%4lu) %6d %5d\n",
                          avg.mic / avg.n, avg.ref / avg.n, avg.echo / avg.n, avg.residual / avg.n,
                          (avg.mic - avg.residual) / avg.n, s.delay, avg.micros / avg.n,
                          avg.maxMicros, s.rawLeft, s.rawRight);
            avg = Average();
        }
        delay(1);
    }
}

void setup()
{
    auto cfg = M5.config();
    cfg.internal_spk = false;
    cfg.internal_mic = false;
    M5.begin(cfg);
    Serial.begin(115200);
    Wire1.end();
    ep.begin(&Wire1, 38, 39, 6, 8, 5, 7, audio::CODEC_RATE);
    Wire1.setTimeOut(20);
    ep.codec().mute(false);
    if (!audio::begin()) { Serial.println("audio::begin failed"); return; }
    audio::setStreaming(true);
    delay(3000);  // let the USB serial monitor attach

    runPhase("Stille", 3000, 50, 0);
    runPhase("Rauschen - bitte still sein", 10000, 50, 0.3f);
    runPhase("Stille", 2000, 50, 0);
    runPhase("Rauschen laut - bitte still sein", 6000, 80, 0.5f);
    runPhase("Rauschen - JETZT normal laut sprechen", 10000, 50, 0.3f);
    runPhase("Stille", 2000, 50, 0);
    if (audio::echoStats().rawLeft == 0)
        Serial.println("\nKeine Mikrofondaten: Pyramide mit eigener USB-Versorgung betreiben");
    Serial.println("\nFERTIG");
}

void loop() { delay(1000); }
