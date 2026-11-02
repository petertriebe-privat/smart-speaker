#pragma once

#include <Arduino.h>

// Full-duplex audio between the Echo Pyramid codec and GPT Live.
// The codec runs at 48 kHz stereo (SI5351 only offers 16/44.1/48 kHz clocks);
// the API side is 24 kHz mono PCM16, resampled 2:1 with a half-band filter.
// The microphone is echo-cancelled and streamed continuously, also while the
// speaker plays: GPT Live itself decides when the user interrupts.
// Speaker functions are called from the network task only, mic reads likewise.
namespace audio {

constexpr int CODEC_RATE = 48000;
constexpr int API_RATE = 24000;

bool begin();  // call after M5EchoPyramid::begin()

void setStreaming(bool on);  // capture microphone into the outgoing queue
size_t micAvailable();       // samples at API_RATE
size_t readMic(int16_t* dst, size_t samples);
// Local voice detection (VOICE_PEAK), not while the echo gate mutes the microphone.
uint32_t voiceMs();      // voice heard since streaming started
uint32_t lastVoiceAt();  // millis() of the last voice block
uint32_t underruns();    // the speaker ran dry during audible speech (since boot)

// 16 kHz microphone audio for the wake word task (blocking read, single reader).
void setWakeFeed(bool on);
size_t readWake(int16_t* dst, size_t samples, TickType_t timeout);

size_t queueSpeaker(const int16_t* src, size_t samples);  // returns samples accepted
void queueTone(int frequency, int ms);
void flushSpeaker();
size_t speakerQueued();  // samples
bool speaking();         // audible output within the last ECHO_TAIL_MS

struct EchoStats {
    float micDb, refDb, echoDb, residualDb;  // last 10 ms block, dBFS
    int delay;                               // echo path peak, samples at 24 kHz
    uint32_t aecMicros;
    int rawLeft, rawRight;                   // raw 48 kHz peaks (MIC1, MIC3)
    uint32_t readBytes;                      // last i2s_read result
};
EchoStats echoStats();

// The microphone's RMS (int16 scale, before the echo canceller) between two millis() times
// within the last 5 s, 0 without data. For the peer link's wake-word claims (system/peer).
float rms(uint32_t from, uint32_t to);

int micLevel();      // 0..100
int speakerLevel();  // 0..100

}  // namespace audio
