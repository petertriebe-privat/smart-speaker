#pragma once

#include <Arduino.h>
#include <ArduinoJson.h>

// A conversation with OpenAI GPT Live over a WebSocket. The session is only open during a
// conversation (GPT Live bills every open second) and hangs up by itself after silence.
// Delegated work runs in the hosted Responses backend, which calls the device's tools.
//
//   Idle -> Connecting -> Starting -> Live -> Closing -> Idle
//           (TLS)         (session.start sent, waiting for session.started)
namespace live {

// Listening: after the wake word the device takes the first words itself and decides before GPT
// Live gets any audio (voice/first_words); GPT Live may already connect, unseen.
enum class State { Idle, Listening, Connecting, Starting, Live, Closing };

bool begin();   // false if out of memory
void addTools();  // start_conversation: The assistant speaks up at the speaker itself (a wake-up)
void update();  // every loop: WebSocket, microphone, hang-up timers

void start(bool byWakeWord = false);
// Tests (serial ~), from any task: pcm (24 kHz, PSRAM, taken over and freed) as the first words
// after the wake word, then the microphone; the main loop starts it (update()).
void testFirstWords(int16_t* pcm, size_t samples);
// A conversation over a phone call (phone/): its audio runs over RTP instead of the Pyramid.
// `reason`: The assistant called the user itself (it waits with its greeting until phone/ says the
// user picked up); "" = the user called. `forSomeoneElse`: The assistant calls someone else for the
// user (phone_call), or a guest calls: the session gets no memory and no tools but end_call.
// `name`: the caller's name (register or address book), "" if unknown. `person`: who is on the
// line for sure (assistant/people id, by the number), "" if not a person of the household.
void startCall(const String& caller, const String& reason = "", bool forSomeoneElse = false,
               const String& name = "", const String& person = "");
// The assistant speaks up at the speaker itself (start_conversation, Home Assistant's
// jarvis_conversation); `task` is the occasion, `person` an assistant/people id or "" (whoever is
// in the room). insist: until someone answers he addresses them again after repeatSeconds of
// silence (0 = SPEAKER_REPEAT_MS), a wake-up; otherwise he speaks up once (a greeting), only if no
// conversation runs and within SPEAKER_START_WINDOW_MS. Any task; returns a result for the model.
String requestConversation(const String& task, const String& person, bool insist, int repeatSeconds = 0);
void endSoon();  // hang up once the assistant finished speaking (end_call)
// Another device answers the wake word (system/peer): ends a conversation the wake word started,
// silently, before it said anything, without remembering it.
void abandon();
void close();   // asks the server to end the session
void toggle();  // screen button: start or close

// Result of a mail tool that finished in the mail task. False if the session is over.
bool sendToolOutput(const String& callId, const String& output);
void sendCommentary(const char* text);  // tells the model something (serial test 'c')
// News for the user during a conversation (a reminder that is due, an appointment coming close,
// a new mail, a finished job): goes to the model at the next pause (not while the assistant speaks or
// the backend works, at the latest after NOTICE_MAX_WAIT_MS), which picks the moment to say it.
// A pending notice delays hanging up. From any task. False if no conversation is live.
bool notice(const String& text, bool urgent);
// Whether the open conversation may hear private news of `owner` (people id: their mail, the
// admin's calendar): it is with them, or with nobody named at the speaker and `owner` is the
// admin. Never in a call the assistant makes for someone else.
bool privateFor(const String& owner);

State state();
bool open();  // Connecting .. Closing
bool startedByWakeWord();
// The wake word opened the conversation (the last one, once closed) and GPT Live heard no words:
// likely a false start (HA: wake_word_silent).
bool unconfirmedWake();
bool onPhone();  // the open conversation is a phone call
bool thinking();                // the backend works on a delegated task
int usageSeconds();             // billed seconds of the current or last session
uint32_t elapsedSeconds();      // of the current session
// One finished backend response of the conversation (`used`: the Responses "usage" object);
// endCheck: voice/conversation_end's verdict, else delegated work. From any task.
void countBackend(JsonObjectConst used, bool endCheck);
void printUsage();  // this month's sessions, delegations and backend tokens (serial U)

}  // namespace live
