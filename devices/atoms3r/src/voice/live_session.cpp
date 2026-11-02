#include "voice/live_session.h"

#include <ArduinoJson.h>
#include <WebSocketsClient.h>
#include <esp_heap_caps.h>
#include <mbedtls/base64.h>
#include <vector>

#include "assistant/agenda.h"
#include "assistant/memory.h"
#include "assistant/people.h"
#include "assistant/prompt.h"
#include "assistant/tasks.h"
#include "assistant/tools.h"
#include "audio/audio.h"
#include "ca_certs.h"
#include "config.h"
#include "phone/rtp.h"
#include "prompts.h"
#include "secrets.h"
#include "services/mcp.h"
#include "services/psram_json.h"
#include "system/error_log.h"
#include "system/file_util.h"
#include "assistant/shared.h"
#include "system/network.h"
#include "system/peer.h"
#include "system/time_util.h"
#include "system/utf8.h"
#include "ui/mute.h"
#include "ui/status.h"
#include "voice/acknowledge.h"
#include "voice/announcer.h"
#include "voice/conversation_end.h"
#include "voice/first_words.h"
#include "voice/quick_home.h"
#include "voice/transcript.h"

namespace live {
namespace {

constexpr size_t MIC_CHUNK = audio::API_RATE / 25;  // 40 ms per append event
// The audio recorded while connecting (seconds, >200 KB base64) went out in one go and held the
// loop for 2.5-3 s: The assistant's voice stalled and then lagged (05.10.). At most this many chunks per
// pass, so his audio keeps coming in between; the backlog still drains far faster than real time.
constexpr int MIC_CHUNKS_PER_PASS = 5;
constexpr size_t DECODE_BYTES = 64 * 1024;
constexpr uint32_t CLOSE_TIMEOUT_MS = 5000;
constexpr size_t HELD_AUDIO_SAMPLES = size_t(audio::API_RATE) * QUICK_HOLD_AUDIO_MS / 1000;

// The hold (#75, as the Echo's live.Session hold): at the speaker GPT Live hears the user from
// the start, but what it says and the tools it calls wait until the device judged the first
// utterance. A quick home command (voice/quick_home) closes the conversation and the held answer
// is dropped: the session billed ~0 s on the Echo. Anything else releases it and the answer plays
// as if it had never waited (GPT Live takes 1.5-2 s to answer anyway).
struct HeldCall {
    String name, arguments, callId;
};
bool held = false;
uint32_t heldSince = 0;
String firstUser;            // GPT Live's transcript of the user while held
uint32_t firstUserAt = 0;    // its last delta
int16_t* heldAudio = nullptr;  // PSRAM
size_t heldSamples = 0;
std::vector<HeldCall> heldCalls;
void releaseHold(const char* why);
void dropHold();

WebSocketsClient ws;
String authHeader;
uint8_t* decodeBuffer = nullptr;

State current = State::Idle;
uint32_t stateSince = 0, sessionSince = 0;
time_t startedAt = 0;       // wall clock, for the memory
uint32_t lastActivity = 0;  // speech, transcript or backend progress: delays hanging up
uint32_t workingSince = 0;
uint32_t greetAt = 0;       // millis() of the greeting if nobody speaks until then, 0 = none
bool working = false;       // delegated work running
int slowCalls = 0;          // slow tools running in the registry's worker (results still due)
// The tools that ran with their results, for the memory: it records what the assistant did, not what it
// said he did (#71, as core Ended.Tools). Slow calls wait for their result by call id.
String toolLog;
std::vector<std::pair<String, String>> pendingCalls;
bool byWakeWord = false;

// The first words after the wake word (#106, voice/first_words): the device records them into
// `words` (PSRAM) and decides before GPT Live gets any audio. From the first voice on GPT Live
// connects ahead (`warm`): no audio, no greeting, no timers until commit or a silent close.
constexpr size_t WORDS_SAMPLES = size_t(audio::API_RATE) * FIRST_WORDS_MAX_MS / 1000;
constexpr size_t NO_UTTERANCE = SIZE_MAX;
bool listening = false;
bool warm = false;
bool wordsHeard = false;      // the device's transcription found words (no false start)
int16_t* words = nullptr;
size_t wordsLen = 0;
size_t firstVoice = NO_UTTERANCE;  // where the first voice began
size_t voiceEnd = 0;               // where the last voice ended
size_t utterance = NO_UTTERANCE;   // the current utterance's start (with 300 ms before it)
size_t utteranceVoiced = 0, quietRun = 0;  // samples
bool judging = false;
// After commit: the recording goes out first (paced), then the microphone.
const int16_t* preload = nullptr;
size_t preloadLen = 0;
String pendingNote;  // a quick command failed: GPT Live is told once it runs
float micCredit = 0;  // samples GPT Live takes at once (MIC_BURST_MS), refilled at 1.1x
uint32_t micCreditAt = 0;
void startSession(bool wakeWord, bool call, const String& number, const String& reason, bool forSomeoneElse,
                  const String& name = "", const String& who = "", bool insist = false, bool warmStart = false);

void logTool(const String& head, const String& output)
{
    if (toolLog.length() > 4000) return;
    String out = output.substring(0, 200);
    out.replace("\n", " ");
    toolLog += "- " + head + " -> " + out + "\n";
}

String toolHead(const char* name, const char* arguments) { return String(name) + " " + String(arguments).substring(0, 200); }

String withTools(const String& text)
{
    if (toolLog.length())
        return text + "\n(The tools the assistant's backend ran in this conversation, with their results; nothing else was done:\n" +
               toolLog + ")\n";
    return text + "\n(The assistant's backend ran no tool in this conversation.)\n";
}
// After the wake word the microphone records at once (start()), the LEDs show it, no cue plays;
// without words it ends silently (most false starts heard noise, the Echos 20.-28.10.2026).
// There is no answer clip ("Ja, Sir?") any more: it waited ACK_WAIT_MS for the user to go on
// talking, but the echo gate muted the microphone while it played, and a request after a breath
// was lost (05.10.2026, again 20.10.2026 with "Hey Jarvis … Licht an"). Silent users are greeted.
// Audio quality of the session, logged when it ends: hangs in the loop (device), gaps between
// GPT Live's audio events (network), the speaker running dry during speech (what one hears).
uint32_t lastUpdate = 0, maxLoopGap = 0, lastAudioEvent = 0, maxAudioGap = 0, underrunsAtStart = 0;
bool disconnectPending = false;
int usage = 0;
// What the assistant said since the user last spoke, and whether it delegated: an announced action
// without a delegation gets a reminder (nudgeIfClaimed; GPT Live said "Ich starte Eminem" and did
// nothing, several times on the Echo, 16.10.2026).
String assistantSaid;
uint32_t assistantSaidAt = 0;
bool delegatedTurn = false, nudgedTurn = false;
// What the backend model did in this session (delegations, end checks): logged when it ends and
// added to the month's totals in USAGE_FILE. The end check counts from the registry's worker.
struct Backend {
    int delegations, responses, endChecks;
    long input, cached, output;
};
Backend backend{};
portMUX_TYPE backendMux = portMUX_INITIALIZER_UNLOCKED;
constexpr const char* USAGE_FILE = "/usage.json";
bool phone = false;  // the conversation runs over a phone call (phone/rtp), not the Echo Pyramid
String caller, callReason;    // callReason: The assistant called (phone/ says when to greet)
String callerName;            // register or address book: he greets the caller by name
String person;                // who Jarvis talks with for sure (assistant/people id), "" = unknown
bool stranger = false;        // The assistant called someone else for the user: no memory, no tools
// The assistant speaks up at the speaker itself (start_conversation; callReason is the occasion): until
// the user answers he addresses them again instead of hanging up.
bool insisting = false;
int attempts = 0;
uint32_t repeatMs = SPEAKER_REPEAT_MS;  // silence before he tries again

// start_conversation runs in a worker: the main loop starts the session (update()).
portMUX_TYPE requestMux = portMUX_INITIALIZER_UNLOCKED;
String requestedReason, requestedPerson;
uint32_t requestedRepeatMs = SPEAKER_REPEAT_MS, requestedAt = 0;
bool requestedInsist = true;
volatile bool requested = false;
void startRequested();

// Which tools the backend is offered and may run: with a stranger on the line only end_call.
tools::Channel channel() { return stranger ? tools::Stranger : tools::Voice; }
// How this conversation came in, for sender lines of messages passed on.
String origin() { return !phone ? "Lautsprecher" : callReason.length() ? "Anruf von " ASSISTANT_NAME : "Anruf"; }
uint32_t endRequestedAt = 0;  // end_call: end once the assistant is quiet (GPT Live decides when)

// Notices for the model (notice()): queued from any task, sent by the main loop at a pause.
struct Notice {
    String text;
    bool urgent;
    uint32_t at;
};
std::vector<Notice> notices;
String privateOwner;  // whose private news this conversation may hear (privateFor), "" = nobody's
bool heardUser = false;        // the user said something in this conversation (someone is there)
bool closingOffered = false;   // the agenda was offered before hanging up (offerAgenda)
SemaphoreHandle_t noticeLock = xSemaphoreCreateMutex();  // notices and privateOwner (other tasks read them)

bool noticesPending()
{
    xSemaphoreTake(noticeLock, portMAX_DELAY);
    const bool pending = !notices.empty();
    xSemaphoreGive(noticeLock);
    return pending;
}

void dropNotices()
{
    xSemaphoreTake(noticeLock, portMAX_DELAY);
    for (const Notice& n : notices) Serial.printf("[Notice] conversation over, not told: %s\n", n.text.c_str());
    notices.clear();
    xSemaphoreGive(noticeLock);
}

// All pending notices in one commentary.
// `now`: the loop's timestamp. lastActivity must not get ahead of it (a later millis() made
// now - lastActivity wrap around and hung up at once).
void sendNotices(uint32_t now)
{
    String items;
    xSemaphoreTake(noticeLock, portMAX_DELAY);
    for (const Notice& n : notices) items += String("- ") + (n.urgent ? "(urgent) " : "") + n.text + "\n";
    notices.clear();
    xSemaphoreGive(noticeLock);
    if (items.isEmpty()) return;
    items.trim();
    String text = LIVE_NOTICE;
    text.replace("{items}", items);
    Serial.printf("\n[Notice] %s\n", items.c_str());
    sendCommentary(text.c_str());
    lastActivity = now;  // the model answers: the silence timers start again
}

bool is(const char* text, const char* expected) { return !strcmp(text, expected); }

// --- The conversation's audio: the Echo Pyramid or the phone line ---------------------

void setStreaming(bool on)
{
    if (phone) rtp::setStreaming(on);
    else audio::setStreaming(on);
}
size_t micAvailable() { return phone ? rtp::micAvailable() : audio::micAvailable(); }
size_t readMic(int16_t* dst, size_t samples) { return phone ? rtp::readMic(dst, samples) : audio::readMic(dst, samples); }
size_t queueSpeaker(const int16_t* src, size_t samples)
{
    return phone ? rtp::queueSpeaker(src, samples) : audio::queueSpeaker(src, samples);
}
size_t speakerQueued() { return phone ? rtp::speakerQueued() : audio::speakerQueued(); }
bool speaking() { return phone ? rtp::speaking() : audio::speaking(); }
void flushSpeaker()
{
    if (phone) rtp::flushSpeaker();
    else audio::flushSpeaker();
}
// Beeps at home only: on the phone the caller hears the ringing and the assistant's greeting instead.
void tone(int frequency, int ms)
{
    if (!phone) audio::queueTone(frequency, ms);
}

void setState(State next)
{
    current = next;
    stateSince = millis();
}

void setWorking()
{
    working = true;
    workingSince = lastActivity = millis();
}

void sendJson(const JsonDocument& doc)
{
    PsramText out(doc);  // session.start is over 64 KB: not a String
    if (!out.data) {
        Serial.println("\n[Warning] no memory for an event");
        return;
    }
    utf8::repair(out.data, out.size);  // invalid UTF-8 fails the event (tool results carry mail text)
    ws.sendTXT(out.data, out.size);
}

// --- Session lifecycle ------------------------------------------------------

// Hangs, stutter and delay the user heard: where they came from. Into the error log when
// noticeable, as the serial log is usually not recorded.
void reportAudio()
{
    const uint32_t underruns = phone ? 0 : audio::underruns() - underrunsAtStart;
    Serial.printf("\n[Audio] loop gap max %lu ms, audio event gap max %lu ms, speaker underruns %lu, WiFi %s\n",
                  (unsigned long)maxLoopGap, (unsigned long)maxAudioGap, (unsigned long)underruns,
                  network::link().c_str());
    if (underruns >= AUDIO_REPORT_UNDERRUNS || maxLoopGap >= AUDIO_REPORT_GAP_MS || maxAudioGap >= AUDIO_REPORT_GAP_MS)
        errorlog::add("Audio%s: %lu speaker underruns, loop gap max %lu ms, audio event gap max %lu ms, after %lu s, WiFi %s",
                      phone ? " (phone)" : "", (unsigned long)underruns, (unsigned long)maxLoopGap,
                      (unsigned long)maxAudioGap, (unsigned long)elapsedSeconds(), network::link().c_str());
}

// Ends the session locally. Often called inside a WebSocket callback, so the socket itself
// is closed later, in update().
void endLocally()
{
    // Also after a failure: what was said until then is worth remembering.
    if (current == State::Live || current == State::Closing) {
        transcript::flush();
        // A call to someone else: the memory must not take that person for the user.
        const String note = !phone && callReason.length()
                                ? "(The assistant spoke up at the speaker itself. Occasion: " + callReason + ")\n"
                            : !stranger ? String()
                            : callReason.isEmpty()
                                ? "(Call from " + caller + ", a number the user has not released; the assistant took a "
                                  "message. „Gegenüber“ is the caller, not the user.)\n"
                                : "(Phone call the assistant made on the user's behalf with " + caller + ". Task: " + callReason +
                                  " „Gegenüber“ is the person called, not the user.)\n";
        const char* channel = !phone                      ? memory::SPEAKER
                              : stranger && callReason.isEmpty() ? memory::GUEST
                              : stranger                        ? memory::CALL_FOR
                              : callReason.length()             ? memory::PHONE_OUT
                                                                : memory::PHONE_IN;
        // A member of a network hands it to the leader, which keeps memory and agenda.
        const String text = withTools(note + transcript::full());
        if (!shared::conversationOver(text, startedAt, stranger ? String() : person, channel, transcript::full())) {
            memory::consolidate(text, startedAt, stranger ? String() : person, channel);
            if (!stranger) agenda::conversationOver(transcript::full());
        }
    }
    if (current == State::Live || current == State::Closing) reportAudio();
    dropHold();
    dropNotices();
    setStreaming(false);
    disconnectPending = true;
    setState(State::Idle);
}

// A session connected ahead (warm) goes away unheard: no tone, the first words go on recording.
void dropWarm(const char* why)
{
    if (!warm) return;
    warm = false;
    Serial.printf("\n[Live] connected ahead, dropped: %s\n", why);
    if (current == State::Starting || current == State::Live) ws.sendTXT("{\"type\":\"session.close\"}");
    disconnectPending = true;
    setState(State::Idle);
    if (!listening) setStreaming(false);
}

void fail(const String& message)
{
    if (warm) {  // the decision starts a session of its own if it needs one
        dropWarm(message.c_str());
        return;
    }
    transcript::flush();
    // TLS lives in internal RAM: the WebSocket fails when other connections use it up.
    Serial.printf("\n[Error] %s (internal free %u, largest block %u, after %lu s)\n", message.c_str(),
                  unsigned(heap_caps_get_free_size(MALLOC_CAP_INTERNAL)),
                  unsigned(heap_caps_get_largest_free_block(MALLOC_CAP_INTERNAL)), elapsedSeconds());
    status::showError(message, 15000);
    errorlog::add("Session failed%s: %s (state %d, internal free %u, block %u, after %lu s, WiFi %s)",
                  phone ? (stranger ? " (call on the user's behalf)" : " (phone)") : "", message.c_str(), int(current),
                  unsigned(heap_caps_get_free_size(MALLOC_CAP_INTERNAL)),
                  unsigned(heap_caps_get_largest_free_block(MALLOC_CAP_INTERNAL)), elapsedSeconds(),
                  network::link().c_str());
    if (!open()) return;
    flushSpeaker();
    tone(220, 300);
    endLocally();
}

// Adds the session to this month's totals: {"month": "2026-10", "sessions": 3, "seconds": 410,
// "delegations": 5, ...}. One flash write per session.
void addToMonth(const Backend& used)
{
    if (!timeutil::synced()) return;
    const String month = timeutil::format(time(nullptr), "%Y-%m");
    JsonDocument totals;
    File file = LittleFS.exists(USAGE_FILE) ? LittleFS.open(USAGE_FILE, "r") : File();
    if (!file || deserializeJson(totals, file)) totals.clear();
    file.close();
    if (month != (totals["month"] | "")) {
        totals.clear();
        totals["month"] = month;
    }
    totals["sessions"] = (totals["sessions"] | 0) + 1;
    totals["seconds"] = (totals["seconds"] | 0) + usage;
    const char* channel = phone ? "phone" : "speaker";  // where the minutes go
    totals["by_channel"][channel]["sessions"] = (totals["by_channel"][channel]["sessions"] | 0) + 1;
    totals["by_channel"][channel]["seconds"] = (totals["by_channel"][channel]["seconds"] | 0) + usage;
    totals["delegations"] = (totals["delegations"] | 0) + used.delegations;
    totals["responses"] = (totals["responses"] | 0) + used.responses;
    totals["end_checks"] = (totals["end_checks"] | 0) + used.endChecks;
    totals["input_tokens"] = (totals["input_tokens"] | 0L) + used.input;
    totals["cached_tokens"] = (totals["cached_tokens"] | 0L) + used.cached;
    totals["output_tokens"] = (totals["output_tokens"] | 0L) + used.output;
    if (!fileutil::saveJson(USAGE_FILE, totals)) Serial.println("[Usage] file not written");
}

void finish()
{
    if (warm) {
        dropWarm("closed");
        return;
    }
    transcript::flush();
    endLocally();
    tone(440, 150);
    portENTER_CRITICAL(&backendMux);
    const Backend used = backend;
    backend = Backend{};
    portEXIT_CRITICAL(&backendMux);
    Serial.printf("\n[Session ended] %d s; backend: %d delegations, %d responses, %d end checks, "
                  "%ld tokens in (%ld cached), %ld out\n",
                  usage, used.delegations, used.responses, used.endChecks, used.input, used.cached, used.output);
    addToMonth(used);
}

void sendSessionStart()
{
    JsonDocument doc(PsramJson::instance());
    doc["type"] = "session.start";
    JsonObject session = doc["session"].to<JsonObject>();
    session["model"] = LIVE_MODEL;
    // The memory notes go to both models: GPT Live talks with them, the backend needs the
    // fact ids for forget and the facts for its tools (addresses, preferences).
    // A member of a network of devices: the leader's (assistant/shared, fetched beforehand).
    const String notes = shared::notes(false, person), notesWithIds = shared::notes(true, person);
    // The admin's calendar only for them or nobody named at the speaker (not in calls for someone
    // else), mail deadlines for their owner. Asked in every session: it also forgets what an
    // earlier one was given.
    const String agendaText = MEMBER ? String() : agenda::context(stranger ? String("-") : person, !phone && !stranger, true);
    const String planned = shared::planned(tasks::context() + (stranger ? String() : agendaText));
    String call;
    if (phone && callReason.length()) call = String(LIVE_PHONE_HINT) + LIVE_PHONE_CALLING + " " + callReason + "\n";
    else if (phone) call = String(LIVE_PHONE_HINT) + "- Caller: " + caller + ".\n";
    else if (callReason.length())
        call = String(LIVE_SPEAKER_CALLING) + (insisting ? LIVE_SPEAKER_INSISTING : "") + "- Occasion: " + callReason + "\n";
    // The household's behaviour rules right after the instructions they refine (binding, not data).
    session["instructions"] = prompt::persona(person) + String(LIVE_INSTRUCTIONS) + "\n" + mcp::summary() + shared::rules(person, false) +
                              call + prompt::context() + "\n" + notes + planned;
    if (stranger) {
        // Someone else is on the line: only the task, none of the user's notes, no tools but hanging up.
        const String name = memory::profileValue("name");
        const String owner = name.length() ? name : String("your owner");
        session["instructions"] =
            callReason.isEmpty()  // an unknown caller
                ? String(LIVE_GUEST_INSTRUCTIONS) + "\nYour owner: " + owner + ". Caller: " + caller +
                      ".\n" + prompt::context()
                : String(LIVE_CALL_INSTRUCTIONS) + "\nYou are calling on behalf of " + owner +
                      ". Number called: " + caller + ".\nTask: " + callReason + "\n" + prompt::context();
    }
    session["audio"]["format"]["type"] = "audio/pcm";
    session["audio"]["format"]["rate"] = audio::API_RATE;
    session["audio"]["output"]["voice"] = LIVE_VOICE;
    JsonObject delegation = session["delegation"].to<JsonObject>();
    if (DELEGATION_MODEL) {
        delegation["type"] = "responses";
        JsonObject responses = delegation["responses"].to<JsonObject>();
        responses["model"] = DELEGATION_MODEL;
        // The time last: it changes every minute, and OpenAI's prompt cache only covers what is the
        // same from the start (cached input costs a tenth).
        responses["instructions"] = String(DELEGATION_INSTRUCTIONS) + "\n" + prompt::persona(person) +
                                    shared::rules(person, true) + notesWithIds + planned + prompt::context();
        if (stranger) responses["instructions"] = DELEGATION_CALL_INSTRUCTIONS;
        tools::describe(responses["tools"].to<JsonArray>(), channel(), person);
        // Each result goes back with response.create right away: with parallel calls the
        // backend continued while others were unanswered ("Missing tool call").
        responses["parallel_tool_calls"] = false;
        // Without these the backend thought for 8-9 s per response: "Welches Lied läuft gerade?"
        // took ~18 s on the Echo, ~6 s with them (16.10.2026). GPT Live phrases the result itself.
        responses["reasoning"]["effort"] = DELEGATION_EFFORT;
        responses["text"]["verbosity"] = "low";
    } else {
        delegation["type"] = "client";
    }
    sendJson(doc);
}

// --- Audio --------------------------------------------------------------------

// GPT Live streams 100 ms packets in real time, silence included, and carries no timing
// fields over WebSocket. Activity is therefore tracked via transcripts.
void onAudioDelta(const char* base64)
{
#ifdef LOG_PACING
    Serial.printf("[PACE] local %lu samples %u queued %u\n", millis() - sessionSince,
                  unsigned(strlen(base64) * 3 / 8), unsigned(audio::speakerQueued()));
#endif
    size_t bytes = 0;
    if (mbedtls_base64_decode(decodeBuffer, DECODE_BYTES, &bytes,
                              reinterpret_cast<const uint8_t*>(base64), strlen(base64)) != 0) {
        Serial.println("\n[Warning] audio delta not decodable");
        return;
    }
    const size_t samples = bytes / 2;
    const int16_t* pcm = reinterpret_cast<int16_t*>(decodeBuffer);
    if (held) {
        if (heldSamples + samples <= HELD_AUDIO_SAMPLES) {
            memcpy(heldAudio + heldSamples, pcm, samples * sizeof(int16_t));
            heldSamples += samples;
            return;
        }
        releaseHold("held audio full");  // then this packet plays after it
    }
    // GPT Live paces its audio in real time, so audio held up once (the loop blocked, e.g. at
    // the first conversation after a start) would lag behind the transcript for the whole
    // conversation. It streams silence too: while the backlog is too long, silent packets are
    // dropped, and the voice catches up in the next pause without losing a word.
    const size_t queued = speakerQueued();
    if (queued > size_t(audio::API_RATE) * SPEAKER_LAG_MS / 1000) {
        int peak = 0;
        for (size_t i = 0; i < samples; ++i) peak = max(peak, abs(int(pcm[i])));
        static uint32_t lastReport = 0;
        if (millis() - lastReport > 2000) {
            lastReport = millis();
            Serial.printf("\n[Audio] speaker %u ms behind%s\n", unsigned(queued * 1000 / audio::API_RATE),
                          peak < SILENCE_PEAK ? ", dropping silence" : "");
        }
        if (peak < SILENCE_PEAK) return;
    }
    if (queueSpeaker(pcm, samples) < samples) Serial.println("\n[Warning] speaker buffer full");
}

void sendMicrophone()
{
    // {"type":"session.input_audio.append","audio":"<base64>"}
    static const char prefix[] = "{\"type\":\"session.input_audio.append\",\"audio\":\"";
    static char message[sizeof(prefix) + (MIC_CHUNK * 2 + 2) / 3 * 4 + 4];
    int16_t samples[MIC_CHUNK];
    // GPT Live takes at most 1.2x real time in bursts of up to 5 s, else it drops the excess (the
    // Echo, 28.10.2026): a credit of MIC_BURST_MS, refilled at 1.1x.
    const uint32_t now = millis();
    const float burst = float(audio::API_RATE) * MIC_BURST_MS / 1000;
    micCredit = micCreditAt ? min(burst, micCredit + (now - micCreditAt) * audio::API_RATE * 1.1f / 1000) : burst;
    micCreditAt = now;
    for (int chunks = 0; chunks < MIC_CHUNKS_PER_PASS && micCredit >= MIC_CHUNK; ++chunks) {
        if (preload && preloadLen >= MIC_CHUNK) {  // the first words before the microphone
            memcpy(samples, preload, sizeof(samples));
            preload += MIC_CHUNK;
            preloadLen -= MIC_CHUNK;
        } else {
            preload = nullptr;
            if (micAvailable() < MIC_CHUNK) break;
            readMic(samples, MIC_CHUNK);
        }
        micCredit -= MIC_CHUNK;
        size_t length = 0;
        memcpy(message, prefix, sizeof(prefix) - 1);
        mbedtls_base64_encode(reinterpret_cast<uint8_t*>(message) + sizeof(prefix) - 1,
                              sizeof(message) - sizeof(prefix), &length,
                              reinterpret_cast<uint8_t*>(samples), sizeof(samples));
        length += sizeof(prefix) - 1;
        message[length++] = '"';
        message[length++] = '}';
        if (!ws.sendTXT(message, length)) return;
    }
}

// --- Tools --------------------------------------------------------------------

// The result goes back as a function_call_output item, then response.create lets the
// backend continue.
void sendResult(const String& callId, const String& output)
{
    JsonDocument reply;
    reply["type"] = "response.item.create";
    reply["item"]["type"] = "function_call_output";
    reply["item"]["call_id"] = callId;
    reply["item"]["output"] = output;
    sendJson(reply);
    JsonDocument next;
    next["type"] = "response.create";
    sendJson(next);
    setWorking();  // the backend continues with the result
}

void onFunctionCall(JsonObjectConst call)
{
    const char* name = call["name"] | "";
    const char* arguments = call["arguments"] | "{}";
    const char* callId = call["call_id"] | "";
    if (held) {
        Serial.printf("\n[Hold] %s waits for the decision\n", name);
        heldCalls.push_back({name, arguments, callId});
        return;
    }
    String output;
    if (tools::isSlow(name)) {
        // TLS / IMAP take seconds: the registry's worker answers later (main loop -> sendToolOutput).
        Serial.printf("\n[Tool] %s %.200s -> running\n", name, arguments);
        if (tools::start(name, arguments, callId, channel(), output, person, origin())) {
            slowCalls++;
            if (pendingCalls.size() < 20) pendingCalls.emplace_back(String(callId), toolHead(name, arguments));
            setWorking();
            return;
        }
    } else {
        // Blocks the loop (Home Assistant: HTTP in the LAN, ~0.1-1 s); the microphone buffers.
        const uint32_t started = millis();
        output = tools::run(name, arguments, channel(), person, origin());
        if (millis() - started >= AUDIO_REPORT_GAP_MS)
            errorlog::add("Tool %s blocked the conversation %lu ms", name, (unsigned long)(millis() - started));
    }
    Serial.printf("\n[Tool] %s %s -> %.200s\n", name, arguments, output.c_str());
    logTool(toolHead(name, arguments), output);
    sendResult(callId, output);
}

// --- The hold ---------------------------------------------------------------------

void dropHold()
{
    held = false;
    heldSamples = 0;
    heldCalls.clear();
}

// What GPT Live said meanwhile plays (without the silence it streamed before its answer), its
// calls run.
void releaseHold(const char* why)
{
    if (!held) return;
    held = false;
    Serial.printf("\n[Hold] released (%s): %u ms audio, %u calls\n", why,
                  unsigned(heldSamples * 1000 / audio::API_RATE), unsigned(heldCalls.size()));
    constexpr size_t BLOCK = audio::API_RATE / 10;
    size_t from = 0;
    for (; from < heldSamples; from += BLOCK) {
        int peak = 0;
        for (size_t i = from; i < min(heldSamples, from + BLOCK); ++i) peak = max(peak, abs(int(heldAudio[i])));
        if (peak >= SILENCE_PEAK) break;
    }
    if (from < heldSamples) queueSpeaker(heldAudio + from, heldSamples - from);
    heldSamples = 0;
    std::vector<HeldCall> calls;
    calls.swap(heldCalls);
    for (const HeldCall& c : calls) {
        JsonDocument call;
        call["name"] = c.name;
        call["arguments"] = c.arguments;
        call["call_id"] = c.callId;
        onFunctionCall(call.as<JsonObjectConst>());
    }
}

// The first utterance, once GPT Live's transcript of it rested and the sentence is whole: a
// home command runs here and the conversation ends; anything else releases the hold.
void judgeFirstUtterance(uint32_t now)
{
    if (!held) return;
    if (now - heldSince > QUICK_HOLD_MAX_MS) {
        releaseHold("deadline");
        return;
    }
    if (firstUser.isEmpty() || now - firstUserAt < QUICK_SILENCE_MS) return;
    String text = firstUser;
    text.trim();
    // only a whole sentence: GPT Live's transcript mostly ends with a full stop, else it settled
    const bool whole = text.endsWith(".") || text.endsWith("!") || text.endsWith("?");
    if (!whole && now - firstUserAt < QUICK_SETTLED_MS) return;
    if (!quick_home::candidate(text)) {
        releaseHold("not a command");
        return;
    }
    String detail;
    const quick_home::Result result = quick_home::run(text, detail);  // HA in the LAN: ~0.1-0.5 s
    if (result == quick_home::Result::Done) {
        Serial.printf("[Quick] \"%s\" done on the device before GPT Live, conversation ended\n", text.c_str());
        dropHold();
        flushSpeaker();
        tone(880, 80);       // the light may be in another room: done is heard
        transcript::clear();  // a switched light is nothing to remember
        close();
        return;
    }
    if (result == quick_home::Result::Failed) {
        releaseHold("the quick command failed");
        const String note = "System note, not from the user: the device tried to do the user's request itself and "
                            "it failed: " + detail + " Tell the user in one short sentence what went wrong and help "
                            "if you can. Do not say you did it.";
        sendCommentary(note.c_str());
        return;
    }
    releaseHold("Home Assistant did not understand");
}

// --- Server events --------------------------------------------------------------

void onSessionStarted(JsonDocument& doc, size_t length)
{
    // The echo of the configuration grows with the memory (limit: scripts/patch_libraries.py).
    Serial.printf("\n[Session] %s (%u bytes)\n", (const char*)(doc["session"]["id"] | "?"), unsigned(length));
    setState(State::Live);
    lastActivity = millis();
    // On the phone the assistant answers right away, like anyone picking up.
    greetAt = lastActivity + (phone ? PHONE_GREETING_DELAY_MS : GREETING_DELAY_MS);
    if (phone && callReason.length()) greetAt = 0;  // The assistant called: it speaks once the user picks up
    // At the speaker after the button: the first utterance may be a quick command (the hold).
    // After the wake word the device decided that already (voice/first_words).
    dropHold();
    held = !phone && callReason.isEmpty() && heldAudio && !byWakeWord;
    heldSince = lastActivity;
    firstUser = "";
    firstUserAt = 0;
    if (!byWakeWord) tone(880, 120);
    setStreaming(true);  // already on after the wake word: the first words are recorded
    if (warm) Serial.println("[Live] connected ahead, waiting for the decision");
}

void onDelegation(JsonObjectConst delegation)
{
    greetAt = 0;
    delegatedTurn = true;
    setWorking();
    portENTER_CRITICAL(&backendMux);
    backend.delegations++;
    portEXIT_CRITICAL(&backendMux);
    const char* target = delegation["target"] | "";
    Serial.printf("\n[Delegation] %s\n", target);
    if (!is(target, "client")) return;
    // No local handler for client delegation: answer so the model does not wait forever.
    JsonDocument reply;
    reply["type"] = "session.commentary.append";
    reply["delegation_id"] = delegation["id"];
    reply["content"] = "Dafuer ist auf diesem Geraet noch keine Funktion verfuegbar.";
    sendJson(reply);
}

// Progress of the hosted Responses backend (web search, tool calls).
void onBackendEvent(JsonObjectConst event)
{
    lastActivity = millis();
    const char* type = event["type"] | "";
    const bool finished = is(type, "response.completed") || is(type, "response.failed") ||
                          is(type, "response.incomplete") || is(type, "response.cancelled");
    if (finished || is(type, "response.created") || is(type, "response.web_search_call.searching"))
        Serial.printf("[Backend] %s (%lu s)\n", type, (millis() - workingSince) / 1000);
    if (finished) countBackend(event["response"]["usage"], false);
    JsonObjectConst item = event["item"];
    if (is(type, "response.output_item.done") && is(item["type"] | "", "function_call"))
        onFunctionCall(item);
    // The backend is done; the silence timer starts now, while the assistant speaks the result. Not while
    // a slow tool still runs: after his "Einen Moment" the result is still due (a 31 s fire_tv call
    // was dropped when the conversation ended at 10 s of silence).
    else if (finished)
        working = slowCalls > 0;
}

void onApiError(const String& message)
{
    if (current == State::Starting) {
        fail(message);  // startup errors prevent session.started
        return;
    }
    Serial.printf("\n[API error] %s\n", message.c_str());
    status::showError(message, 15000);
}

void onEvent(const char* payload, size_t length)
{
    // In PSRAM: every event, and session.started (the whole configuration, >30 KB) in many small
    // blocks, fragmented the internal RAM so that a TLS handshake during the conversation failed.
    JsonDocument doc(PsramJson::instance());
    // session.started echoes the tool schemas (Home Assistant's are deeper than the default 10).
    const DeserializationError error =
        deserializeJson(doc, payload, length, DeserializationOption::NestingLimit(32));
    if (error) {
        Serial.printf("\n[Warning] invalid JSON (%s, %u bytes): %.120s\n", error.c_str(),
                      unsigned(length), payload);
        return;
    }
    const char* type = doc["type"] | "";
    if (is(type, "session.output_audio.delta")) {
        // Gaps count only within audible speech: between turns GPT Live may pause on purpose.
        const uint32_t now = millis();
        if (lastAudioEvent) maxAudioGap = max(maxAudioGap, now - lastAudioEvent);
        lastAudioEvent = speaking() ? now : 0;
        if (current == State::Live || current == State::Closing) onAudioDelta(doc["delta"] | "");
    } else if (is(type, "session.output_transcript.delta")) {
        // Not the end of delegated work: Jarvis often says "Einen Moment" while it runs.
        lastActivity = millis();
        greetAt = 0;
        if (assistantSaid.length() < 600) assistantSaid += doc["delta"] | "";
        assistantSaidAt = lastActivity;
        transcript::append(transcript::Speaker::Assistant, doc["delta"] | "");
    } else if (is(type, "session.input_transcript.delta")) {
        lastActivity = millis();
        greetAt = 0;
        assistantSaid = "";
        delegatedTurn = nudgedTurn = false;
        if (insisting) Serial.printf("\n[Speaker] the user answered after %d attempts\n", attempts + 1);
        insisting = false;
        heardUser = true;
        transcript::append(transcript::Speaker::User, doc["delta"] | "");
        if (held) {
            firstUser += doc["delta"] | "";
            firstUserAt = lastActivity;
        }
    } else if (is(type, "session.started")) {
        onSessionStarted(doc, length);
    } else if (is(type, "session.delegation.created")) {
        onDelegation(doc["delegation"]);
    } else if (is(type, "response.event")) {
        onBackendEvent(doc["event"]);
    } else if (is(type, "session.usage.updated")) {
        usage = doc["usage"]["seconds"] | usage;
    } else if (is(type, "session.closed")) {
        usage = doc["usage"]["seconds"] | usage;
        Serial.printf("\n[Closed] %s\n", (const char*)(doc["reason"] | "?"));
        finish();
    } else if (is(type, "error")) {
        onApiError(doc["error"]["message"] | "Unbekannter Fehler");
    }
}

void onWebSocket(WStype_t type, uint8_t* payload, size_t length)
{
    switch (type) {
        case WStype_CONNECTED:
            setState(State::Starting);
            sendSessionStart();
            break;
        case WStype_TEXT:
            onEvent(reinterpret_cast<const char*>(payload), length);
            break;
        case WStype_DISCONNECTED:
            if (current == State::Closing) finish();
            else if (open())
                fail(length ? String(reinterpret_cast<const char*>(payload), length)
                            : String("Verbindung getrennt"));
            break;
        default:
            break;
    }
    // Diagnostics for unexpected frames (e.g. fragmented messages this code does not reassemble).
    if (type != WStype_TEXT && type != WStype_PING && type != WStype_PONG) {
        Serial.printf("[WS] type %d, %u bytes: %.*s\n", int(type), unsigned(length),
                      int(min(length, size_t(300))), type == WStype_BIN || !payload ? "" : (const char*)payload);
    }
}

// --- Notices ----------------------------------------------------------------------

// Pending news goes to the model at a pause: not while the assistant speaks or the backend works, not
// while the user's answer is still due, not before the greeting. Urgent news does not wait for
// the quiet; anything waits at most NOTICE_MAX_WAIT_MS.
void deliverNotices(uint32_t now)
{
    bool urgent = false;
    uint32_t oldest = now;
    xSemaphoreTake(noticeLock, portMAX_DELAY);
    for (const Notice& n : notices) {
        urgent |= n.urgent;
        if (int32_t(n.at - oldest) < 0) oldest = n.at;
    }
    const bool pending = !notices.empty();
    xSemaphoreGive(noticeLock);
    if (!pending || insisting) return;
    const bool silent = !speaking() && !working && !greetAt;
    const bool quiet = silent && now - lastActivity > NOTICE_QUIET_MS && transcript::lastSpeaker() != transcript::Speaker::User;
    if (quiet || (urgent && silent) || now - oldest > NOTICE_MAX_WAIT_MS) sendNotices(now);
}

// Before hanging up (the end check said "over", or silence): agenda entries that matter now and
// have not come up, once per conversation and only if someone talked. The model decides whether
// one is worth a sentence. True if it got them (hanging up waits for its answer).
bool offerAgenda(uint32_t now)
{
    // A member: the agenda is the leader's, and asking it would block the loop (not yet, #66).
    if (closingOffered || !heardUser || stranger || MEMBER) return false;
    closingOffered = true;
    transcript::flush();
    const String items = agenda::closing(transcript::full());
    if (items.isEmpty()) return false;
    String text = LIVE_CLOSING_NOTICE;
    text.replace("{items}", items);
    Serial.printf("\n[Agenda] before hanging up: %s\n", items.c_str());
    sendCommentary(text.c_str());
    lastActivity = now;
    return true;
}

// --- Hanging up -------------------------------------------------------------------

// The user woke the assistant but says nothing (yet): The assistant greets instead of waiting in silence.
void greetIfSilent(uint32_t now)
{
    if (!greetAt || int32_t(now - greetAt) < 0) return;
    // After the wake word GPT Live only runs once the device heard words (voice/first_words): it
    // answers them, no greeting (and the silent end below would count from the connection ahead).
    if (byWakeWord && wordsHeard) {
        greetAt = 0;
        return;
    }
    // The microphone heard the user, but no transcript came yet: what was said during the
    // connection still goes through GPT Live's turn detection. A greeting now would talk over it.
    if (!phone && audio::voiceMs() >= VOICE_MIN_MS &&
        (now - audio::lastVoiceAt() < GREETING_AFTER_SPEECH_MS || now - stateSince < GREETING_AFTER_SPEECH_MS))
        return;
    // Woken by the wake word and nobody said a word: most likely the TV or talk in the room. Alexa
    // ends those silently; a greeting into the empty room made every false start heard and cost
    // ~20 s of GPT Live (the Echo, 22.10.2026, #78). The button still gets its greeting.
    if (byWakeWord && !heardUser && !phone && callReason.isEmpty()) {
        if (now - stateSince < WAKE_SILENT_END_MS) return;  // the user may still be thinking
        greetAt = 0;
        Serial.println("\n[Wake word] nobody spoke: ended silently");
        close();
        return;
    }
    greetAt = 0;
    Serial.println("\n[Greeting] the user is silent");
    releaseHold("the user is silent");  // the greeting must be heard
    if (phone && callerName.length()) {  // someone from the address book called
        String hint = LIVE_NAMED_GREETING;
        hint.replace("{name}", callerName);
        sendCommentary(hint.c_str());
        return;
    }
    sendCommentary(callReason.length() && !phone ? LIVE_SPEAKER_GREETING
                   : !phone  ? LIVE_GREETING
                   : stranger ? LIVE_GUEST_GREETING
                              : LIVE_PHONE_GREETING);
}

// The assistant spoke up itself and nobody answered yet: no hanging up, it tries again after a pause
// (a sleeper does not answer the first time), until SPEAKER_INSIST_MS.
void insist(uint32_t now)
{
    endRequestedAt = 0;  // not before the user answered
    if (now - sessionSince > SPEAKER_INSIST_MS) {
        Serial.printf("[Hang up] nobody answered at the speaker after %d attempts\n", attempts + 1);
        errorlog::add("Speaker: nobody answered after %d attempts", attempts + 1);
        close();
        return;
    }
    if (greetAt || speaking() || working || now - lastActivity < repeatMs) return;
    ++attempts;
    lastActivity = now;
    Serial.printf("\n[Speaker] no answer, attempt %d\n", attempts + 1);
    String hint = LIVE_SPEAKER_AGAIN;
    hint.replace("{n}", String(attempts + 1));
    sendCommentary(hint.c_str());
}

// claimed: Jarvis says it is doing something ("Ich starte Eminem", "Wird gemacht").
bool claimed(String said)
{
    said.toLowerCase();
    static const char* const PHRASES[] = {"ich starte", "ich spiele", "ich schalte", "ich mache",  "ich sende",
                                          "ich schicke", "ich stelle", "ich pausiere", "ich lege",  "ich setze",
                                          "ich trage",  "ich rufe",   "ich drucke",   "ich öffne", "ich schließe",
                                          "ich lösche", "ich kümmere", "ich erledige", "wird gemacht", "wird erledigt"};
    for (const char* phrase : PHRASES)
        if (said.indexOf(phrase) >= 0) return true;
    return false;
}

// nudgeIfClaimed: GPT Live announced an action and then did not delegate it. Once he is quiet for
// 3 s without a delegation since the user spoke, a system note tells him to do it (once per turn).
void nudgeIfClaimed(uint32_t now)
{
    if (nudgedTurn || delegatedTurn || working || !assistantSaidAt || now - assistantSaidAt < 3000 || speaking()) return;
    if (!claimed(assistantSaid)) return;
    nudgedTurn = true;
    Serial.printf("\n[Nudge] announced without delegating: %s\n", assistantSaid.substring(0, 120).c_str());
    sendCommentary(LIVE_DELEGATE_NUDGE);
    lastActivity = now;
}

// Silence = nothing audible, no transcript and no delegated work running.
void hangUpWhenQuiet(uint32_t now)
{
    // The transcript runs ahead of the audio: count the silence from the end of the
    // audible answer, not from the last transcript fragment.
    if (speaking()) lastActivity = now;
    if (insisting) {
        insist(now);
        return;
    }
    const bool busy = speaking() || (working && now - workingSince < THINKING_TIMEOUT_MS);
    // After the user spoke an answer is still due; only after the assistant spoke is it over.
    const bool userSpokeLast = transcript::lastSpeaker() == transcript::Speaker::User;
    // News still pending: the model hears it before the conversation may end.
    if (!speaking() && noticesPending() &&
        (endRequestedAt ? now - endRequestedAt > 800 : !working && now - lastActivity > END_CHECK_QUIET_MS)) {
        endRequestedAt = 0;
        sendNotices(now);
        return;
    }
    // end_call: GPT Live decided the conversation is over.
    // Not waiting for more words: after end_call GPT Live often narrates the tool's result.
    if (endRequestedAt && now - endRequestedAt > 800 && !speaking()) {
        Serial.println("[Hang up] the assistant ends the conversation");
        close();
        return;
    }
    // Is the conversation over? Asked once per quiet moment after the assistant spoke (voice/conversation_end).
    // A "no" is asked once more if it stays quiet: one answer of the model can be wrong.
    static uint32_t checked = 0, checkedAt = 0;
    static int checks = 0;
    if (checked != lastActivity) checks = 0;
    if (!working && !speaking() && !userSpokeLast && now - lastActivity > END_CHECK_QUIET_MS && checks < 2 &&
        (checks == 0 || now - checkedAt > END_CHECK_QUIET_MS + 3000)) {
        checked = lastActivity;
        checkedAt = now;
        ++checks;
        transcript::flush();  // the last line reaches full() only after a pause
        conversation_end::check(transcript::full(), lastActivity);
    }
    if (conversation_end::over(lastActivity) && !speaking()) {
        if (offerAgenda(now)) return;
        Serial.println("[Hang up] the conversation is over");
        close();
        return;
    }
    // On the phone a pause is normal (the caller thinks, looks something up): wait longer.
    const uint32_t limit = phone ? PHONE_IDLE_TIMEOUT_MS : userSpokeLast ? ANSWER_TIMEOUT_MS : IDLE_TIMEOUT_MS;
    if (!busy && now - lastActivity > limit) {
        if (!userSpokeLast && offerAgenda(now)) return;
        transcript::flush();
        Serial.printf("[Silence] %lu s without speech, last spoke %s - ending the session\n",
                      (unsigned long)(now - lastActivity) / 1000, userSpokeLast ? "the user" : ASSISTANT_NAME "");
        // Otherwise it looks like the assistant just stopped: say why.
        if (working) status::showError("Backend nach " + String((now - workingSince) / 1000) + " s ohne Ergebnis", 10000);
        close();
    } else if (now - stateSince > MAX_SESSION_MS && (!busy || now - stateSince > MAX_SESSION_MS + THINKING_TIMEOUT_MS)) {
        // Not in the middle of an answer or a delegated task, unless that takes too long.
        Serial.println("\n[Maximum duration reached]");
        status::showError("Maximale Gespraechsdauer erreicht", 10000);
        close();
    }
}

void disconnectIfPending()
{
    if (!disconnectPending) return;
    disconnectPending = false;
    ws.disconnect();
}

// --- The first words (#106) -------------------------------------------------------

// Ends the first words without GPT Live; words=false: a start nobody spoke to (HA: wake_word_silent,
// services/ha_device).
void stopListening(const char* why, bool words)
{
    if (!listening) return;
    listening = false;
    wordsHeard = wordsHeard || words;
    first_words::cancel();
    first_words::release();
    judging = false;
    Serial.printf("\n[Listen] ended: %s\n", why);
    dropWarm(why);
    if (current == State::Idle) setStreaming(false);
}

// GPT Live takes over with the recording from 300 ms before the first voice to 1 s after the last
// (the silence around it would only delay the answer); with a note instead when a quick command
// failed (the recording would make it answer the words as well).
void commitLive(const char* why, const String& note = "")
{
    listening = false;
    first_words::cancel();
    judging = false;
    pendingNote = note;
    preload = nullptr;
    preloadLen = 0;
    if (note.isEmpty() && firstVoice != NO_UTTERANCE) {
        const size_t from = firstVoice > audio::API_RATE * 3 / 10 ? firstVoice - audio::API_RATE * 3 / 10 : 0;
        const size_t to = min(wordsLen, voiceEnd + audio::API_RATE);
        preload = words + from;
        preloadLen = to - from;
    }
    Serial.printf("\n[Listen] GPT Live takes over (%s): %.1f s of audio\n", why, float(preloadLen) / audio::API_RATE);
    if (warm) {
        warm = false;
        lastActivity = millis();
        if (current == State::Live) greetAt = lastActivity + GREETING_DELAY_MS;
        return;
    }
    startSession(true, false, "", "", false, "", "", false, true);  // not connected ahead (or it failed)
}

void actOn(first_words::Result& r)
{
    using first_words::Verdict;
    switch (r.verdict) {
        case Verdict::NoWords:
            return;  // a scrap: listen on (the quiet ends it)
        case Verdict::NotMeant:
            stopListening(("\"" + r.text + "\" not meant for the assistant").c_str(), false);
            return;
        case Verdict::Done:
            Serial.printf("[Quick] \"%s\" done on the device, no GPT Live\n", r.text.c_str());
            stopListening("quick command", true);
            tone(880, 80);  // the light may be in another room: done is heard
            return;
        case Verdict::Spoken:
            Serial.printf("[Quick] \"%s\" answered on the device, no GPT Live\n", r.text.c_str());
            stopListening("quick command", true);
            if (queueSpeaker(r.speech, r.speechSamples) < r.speechSamples) Serial.println("[Warning] speaker buffer full");
            free(r.speech);
            r.speech = nullptr;
            return;
        case Verdict::Failed:
            wordsHeard = true;
            commitLive("the quick command failed",
                       "System note, not from the user: the user said \"" + r.text + "\". The device tried to do it "
                       "itself and it failed: " + r.detail + " Tell the user in one short sentence what went wrong "
                       "and help if you can. Do not say you did it.");
            return;
        case Verdict::Live:
            wordsHeard = true;
            commitLive("not a quick command");
            return;
    }
}

constexpr size_t BLOCK = audio::API_RATE / 50;  // 20 ms
constexpr size_t PAUSE = size_t(audio::API_RATE) * FIRST_WORDS_PAUSE_MS / 1000;
constexpr size_t CUT = size_t(audio::API_RATE) * FIRST_WORDS_CUT_MS / 1000;
constexpr size_t QUIET = size_t(audio::API_RATE) * FIRST_WORDS_QUIET_MS / 1000;
constexpr size_t MIN_VOICE = size_t(audio::API_RATE) * FIRST_WORDS_MIN_VOICE_MS / 1000;

// The 20 ms block just written at words + wordsLen: voice, the utterance, GPT Live connecting ahead.
void hearBlock()
{
    constexpr size_t LEAD = audio::API_RATE * 3 / 10;
    const int16_t* block = words + wordsLen;
    int peak = 0;
    for (size_t i = 0; i < BLOCK; ++i) peak = max(peak, abs(int(block[i])));
    const size_t at = wordsLen;
    wordsLen += BLOCK;
    if (peak >= VOICE_PEAK) {
        if (firstVoice == NO_UTTERANCE) {
            firstVoice = at;
            // Someone speaks: GPT Live connects ahead, without audio, while the device decides
            // (~3 s it would otherwise add to every question).
            warm = true;
            startSession(true, false, "", "", false, "", "", false, true);
        }
        if (utterance == NO_UTTERANCE) utterance = at > LEAD ? at - LEAD : 0;
        utteranceVoiced += BLOCK;
        voiceEnd = wordsLen;
        quietRun = 0;
    } else {
        quietRun += BLOCK;
    }
    if (utterance != NO_UTTERANCE && quietRun >= PAUSE && utteranceVoiced < MIN_VOICE) {
        utterance = NO_UTTERANCE;  // a click or a knock
        utteranceVoiced = 0;
    }
}

// Every loop pass while listening: the microphone into `words`, the utterance's end, the verdict.
bool playTest();

void listenStep()
{
    while (!playTest() && micAvailable() >= BLOCK && wordsLen + BLOCK <= WORDS_SAMPLES) {
        readMic(words + wordsLen, BLOCK);
        hearBlock();
    }
    // The utterance ended (or runs too long): to the transcription, one at a time; the device
    // listens on meanwhile (a scrap may come back empty, and the words after it must not be lost).
    const bool full = wordsLen + BLOCK > WORDS_SAMPLES;
    if (utterance != NO_UTTERANCE && !judging &&
        (quietRun >= PAUSE || wordsLen - utterance >= CUT || full)) {
        if (first_words::judge(words + utterance, wordsLen - utterance)) {
            judging = true;
            utterance = NO_UTTERANCE;
            utteranceVoiced = 0;
        }
    }
    first_words::Result r;
    if (judging && first_words::take(r)) {
        judging = false;
        actOn(r);
        if (!listening) return;
    }
    if (!judging && utterance == NO_UTTERANCE && (wordsLen - voiceEnd >= QUIET || full))
        stopListening(firstVoice == NO_UTTERANCE ? "no voice after the wake word" : "no words after the wake word", false);
}

// The serial test's words (testFirstWords), handed over from the worker.
portMUX_TYPE testMux = portMUX_INITIALIZER_UNLOCKED;
int16_t* testPcm = nullptr;
size_t testSamples = 0;
void beginListening();

// The test running: its words in real time instead of the microphone, then 1.5 s of silence (the
// pause that ends them), then the microphone again.
int16_t* playing = nullptr;
size_t playingSamples = 0, playingPos = 0;
uint32_t playingSince = 0;

// In the main loop: a new test starts as if the wake word was heard.
void runTest()
{
    portENTER_CRITICAL(&testMux);
    int16_t* pcm = testPcm;
    const size_t samples = testSamples;
    testPcm = nullptr;
    portEXIT_CRITICAL(&testMux);
    if (!pcm) return;
    if (open() || !words) {
        Serial.println("[First words test] busy");
        free(pcm);
        return;
    }
    beginListening();
    free(playing);
    playing = pcm;
    playingSamples = samples + audio::API_RATE * 3 / 2;
    playingPos = 0;
    playingSince = millis();
}

// While a test plays: its blocks that are due, the microphone's dropped. True while it plays.
bool playTest()
{
    if (!playing) return false;
    const size_t due = size_t(millis() - playingSince) * audio::API_RATE / 1000;
    while (playingPos + BLOCK <= min(due, playingSamples) && wordsLen + BLOCK <= WORDS_SAMPLES) {
        int16_t* block = words + wordsLen;
        for (size_t j = 0; j < BLOCK; ++j)
            block[j] = playingPos + j < playingSamples - audio::API_RATE * 3 / 2 ? playing[playingPos + j] : 0;
        playingPos += BLOCK;
        hearBlock();
    }
    int16_t discard[BLOCK];
    while (micAvailable() >= BLOCK) readMic(discard, BLOCK);
    if (playingPos + BLOCK <= playingSamples && listening) return true;
    free(playing);
    playing = nullptr;
    return false;
}

// After the wake word: the first words before anything else (start()).
void beginListening()
{
    phone = false;
    callReason = "";
    person = "";
    stranger = false;
    byWakeWord = true;
    heardUser = wordsHeard = false;
    listening = true;
    warm = false;
    wordsLen = voiceEnd = utteranceVoiced = quietRun = 0;
    firstVoice = utterance = NO_UTTERANCE;
    judging = false;
    preload = nullptr;
    preloadLen = 0;
    pendingNote = "";
    first_words::cancel();
    first_words::prepare();
    setStreaming(true);
    Serial.println("\n[Listen] after the wake word");
}

}  // namespace

bool begin()
{
    decodeBuffer = static_cast<uint8_t*>(
        heap_caps_malloc(DECODE_BYTES, MALLOC_CAP_SPIRAM | MALLOC_CAP_8BIT));
    // without it there is no hold: every conversation answers at once, as before
    heldAudio = static_cast<int16_t*>(heap_caps_malloc(HELD_AUDIO_SAMPLES * sizeof(int16_t), MALLOC_CAP_SPIRAM));
    // without it the wake word connects GPT Live at once, as before #106
    words = static_cast<int16_t*>(heap_caps_malloc(WORDS_SAMPLES * sizeof(int16_t), MALLOC_CAP_SPIRAM));
    authHeader = String("Authorization: Bearer ") + OPENAI_API_KEY;
    ws.onEvent(onWebSocket);
    return decodeBuffer;
}

namespace {
// Which part of the session's loop step hung (main.cpp only sees "live"): into the error log.
template <typename Part>
void timedPart(const char* name, Part part)
{
    const bool talking = current == State::Live;
    const uint32_t started = millis();
    part();
    const uint32_t took = millis() - started;
    if (talking && took >= AUDIO_REPORT_GAP_MS) errorlog::add("Live: %s blocked %lu ms", name, (unsigned long)took);
}
}  // namespace

void update()
{
    timedPart("transcript", [] { transcript::update(); });
    acknowledge::update();
    disconnectIfPending();
    startRequested();
    runTest();
    if (listening) timedPart("first words", [] { listenStep(); });
    if (current == State::Idle) return;
    // Another device may answer this wake word (system/peer): its handshake would block the loop
    // and with it the decision. The 250 ms vanish in the connection time.
    if (byWakeWord && peer::deciding()) return;
    timedPart("websocket", [] { ws.loop(); });
    if (current == State::Idle) return;  // a callback ended it
    // ws.loop() can block for seconds (TLS handshake): take a fresh timestamp.
    const uint32_t now = millis();
    if (current == State::Live && lastUpdate) maxLoopGap = max(maxLoopGap, now - lastUpdate);
    lastUpdate = current == State::Live ? now : 0;
    const uint32_t age = now - stateSince;
    switch (current) {
        case State::Connecting:
        case State::Starting:
            if (age > CONNECT_TIMEOUT_MS)
                fail(current == State::Connecting ? "Verbindung fehlgeschlagen" : "Keine Antwort");
            break;
        case State::Live:
            if (warm) break;  // connected ahead: no audio, nothing speaks up or ends it
            if (pendingNote.length()) {
                sendCommentary(pendingNote.c_str());
                pendingNote = "";
            }
            timedPart("microphone", [] { sendMicrophone(); });
            timedPart("quick", [now] { judgeFirstUtterance(now); });
            if (current != State::Live) break;  // a quick command closed it
            greetIfSilent(now);
            nudgeIfClaimed(now);
            timedPart("notices", [now] { deliverNotices(now); });
            if (current == State::Live) timedPart("hang-up", [now] { hangUpWhenQuiet(now); });
            break;
        case State::Closing:
            if (age > CLOSE_TIMEOUT_MS) finish();  // final usage unconfirmed
            break;
        case State::Idle:
            break;
    }
}

namespace {

// warmStart: after the wake word, from the first words (listening; connected ahead or committed).
void startSession(bool wakeWord, bool call, const String& number, const String& reason, bool forSomeoneElse,
                  const String& name, const String& who, bool insist, bool warmStart)
{
    if (warmStart ? current != State::Idle : open()) return;
    phone = call;
    person = forSomeoneElse ? String() : who;
    caller = name.length() ? name + " (" + number + ")" : number;
    callerName = name;
    callReason = reason;
    stranger = forSomeoneElse;
    insisting = insist;
    attempts = 0;
    endRequestedAt = 0;
    dropNotices();  // queued for a conversation that ended meanwhile
    heardUser = closingOffered = false;
    {
        // The person on the line; nobody named at the speaker: the admin's (the house is trusted).
        const String owner = stranger ? String() : person.length() ? person : !phone ? people::admin() : String();
        xSemaphoreTake(noticeLock, portMAX_DELAY);
        privateOwner = owner;
        xSemaphoreGive(noticeLock);
    }
    conversation_end::reset();
    transcript::setUserName(stranger ? "Gegenüber" : "Du");
    if (!network::connected()) {
        fail("Noch kein WLAN - bitte warten");
        return;
    }
    disconnectIfPending();  // the previous session may have failed in this loop pass
    byWakeWord = wakeWord;
    if (wakeWord) setStreaming(true);  // the first words are recorded (listenStep)
    micCreditAt = 0;
    maxLoopGap = maxAudioGap = lastUpdate = lastAudioEvent = 0;
    underrunsAtStart = audio::underruns();
    transcript::clear();
    startedAt = time(nullptr);
    usage = 0;
    portENTER_CRITICAL(&backendMux);
    backend = Backend{};
    portEXIT_CRITICAL(&backendMux);
    working = false;
    slowCalls = 0;
    toolLog = "";
    pendingCalls.clear();
    greetAt = 0;
    assistantSaid = "";
    assistantSaidAt = 0;
    delegatedTurn = nudgedTurn = false;
    status::clearError();
    ws.beginSslWithCA(LIVE_HOST, 443, LIVE_PATH, CA_CERTS, "");
    ws.setExtraHeaders(authHeader.c_str());
    ws.setReconnectInterval(CONNECT_TIMEOUT_MS);  // at most one attempt per session
    ws.enableHeartbeat(15000, 5000, 2);
    setState(State::Connecting);
    sessionSince = stateSince;
    Serial.println(phone ? "\n[Connecting] phone call" : "\n[Connecting]");
}

// A conversation the assistant was asked to start at the speaker: once nothing else runs there. One
// that does not insist (a greeting) is dropped if it cannot start within SPEAKER_START_WINDOW_MS:
// late it would be wrong.
void startRequested()
{
    if (!requested) return;
    taskENTER_CRITICAL(&requestMux);
    const String reason = requestedReason, who = requestedPerson;
    const uint32_t repeat = requestedRepeatMs, since = requestedAt;
    const bool insist = requestedInsist;
    taskEXIT_CRITICAL(&requestMux);
    if (!insist && millis() - since > SPEAKER_START_WINDOW_MS) {
        requested = false;
        Serial.printf("\n[Speaker] too late, dropped: %s\n", reason.c_str());
        return;
    }
    if (mute::on()) {  // muted after it was asked for
        requested = false;
        Serial.printf("\n[Speaker] microphone off, dropped: %s\n", reason.c_str());
        return;
    }
    if (open() || !network::connected() || announcer::active()) return;
    requested = false;
    if (insist) Serial.printf("\n[Speaker] the assistant speaks up (again after %lu s of silence): %s\n", repeat / 1000, reason.c_str());
    else Serial.printf("\n[Speaker] the assistant speaks up once: %s\n", reason.c_str());
    startSession(false, false, "", reason, false, people::name(who), who, insist);
    repeatMs = repeat;
}

}  // namespace

String requestConversation(const String& task, const String& person, bool insist, int repeatSeconds)
{
    if (task.length() < 10) return "Error: describe the occasion completely (why the assistant speaks up, what it wants).";
    if (mute::on())
        return "Error: the user switched off the microphone of this speaker, so no conversation can start here; "
               "an announcement (speak) still plays.";
    if (!insist && open()) return "Error: a conversation is already running at the speaker.";
    const uint32_t repeat = repeatSeconds > 0 ? constrain(repeatSeconds, 3, 300) * 1000UL : SPEAKER_REPEAT_MS;
    taskENTER_CRITICAL(&requestMux);
    requestedReason = task;
    requestedPerson = person;
    requestedRepeatMs = repeat;
    requestedInsist = insist;
    requestedAt = millis();
    taskEXIT_CRITICAL(&requestMux);
    requested = true;
    const String who = person.length() ? people::name(person) : String("whoever is there");
    if (!insist) return "The assistant speaks up at the speaker in a moment and talks with " + who + "; if nobody answers, he ends it.";
    return "The assistant speaks up at the speaker " + String(open() ? "once the current conversation is over" : "in a moment") +
           " and addresses " + who + " again after " + String(repeat / 1000) + " s of silence until they answer (at most " +
           String(SPEAKER_INSIST_MS / 60000) + " minutes).";
}

void addTools()
{
    tools::add(tools::Tool{
        "start_conversation",
        "The assistant starts a voice conversation at the speaker at home itself and speaks first. insist (default "
        "true): while nobody answers, he addresses them again after a few seconds of silence (repeat_seconds) "
        "for up to 10 minutes, for waking someone by voice ('weck mich hier über den Lautsprecher'); insist "
        "false: he speaks up once and ends the conversation if nobody answers. Not for one-way announcements "
        "(speak, alexa: nobody could answer) or calls (phone_call). At a later time ('weck mich um 7 hier'): "
        "create_task. Returns right away; the conversation starts once no other one runs at the speaker.",
        R"json({"type": "object", "additionalProperties": false,
            "properties": {"to": {"type": "string", "description": "a person of the household by name ('' = the user)"},
                           "task": {"type": "string", "description": "in German: why the assistant speaks up and what it wants, e.g. 'Wecke Max, es ist 6:30. Bleib dran, bis er sagt, dass er wach ist.'"},
                           "insist": {"type": "boolean", "description": "optional: keep at it until they answer (default true)"},
                           "repeat_seconds": {"type": "integer", "minimum": 3, "maximum": 300, "description": "optional: seconds of silence before he speaks up again, only if the user asked for it (default 5)"}},
            "required": ["to", "task"]})json",
        [](JsonObjectConst arguments, const tools::Context& context) -> String {
            String to = arguments["to"] | "";
            to.trim();
            const String person = to.isEmpty() ? (context.person.length() ? context.person : people::admin())
                                               : people::byName(to);
            if (person.isEmpty()) return "Error: " + to + " is not a person of the household.";
            return requestConversation(arguments["task"] | "", person, arguments["insist"] | true,
                                       arguments["repeat_seconds"] | 0);
        },
        0, tools::Agent | tools::Console});
}

void start(bool wakeWord)
{
    if (!wakeWord || !words) {  // the button (or no PSRAM for the first words): GPT Live at once
        startSession(wakeWord, false, "", "", false);
        return;
    }
    if (open()) return;
    beginListening();
}

void testFirstWords(int16_t* pcm, size_t samples)
{
    portENTER_CRITICAL(&testMux);
    int16_t* old = testPcm;
    testPcm = pcm;
    testSamples = samples;
    portEXIT_CRITICAL(&testMux);
    free(old);
}

void startCall(const String& number, const String& reason, bool forSomeoneElse, const String& name,
               const String& who)
{
    startSession(false, true, number, reason, forSomeoneElse, name, who);
}

void endSoon() { endRequestedAt = millis(); }

void abandon()
{
    if (!open() || !byWakeWord) return;
    if (listening) {
        stopListening("another device answers the wake word", true);
        return;
    }
    Serial.println("\n[Session] dropped: another device answers the wake word");
    transcript::clear();  // nothing to remember: the other device has the conversation
    dropHold();
    flushSpeaker();
    setStreaming(false);
    if (current == State::Starting || current == State::Live) ws.sendTXT("{\"type\":\"session.close\"}");
    dropNotices();
    disconnectPending = true;
    setState(State::Idle);
}

void close()
{
    if (listening) {
        stopListening("closed", true);
        return;
    }
    if (current == State::Connecting) {
        finish();
        return;
    }
    if (current != State::Starting && current != State::Live) return;
    setStreaming(false);
    ws.sendTXT("{\"type\":\"session.close\"}");
    setState(State::Closing);
}

void toggle()
{
    if (open()) close();
    else start();
}

bool sendToolOutput(const String& callId, const String& output)
{
    if (current != State::Live) return false;
    if (slowCalls > 0) slowCalls--;
    for (auto it = pendingCalls.begin(); it != pendingCalls.end(); ++it) {
        if (it->first == callId) {
            logTool(it->second, output);
            pendingCalls.erase(it);
            break;
        }
    }
    sendResult(callId, output);
    return true;
}

void sendCommentary(const char* text)
{
    if (current != State::Live) return;
    JsonDocument doc;
    doc["type"] = "session.commentary.append";
    doc["delegation_id"] = nullptr;
    doc["content"] = text;
    sendJson(doc);
}

bool notice(const String& text, bool urgent)
{
    if (current != State::Live || text.isEmpty()) return false;
    xSemaphoreTake(noticeLock, portMAX_DELAY);
    notices.push_back({text, urgent, millis()});
    xSemaphoreGive(noticeLock);
    return true;
}

bool privateFor(const String& owner)
{
    if (!open() || owner.isEmpty()) return false;
    xSemaphoreTake(noticeLock, portMAX_DELAY);
    const bool same = owner == privateOwner;
    xSemaphoreGive(noticeLock);
    return same;
}

State state() { return listening ? State::Listening : current; }
bool open() { return listening || current != State::Idle; }
bool startedByWakeWord() { return byWakeWord; }
bool unconfirmedWake() { return byWakeWord && !heardUser && !wordsHeard && !phone && callReason.isEmpty(); }
bool onPhone() { return open() && phone; }
bool thinking() { return working; }
int usageSeconds() { return usage; }

void countBackend(JsonObjectConst used, bool endCheck)
{
    portENTER_CRITICAL(&backendMux);
    if (endCheck) backend.endChecks++;
    else backend.responses++;
    backend.input += used["input_tokens"] | 0L;
    backend.cached += used["input_tokens_details"]["cached_tokens"] | 0L;
    backend.output += used["output_tokens"] | 0L;
    portEXIT_CRITICAL(&backendMux);
}

void printUsage()
{
    File file = LittleFS.exists(USAGE_FILE) ? LittleFS.open(USAGE_FILE, "r") : File();
    Serial.printf("\n[Usage] this month: %s\n", file ? file.readString().c_str() : "nothing counted yet");
}
uint32_t elapsedSeconds() { return (millis() - sessionSince) / 1000; }

}  // namespace live
