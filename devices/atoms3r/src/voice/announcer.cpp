#include "voice/announcer.h"

#include <Arduino.h>

#include "assistant/agent.h"
#include "audio/audio.h"
#include "voice/live_session.h"

namespace announcer {
namespace {

int16_t* pcm = nullptr;
size_t samples = 0, position = 0;

// Ends the announcement now: up to 10 s of it are already in the speaker queue
// (audio::SPEAKER_BYTES), which played on under the button or a starting conversation (07.10.).
void cut()
{
    free(pcm);
    pcm = nullptr;
    audio::flushSpeaker();
}

}  // namespace

void update()
{
    if (!pcm && !live::open() && agent::takeSpeech(pcm, samples)) {
        position = 0;
        audio::queueTone(880, 120);
    }
    if (!pcm) return;
    // Fed in as the speaker queue drains; a conversation wins. Its audio comes only after the
    // connection is up, so the flush takes nothing of it.
    if (live::open()) {
        cut();
        return;
    }
    position += audio::queueSpeaker(pcm + position, samples - position);
    if (position >= samples) {
        free(pcm);
        pcm = nullptr;
    }
}

bool active() { return pcm || (!live::open() && audio::speaking()); }

void stop()
{
    const int waiting = agent::dropSpeech();
    cut();
    Serial.printf("[Announcer] stopped by the user (%d more dropped)\n", waiting);
}

}  // namespace announcer
