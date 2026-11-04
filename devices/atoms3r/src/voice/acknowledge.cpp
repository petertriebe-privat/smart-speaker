#include "voice/acknowledge.h"

#include <LittleFS.h>
#include <esp_heap_caps.h>

#include "assistant/tools.h"
#include "audio/audio.h"
#include "services/openai.h"
#include "services/psram_sink.h"
#include "system/network.h"
#include "system/tls_gate.h"

namespace acknowledge {
namespace {

// Changing a phrase: raise the version, the old files are then made again.
const char* const PHRASES[] = {"Ja, Sir?", "Ich höre, Sir.", "Ja?"};
constexpr int COUNT = sizeof(PHRASES) / sizeof(PHRASES[0]);
constexpr int VERSION = 1;
constexpr const char* DIR = "/voice";
constexpr int16_t SILENCE = 400;  // TTS starts and ends with silence: trimmed away

struct Clip {
    int16_t* pcm = nullptr;
    size_t samples = 0;
};
Clip clips[COUNT];
volatile bool ready = false;  // all clips loaded (set by the worker, read by the main loop)
const Clip* current = nullptr;
size_t position = 0;

String path(int index) { return String(DIR) + "/ack" + VERSION + "_" + index + ".pcm"; }

// Trims the silence before and after the speech, keeps 30 ms around it.
void trim(Clip& clip)
{
    size_t first = 0, last = clip.samples;
    while (first < clip.samples && abs(clip.pcm[first]) < SILENCE) ++first;
    while (last > first && abs(clip.pcm[last - 1]) < SILENCE) --last;
    const size_t margin = audio::API_RATE * 30 / 1000;
    first = first > margin ? first - margin : 0;
    last = min(clip.samples, last + margin);
    memmove(clip.pcm, clip.pcm + first, (last - first) * 2);
    clip.samples = last - first;
}

bool load(int index)
{
    File file = LittleFS.open(path(index), "r");
    if (!file || file.size() < 2) return false;
    Clip& clip = clips[index];
    clip.samples = file.size() / 2;
    clip.pcm = static_cast<int16_t*>(heap_caps_malloc(clip.samples * 2, MALLOC_CAP_SPIRAM | MALLOC_CAP_8BIT));
    if (!clip.pcm || file.read(reinterpret_cast<uint8_t*>(clip.pcm), clip.samples * 2) != clip.samples * 2) {
        free(clip.pcm);
        clip = Clip();
        return false;
    }
    return true;
}

bool make(int index)
{
    PsramSink sink(5 * audio::API_RATE * 2);
    const String error = openai::speech(PHRASES[index], "pcm", sink);
    if (error.length()) {
        Serial.printf("[Ack] \"%s\": %s\n", PHRASES[index], error.c_str());
        return false;
    }
    Clip clip;
    clip.pcm = reinterpret_cast<int16_t*>(sink.data);
    clip.samples = sink.size / 2;
    trim(clip);
    LittleFS.mkdir(DIR);
    File file = LittleFS.open(path(index), "w");
    const bool written = file && file.write(reinterpret_cast<uint8_t*>(clip.pcm), clip.samples * 2) == clip.samples * 2;
    file.close();
    if (!written) LittleFS.remove(path(index));
    return written && load(index);
}

// Background work of the registry's worker: loads the clips, makes missing ones once.
void prepare()
{
    static uint32_t retryAt = 0;
    if (ready || !network::connected() || int32_t(millis() - retryAt) < 0) return;
    for (int i = 0; i < COUNT; ++i) {
        if (clips[i].pcm || load(i)) continue;
        tls_gate::TryHold gate;  // one background TLS job at a time
        if (!gate || !make(i)) {
            retryAt = millis() + (gate ? 60000 : 1000);
            return;
        }
        Serial.printf("[Ack] \"%s\" made (%.2f s)\n", PHRASES[i], clips[i].samples / float(audio::API_RATE));
    }
    ready = true;
}

}  // namespace

void begin() { tools::addBackgroundWork(prepare); }

bool start()
{
    if (!ready) return false;
    const int index = random(COUNT);
    current = &clips[index];
    position = 0;
    Serial.printf("[Ack] %s\n", PHRASES[index]);
    update();
    return true;
}

void update()
{
    if (!current) return;
    position += audio::queueSpeaker(current->pcm + position, current->samples - position);
    if (position >= current->samples) current = nullptr;
}

bool playing() { return current || audio::speaking(); }

}  // namespace acknowledge
