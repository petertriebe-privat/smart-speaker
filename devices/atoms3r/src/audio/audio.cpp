#include "audio/audio.h"

#include <driver/i2s.h>
#include <esp_heap_caps.h>
#include <esp_timer.h>
#include <freertos/stream_buffer.h>

#include "audio/aec.h"
#include "config.h"

// The Echo Pyramid library installs the legacy I2S driver only on IDF4.
#if ESP_IDF_VERSION_MAJOR >= 5
#error "Revisit the I2S calls in audio.cpp before upgrading this project to IDF5"
#endif

namespace audio {
namespace {

constexpr int BLOCK = 240;                             // API samples per block = 10 ms
constexpr size_t SPEAKER_BYTES = API_RATE * 2 * 10;    // GPT Live streams in real time
constexpr size_t MIC_BYTES = API_RATE * 2 * 8;         // speech after the wake word, until the session runs
constexpr size_t WAKE_BYTES = 16000 * 2 / 2;           // 0.5 s at 16 kHz for the wake word task
constexpr size_t JITTER_BYTES = API_RATE * 2 * JITTER_MS / 1000;
constexpr int LOUD_PEAK = 200;                         // ~ -44 dBFS: audible playback
constexpr float REF_ACTIVE = 1e-6f;                    // -60 dBFS reference: worth adapting
constexpr int WARMUP_BLOCKS = 150;                     // 1.5 s of adaptation

StreamBufferHandle_t speaker = nullptr, mic = nullptr, wake = nullptr;
volatile bool streaming = false, flushRequested = false, wakeFeed = false;
volatile uint32_t lastLoud = 0, aecMicros = 0;
volatile int micPeak = 0, speakerPeak = 0, rawLeftPeak = 0, rawRightPeak = 0;
volatile uint32_t lastReadBytes = 0;
volatile uint32_t voiceBlocks = 0, lastVoice = 0;  // since streaming started (see voiceMs)
volatile uint32_t underrunCount = 0;

EchoCanceller aec;
EchoCanceller::Stats stats;
int adaptedBlocks = 0;

// The microphone's level over the last seconds for the peer link's wake-word claims
// (system/peer): mean square per LEVEL_BLOCKS blocks (50 ms), before the echo canceller, also
// during a conversation. Written by the audio task only.
constexpr int LEVEL_BLOCKS = 5, LEVEL_SLOTS = 100;  // 5 s
struct LevelSlot {
    uint32_t at;  // millis() at the end of the slot
    uint32_t meanSquare;
};
LevelSlot levels[LEVEL_SLOTS] = {};
int levelNext = 0;
portMUX_TYPE levelMux = portMUX_INITIALIZER_UNLOCKED;

void addLevel(const int16_t* pcm, int samples)
{
    static uint64_t sum = 0;
    static uint32_t count = 0;
    static int blocks = 0;
    for (int i = 0; i < samples; ++i) sum += int32_t(pcm[i]) * pcm[i];
    count += samples;
    if (++blocks < LEVEL_BLOCKS) return;
    portENTER_CRITICAL(&levelMux);
    levels[levelNext] = LevelSlot{millis(), uint32_t(sum / count)};
    levelNext = (levelNext + 1) % LEVEL_SLOTS;
    portEXIT_CRITICAL(&levelMux);
    sum = count = blocks = 0;
}

StreamBufferHandle_t createStream(size_t bytes)
{
    // FreeRTOS needs one spare byte of storage.
    auto* storage = static_cast<uint8_t*>(
        heap_caps_malloc(bytes + 1, MALLOC_CAP_SPIRAM | MALLOC_CAP_8BIT));
    auto* control = static_cast<StaticStreamBuffer_t*>(malloc(sizeof(StaticStreamBuffer_t)));
    if (!storage || !control) return nullptr;
    return xStreamBufferCreateStatic(bytes, 1, storage, control);
}

inline int16_t clamp16(int v) { return v > 32767 ? 32767 : v < -32768 ? -32768 : v; }

int toLevel(int peak) { return min(100, peak / 164); }  // full scale at -6 dBFS

// Half-band filter h = [-1 0 9 16 9 0 -1] / 32, 48 kHz -> 24 kHz.
// in: 6 samples history + 2 * BLOCK new samples; history is shifted for the next block.
void decimate(int16_t* in, int16_t* out)
{
    for (int m = 0; m < BLOCK; ++m) {
        const int16_t* x = in + 2 * m;
        out[m] = clamp16((16 * x[3] + 9 * (x[2] + x[4]) - x[0] - x[6]) / 32);
    }
    memcpy(in, in + 2 * BLOCK, 6 * sizeof(int16_t));
}

// 48 kHz -> 16 kHz for the wake word: windowed-sinc low-pass (7.2 kHz, Blackman, Q15), -14 dB
// at 8 kHz, -90 dB at 12 kHz. Every block has 480 samples, so the 3:1 phase stays aligned.
constexpr int WAKE_TAPS = 47;
constexpr int16_t WAKE_FIR[WAKE_TAPS] = {
    0, 1, 3, 0, -13, -27, -14, 40, 100, 83, -61, -253, -288, 0, 493, 749, 314, -772, -1719, -1355,
    1003, 4811, 8375, 9831, 8375, 4811, 1003, -1355, -1719, -772, 314, 749, 493, 0, -288, -253, -61,
    83, 100, 40, -14, -27, -13, 0, 3, 1, 0};

void feedWake(const int16_t* in48)
{
    static int16_t buffer[WAKE_TAPS - 1 + 2 * BLOCK];
    memcpy(buffer + WAKE_TAPS - 1, in48, 2 * BLOCK * sizeof(int16_t));
    int16_t out[2 * BLOCK / 3];
    for (int m = 0; m < 2 * BLOCK / 3; ++m) {
        const int16_t* x = buffer + 3 * m;
        int32_t acc = 0;
        for (int k = 0; k < WAKE_TAPS; ++k) acc += int32_t(WAKE_FIR[k]) * x[k];
        out[m] = clamp16(acc >> 15);
    }
    memmove(buffer, buffer + 2 * BLOCK, (WAKE_TAPS - 1) * sizeof(int16_t));
    if (wakeFeed && xStreamBufferSpacesAvailable(wake) >= sizeof(out))
        xStreamBufferSend(wake, out, sizeof(out), 0);
}

// Double talk: the user speaking makes the residual exceed the estimated echo.
// Adapting then would teach the filter to cancel the user's voice.
bool shouldAdapt()
{
    if (stats.ref < REF_ACTIVE) return false;
    return adaptedBlocks < WARMUP_BLOCKS || stats.residual < DOUBLE_TALK_RATIO * stats.echo;
}

void audioTask(void*)
{
    int16_t up[BLOCK + 3] = {};  // 3 samples history + block, 24 kHz
    int16_t downMic[2 * BLOCK + 6] = {}, downRef[2 * BLOCK + 6] = {};
    int16_t mic24[BLOCK], ref24[BLOCK], out[BLOCK];
    static int16_t stereo[2 * BLOCK * 2];
    bool playing = false;
    for (;;) {
        // The reader drains on request: resetting from the writer side is not safe.
        if (flushRequested) {
            while (xStreamBufferReceive(speaker, stereo, sizeof(stereo), 0)) {}
            flushRequested = false;
            playing = false;
        }
        // Jitter buffer: after an underrun, wait until JITTER_MS are queued again.
        if (!playing && xStreamBufferBytesAvailable(speaker) >= JITTER_BYTES) playing = true;
        const size_t got = playing ? xStreamBufferReceive(speaker, up + 3, BLOCK * 2, 0) / 2 : 0;
        if (playing && got < BLOCK) {
            playing = false;
            // Ran dry in the middle of audible speech: a gap the listener hears.
            if (millis() - lastLoud < 100) ++underrunCount;
        }
        memset(up + 3 + got, 0, (BLOCK - got) * 2);
        int peak = 0;
        // Upsampling with the same half-band filter (zero stuffing, gain 2).
        for (int n = 0; n < BLOCK; ++n) {
            const int16_t even = up[n + 1];
            const int16_t odd = clamp16((9 * (up[n + 1] + up[n + 2]) - up[n] - up[n + 3]) / 16);
            stereo[4 * n] = stereo[4 * n + 1] = even;
            stereo[4 * n + 2] = stereo[4 * n + 3] = odd;
            peak = max(peak, abs(int(even)));
        }
        memcpy(up, up + BLOCK, 3 * sizeof(int16_t));
        speakerPeak = peak;
        if (peak > LOUD_PEAK) lastLoud = millis();

        // Always write, silence included: the DMA would otherwise loop stale buffers.
        size_t bytes = 0;
        i2s_write(I2S_NUM_0, stereo, sizeof(stereo), &bytes, pdMS_TO_TICKS(100));
        bytes = 0;
        i2s_read(I2S_NUM_0, stereo, sizeof(stereo), &bytes, pdMS_TO_TICKS(100));
        const size_t frames = bytes / 4;
        lastReadBytes = bytes;
        int left = 0, right = 0;
        for (size_t i = 0; i < frames; ++i) {
            left = max(left, abs(int(stereo[2 * i])));
            right = max(right, abs(int(stereo[2 * i + 1])));
        }
        rawLeftPeak = left;
        rawRightPeak = right;
        // Left = MIC1 (voice), right = MIC2 (no speaker loopback, see docs/architecture.md).
        for (size_t i = 0; i < 2 * BLOCK; ++i) {
            downMic[6 + i] = i < frames ? stereo[2 * i] : 0;
            downRef[6 + i] = i < frames ? stereo[2 * i + 1] : 0;
        }
        feedWake(downMic + 6);
        decimate(downMic, mic24);
        decimate(downRef, ref24);
        addLevel(mic24, BLOCK);

        // Full duplex: GPT Live always gets the echo-cancelled microphone and decides
        // itself when the user interrupts. The decision uses the previous block's stats.
        const int64_t started = esp_timer_get_time();
        const bool adapt = ECHO_CANCELLER && shouldAdapt();
        if (ECHO_CANCELLER) aec.process(mic24, ref24, out, BLOCK, adapt);
        else memcpy(out, mic24, sizeof(out));
        if (adapt && adaptedBlocks < WARMUP_BLOCKS) ++adaptedBlocks;
        aecMicros = uint32_t(esp_timer_get_time() - started);
        stats = aec.stats();
        peak = 0;
        for (int m = 0; m < BLOCK; ++m) peak = max(peak, abs(int(out[m])));
        micPeak = peak;

        // Without echo cancellation the model hears itself through the microphone.
        if (ECHO_GATE && speaking()) memset(out, 0, sizeof(out));
        else if (streaming && peak > VOICE_PEAK) {
            ++voiceBlocks;
            lastVoice = millis();
        }
        if (streaming && xStreamBufferSpacesAvailable(mic) >= sizeof(out))
            xStreamBufferSend(mic, out, sizeof(out), 0);
    }
}

float dB(float energy) { return 10 * log10f(energy + 1e-12f); }

}  // namespace

bool begin()
{
    speaker = createStream(SPEAKER_BYTES);
    mic = createStream(MIC_BYTES);
    wake = createStream(WAKE_BYTES);
    if (!speaker || !mic || !wake) return false;
    i2s_zero_dma_buffer(I2S_NUM_0);
    // Above loop() on the same core: TLS work in loop() must not starve the DMA.
    return xTaskCreatePinnedToCore(audioTask, "audio", 8192, nullptr, 5, nullptr, 1) == pdPASS;
}

void setStreaming(bool on)
{
    // Starting fresh discards stale audio; already streaming keeps the wake word pre-buffer.
    if (on && !streaming) {
        int16_t discard[BLOCK];
        while (xStreamBufferReceive(mic, discard, sizeof(discard), 0)) {}
        voiceBlocks = 0;
    }
    streaming = on;
}

uint32_t voiceMs() { return voiceBlocks * BLOCK * 1000 / API_RATE; }
uint32_t lastVoiceAt() { return lastVoice; }
uint32_t underruns() { return underrunCount; }

void setWakeFeed(bool on) { wakeFeed = on; }

size_t readWake(int16_t* dst, size_t samples, TickType_t timeout)
{
    return xStreamBufferReceive(wake, dst, samples * 2, timeout) / 2;
}

size_t micAvailable() { return xStreamBufferBytesAvailable(mic) / 2; }

size_t readMic(int16_t* dst, size_t samples)
{
    return xStreamBufferReceive(mic, dst, samples * 2, 0) / 2;
}

size_t queueSpeaker(const int16_t* src, size_t samples)
{
    // Keep sample alignment: a partial send must not split an int16.
    const size_t bytes = min(samples * 2, xStreamBufferSpacesAvailable(speaker) & ~size_t(1));
    if (!bytes) return 0;  // FreeRTOS asserts on zero-length sends (full queue while feeding)
    return xStreamBufferSend(speaker, src, bytes, 0) / 2;
}

void queueTone(int frequency, int ms)
{
    int16_t buffer[BLOCK];
    const int total = API_RATE * ms / 1000, fade = API_RATE / 200;  // 5 ms ramps avoid clicks
    for (int start = 0; start < total; start += BLOCK) {
        const int n = min(BLOCK, total - start);
        for (int i = 0; i < n; ++i) {
            const int t = start + i;
            const float envelope = min(1.0f, min(t, total - t) / float(fade));
            buffer[i] = int16_t(8000 * envelope * sinf(2 * PI * frequency * t / API_RATE));
        }
        queueSpeaker(buffer, n);
    }
}

void flushSpeaker() { flushRequested = true; }

size_t speakerQueued() { return xStreamBufferBytesAvailable(speaker) / 2; }

// GPT Live streams silence too, so "speaking" means audible output, not queued data.
bool speaking() { return millis() - lastLoud < ECHO_TAIL_MS; }

EchoStats echoStats()
{
    const EchoCanceller::Stats s = stats;
    EchoStats e;
    e.micDb = dB(s.mic);
    e.refDb = dB(s.ref);
    e.echoDb = dB(s.echo);
    e.residualDb = dB(s.residual);
    e.delay = aec.peakTap();
    e.aecMicros = aecMicros;
    e.rawLeft = rawLeftPeak;
    e.rawRight = rawRightPeak;
    e.readBytes = lastReadBytes;
    return e;
}

float rms(uint32_t from, uint32_t to)
{
    uint64_t sum = 0;
    int n = 0;
    portENTER_CRITICAL(&levelMux);
    for (const LevelSlot& slot : levels) {
        if (slot.at && int32_t(slot.at - from) >= 0 && int32_t(to - slot.at) >= 0) {
            sum += slot.meanSquare;
            ++n;
        }
    }
    portEXIT_CRITICAL(&levelMux);
    return n ? sqrtf(float(sum) / n) : 0;
}

int micLevel() { return toLevel(micPeak); }
int speakerLevel() { return toLevel(speakerPeak); }

}  // namespace audio
