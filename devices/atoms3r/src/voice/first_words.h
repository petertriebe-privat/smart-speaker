#pragma once

#include <Arduino.h>

// The first words after the wake word, decided before GPT Live gets any audio (#106; the Echo's
// core/cmd/assistant/capture.go and intent.go). voice/live_session records the
// first utterance; this judges it in the registry's worker (TLS, seconds): the transcription, the
// quick home rules (voice/quick_home), then the Decisions API with the device's commands and
// whether the words were meant for the assistant at all.
namespace first_words {

enum class Verdict {
    NoWords,   // nothing understood (noise, a scrap): listen on
    NotMeant,  // words, but not for the assistant (the TV, people in the room): end silently
    Done,      // a home command ran
    Failed,    // a home command failed: GPT Live says why (detail)
    Spoken,    // the time or the date: `speech` holds the answer (24 kHz PCM16 in PSRAM)
    Live,      // anything else: GPT Live answers, with the recording
};

struct Result {
    Verdict verdict = Verdict::NoWords;
    String text;    // the transcript
    String detail;  // Failed: what went wrong
    int16_t* speech = nullptr;  // Spoken: the caller frees it
    size_t speechSamples = 0;
};

// Opens the connection to OpenAI ahead, in the worker (at the wake word, while the user speaks).
void prepare();
// Closes it, in the worker (the first words ended without a verdict that closed it).
void release();
// Judges pcm[0..samples) (24 kHz; must stay unchanged until the result is taken) in the worker.
// False when a judgement still runs or the worker's queue is full.
bool judge(const int16_t* pcm, size_t samples);
bool busy();
// The result of the last judge() once it is done; false while it runs.
bool take(Result& out);
// Drops the result of a running judgement (the start was given up).
void cancel();

// What Jarvis says to "time" or "date" now.
String clockAnswer(bool date);

}  // namespace first_words
