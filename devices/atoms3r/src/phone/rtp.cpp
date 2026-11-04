#include "phone/rtp.h"

#include <WiFi.h>
#include <WiFiUdp.h>
#include <atomic>
#include <esp_heap_caps.h>
#include <esp_random.h>
#include <freertos/stream_buffer.h>
#include <math.h>
#include <new>

#include "config.h"
#include "phone/g711.h"

namespace rtp {
namespace {

constexpr int RATE = 8000, API_RATE = 24000, FACTOR = API_RATE / RATE;
constexpr size_t FRAME = RATE / 50;               // 160 samples = 20 ms, one packet
constexpr size_t API_FRAME = FRAME * FACTOR;      // the same 20 ms at 24 kHz
constexpr size_t PREFILL = PHONE_JITTER_MS * API_RATE / 1000;  // before playing: against network jitter
constexpr int AUDIBLE = 300;                      // |sample| above this counts as speaking
constexpr int TAPS = 33;
// A pressed key: 100 ms tone, the RFC 4733 end packet three times, then a pause before the next.
constexpr int KEY_TONE = 5, KEY_END = 3, KEY_FRAMES = KEY_TONE + KEY_END + 5;

// Low-pass at 24 kHz with a cutoff of 3.6 kHz (the telephone band) for both directions: before
// keeping every third sample (else the assistant's sibilants fold back as noise) and after stuffing two
// zeros between the caller's samples (else the spectrum repeats above 4 kHz).
float taps[TAPS];

void makeTaps()
{
    const float cutoff = 3600.0f / API_RATE;
    float sum = 0;
    for (int i = 0; i < TAPS; ++i) {
        const float n = i - (TAPS - 1) / 2.0f;
        const float sinc = n == 0 ? 2 * cutoff : sinf(2 * PI * cutoff * n) / (PI * n);
        const float window = 0.54f - 0.46f * cosf(2 * PI * i / (TAPS - 1));  // Hamming
        taps[i] = sinc * window;
        sum += taps[i];
    }
    for (float& tap : taps) tap /= sum;  // gain 1 at 0 Hz
}

// The filter over a 24 kHz stream; keeps its history between packets.
struct Fir {
    float history[TAPS] = {};
    int at = 0;
    void push(float x)
    {
        history[at] = x;
        at = (at + 1) % TAPS;
    }
    float output() const
    {
        float y = 0;
        for (int i = 0, k = at; i < TAPS; ++i) {
            k = (k == 0 ? TAPS : k) - 1;
            y += taps[i] * history[k];
        }
        return y;
    }
    void clear() { *this = Fir(); }
};

int16_t clamp(float x) { return int16_t(x > 32767 ? 32767 : x < -32768 ? -32768 : x); }

uint32_t be32(const uint8_t* p) { return uint32_t(p[0]) << 24 | uint32_t(p[1]) << 16 | uint32_t(p[2]) << 8 | p[3]; }

void put32(uint8_t* p, uint32_t v)
{
    p[0] = v >> 24;
    p[1] = v >> 16;
    p[2] = v >> 8;
    p[3] = v;
}

// Stream buffers in PSRAM (single writer, single reader): mic = task -> conversation,
// speaker = conversation -> task.
StreamBufferHandle_t mic = nullptr, speaker = nullptr;
QueueHandle_t digits = nullptr, presses = nullptr;

StreamBufferHandle_t makeBuffer(size_t bytes)
{
    auto* storage = static_cast<uint8_t*>(heap_caps_malloc(bytes + 1, MALLOC_CAP_SPIRAM | MALLOC_CAP_8BIT));
    auto* control = static_cast<StaticStreamBuffer_t*>(
        heap_caps_malloc(sizeof(StaticStreamBuffer_t), MALLOC_CAP_INTERNAL | MALLOC_CAP_8BIT));
    if (!storage || !control) return nullptr;
    return xStreamBufferCreateStatic(bytes, 1, storage, control);
}

// Set by start() before `running`; read by the task.
IPAddress remoteIp;
uint16_t remotePort = 0;
uint8_t payloadType = 8;
int dtmfType = -1;
std::atomic<bool> running{false}, streaming{false}, flushRequested{false};
std::atomic<int> generation{0};
std::atomic<uint32_t> lastAudible{0};

uint8_t encode(int16_t sample) { return payloadType == 0 ? g711::encodeUlaw(sample) : g711::encodeAlaw(sample); }
int16_t decode(uint8_t code) { return payloadType == 0 ? g711::decodeUlaw(code) : g711::decodeAlaw(code); }

struct Stream {
    WiFiUDP udp;
    bool bound = false;
    Fir down, up;
    uint16_t sequence = 0;
    uint32_t timestamp = 0, ssrc = 0, lastEvent = 0xFFFFFFFF;
    bool first = true, playing = false;
    int generation = -1;
    char key = 0;      // the key being pressed, 0 = none
    int keyFrame = 0;  // 20 ms frames since it started
    uint32_t keyStamp = 0;
    uint8_t packet[1500];

    void reset()
    {
        down.clear();
        up.clear();
        sequence = uint16_t(esp_random());
        timestamp = esp_random();
        ssrc = esp_random();
        first = true;
        playing = false;
        lastEvent = 0xFFFFFFFF;
        key = 0;
    }

    // The caller's audio: G.711 -> 24 kHz into the microphone queue.
    void receiveAudio(const uint8_t* payload, size_t length)
    {
        int16_t out[API_FRAME];
        size_t count = 0;
        for (size_t i = 0; i < length; ++i) {
            const float sample = FACTOR * float(decode(payload[i]));  // the zeros take 2/3 of the energy
            for (int k = 0; k < FACTOR; ++k) {
                up.push(k == 0 ? sample : 0);
                out[count++] = clamp(up.output());
                if (count == API_FRAME) {
                    xStreamBufferSend(mic, out, count * 2, 0);
                    count = 0;
                }
            }
        }
        if (count) xStreamBufferSend(mic, out, count * 2, 0);
    }

    // RFC 4733: the end packet (sent three times) reports the key once per event.
    void receiveEvent(const uint8_t* payload, size_t length, uint32_t stamp)
    {
        if (length < 4 || !(payload[1] & 0x80) || stamp == lastEvent) return;
        lastEvent = stamp;
        const uint8_t event = payload[0];
        const char digit = event <= 9 ? char('0' + event) : event == 10 ? '*' : event == 11 ? '#' : 0;
        if (digit) xQueueSend(digits, &digit, 0);
    }

    void receive()
    {
        while (udp.parsePacket() > 0) {
            const int n = udp.read(packet, sizeof(packet));
            if (n < 12 || (packet[0] >> 6) != 2) continue;
            int offset = 12 + 4 * (packet[0] & 0x0F);  // CSRC list
            if ((packet[0] & 0x10) && n >= offset + 4)  // header extension
                offset += 4 + 4 * ((packet[offset + 2] << 8) | packet[offset + 3]);
            int end = n;
            if (packet[0] & 0x20) end -= packet[n - 1];  // padding
            if (offset >= end) continue;
            const uint8_t type = packet[1] & 0x7F;
            if (type == payloadType) {
                if (streaming) receiveAudio(packet + offset, end - offset);
            } else if (dtmfType >= 0 && type == dtmfType) {
                receiveEvent(packet + offset, end - offset, be32(packet + 4));
            }
        }
    }

    // One 20 ms packet of the assistant's voice, or silence while nothing is queued.
    void send()
    {
        int16_t pcm[API_FRAME] = {};
        const size_t queued = xStreamBufferBytesAvailable(speaker) / 2;
        if (!playing && queued >= PREFILL) playing = true;
        if (playing) {
            xStreamBufferReceive(speaker, pcm, sizeof(pcm), 0);  // short at the end: the rest stays 0
            if (queued < API_FRAME) playing = false;
        }
        uint8_t* payload = packet + 12;
        bool audible = false;
        for (size_t i = 0, j = 0; i < API_FRAME; ++i) {
            down.push(pcm[i]);
            if (i % FACTOR != FACTOR - 1) continue;
            const int16_t sample = clamp(down.output());
            audible |= abs(sample) > AUDIBLE;
            payload[j++] = encode(sample);
        }
        if (audible) lastAudible = millis();
        transmit(payloadType, first, timestamp, FRAME);  // marker: start of the stream
        timestamp += FRAME;
        first = false;
    }

    void transmit(uint8_t type, bool marker, uint32_t stamp, size_t length)
    {
        packet[0] = 0x80;  // version 2
        packet[1] = type | (marker ? 0x80 : 0);
        packet[2] = sequence >> 8;
        packet[3] = sequence;
        put32(packet + 4, stamp);
        put32(packet + 8, ssrc);
        udp.beginPacket(remoteIp, remotePort);
        udp.write(packet, 12 + length);
        udp.endPacket();
        ++sequence;
    }

    // One 20 ms frame of a pressed key instead of the assistant's voice (dropped meanwhile): an RFC 4733
    // event if the call negotiated telephone-event, else the two tones in the audio itself.
    void sendKey()
    {
        int16_t skipped[API_FRAME];
        xStreamBufferReceive(speaker, skipped, sizeof(skipped), 0);
        if (keyFrame == 0) keyStamp = timestamp;
        const char* keys = "0123456789*#";
        const int event = strchr(keys, key) - keys;
        uint8_t* payload = packet + 12;
        if (dtmfType >= 0 && keyFrame < KEY_TONE + KEY_END) {
            const bool end = keyFrame >= KEY_TONE;
            const uint16_t duration = FRAME * (end ? KEY_TONE : keyFrame + 1);
            payload[0] = event;
            payload[1] = (end ? 0x80 : 0) | 10;  // end bit, volume -10 dBm0
            payload[2] = duration >> 8;
            payload[3] = duration;
            transmit(dtmfType, keyFrame == 0, keyStamp, 4);
        } else {
            static const int16_t low[] = {941, 697, 697, 697, 770, 770, 770, 852, 852, 852, 941, 941};
            static const int16_t high[] = {1336, 1209, 1336, 1477, 1209, 1336, 1477, 1209, 1336, 1477, 1209, 1477};
            for (size_t i = 0; i < FRAME; ++i) {
                const float t = float(keyFrame * FRAME + i) / RATE;
                const float tone = keyFrame < KEY_TONE && dtmfType < 0
                                       ? 6000 * (sinf(2 * PI * low[event] * t) + sinf(2 * PI * high[event] * t))
                                       : 0;
                payload[i] = encode(clamp(tone));
            }
            transmit(payloadType, false, timestamp, FRAME);
        }
        timestamp += FRAME;
        if (++keyFrame == KEY_FRAMES) key = 0;
    }
};

// The task runs only during a call: its stack is internal RAM, which the TLS handshakes and the
// memory's and scheduler's tasks need between calls (with a permanent 8 KB task they no longer
// found room after boot). Its state and packet buffer live in PSRAM.
Stream* stream = nullptr;
portMUX_TYPE taskMux = portMUX_INITIALIZER_UNLOCKED;
bool taskRunning = false;  // guarded by taskMux

void task(void*)
{
    Stream& s = *stream;
    s.bound = s.udp.begin(PHONE_RTP_PORT);
    if (!s.bound) Serial.println("[RTP] port not available");
    TickType_t wake = xTaskGetTickCount();
    for (;;) {
        vTaskDelayUntil(&wake, pdMS_TO_TICKS(20));
        if (flushRequested.exchange(false)) xStreamBufferReset(speaker);
        if (!running) {  // the call is over: clean up and end, unless a new call started meanwhile
            s.udp.stop();
            xStreamBufferReset(speaker);
            xStreamBufferReset(mic);
            s.generation = -1;
            taskENTER_CRITICAL(&taskMux);
            const bool over = !running;
            if (over) taskRunning = false;
            taskEXIT_CRITICAL(&taskMux);
            if (over) vTaskDelete(nullptr);
            s.bound = s.udp.begin(PHONE_RTP_PORT);
            continue;
        }
        if (s.generation != generation) {
            s.reset();
            s.generation = generation;
        }
        s.receive();
        if (!s.key && xQueueReceive(presses, &s.key, 0) == pdTRUE) s.keyFrame = 0;
        if (s.key)
            s.sendKey();
        else
            s.send();
    }
}

}  // namespace

bool begin()
{
    makeTaps();
    mic = makeBuffer(API_RATE * 2);          // 1 s
    speaker = makeBuffer(API_RATE * 2 * 4);  // 4 s: GPT Live sends in real time, bursts stay small
    digits = xQueueCreate(16, sizeof(char));
    presses = xQueueCreate(32, sizeof(char));
    void* memory = heap_caps_malloc(sizeof(Stream), MALLOC_CAP_SPIRAM | MALLOC_CAP_8BIT);
    if (memory) stream = new (memory) Stream();
    return mic && speaker && digits && presses && stream;
}

bool start(IPAddress remote, uint16_t port, uint8_t payload, int dtmf)
{
    running = false;
    remoteIp = remote;
    remotePort = port;
    payloadType = payload;
    dtmfType = dtmf;
    xQueueReset(digits);
    xQueueReset(presses);
    ++generation;
    running = true;
    bool create = false;
    taskENTER_CRITICAL(&taskMux);
    // cppcheck-suppress duplicateConditionalAssign ; create only says whether this call started it
    if (!taskRunning) taskRunning = create = true;
    taskEXIT_CRITICAL(&taskMux);
    // Above the main loop (priority 2, main.cpp): the 20 ms voice packets must not wait.
    if (create && xTaskCreatePinnedToCore(task, "rtp", 5120, nullptr, 3, nullptr, 1) != pdPASS) {
        taskENTER_CRITICAL(&taskMux);
        taskRunning = false;
        taskEXIT_CRITICAL(&taskMux);
        running = false;
        Serial.println("[RTP] task could not start (not enough memory)");
        return false;
    }
    return true;
}

void stop()
{
    running = false;
    streaming = false;
}

bool active() { return running; }

void setStreaming(bool on) { streaming = on; }

size_t micAvailable() { return mic ? xStreamBufferBytesAvailable(mic) / 2 : 0; }

size_t readMic(int16_t* dst, size_t samples) { return xStreamBufferReceive(mic, dst, samples * 2, 0) / 2; }

size_t queueSpeaker(const int16_t* src, size_t samples)
{
    if (!running) return samples;  // no call (any more): dropped like on a closed line
    return xStreamBufferSend(speaker, src, samples * 2, 0) / 2;
}

size_t speakerQueued() { return running && speaker ? xStreamBufferBytesAvailable(speaker) / 2 : 0; }

void flushSpeaker() { flushRequested = true; }

// By level, not by queued audio: GPT Live streams silence too, so the queue is never empty
// (with that test the assistant never counted as quiet on the phone and never hung up).
bool speaking() { return millis() - lastAudible < ECHO_TAIL_MS; }

bool takeDigit(char& digit) { return digits && xQueueReceive(digits, &digit, 0) == pdTRUE; }

bool pressKeys(const String& keys)
{
    if (!running || !presses || keys.length() > uxQueueSpacesAvailable(presses)) return false;
    for (char key : keys) xQueueSend(presses, &key, 0);
    return true;
}

}  // namespace rtp
