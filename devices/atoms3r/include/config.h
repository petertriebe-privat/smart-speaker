#pragma once

#include <stdint.h>
#include <stddef.h>

#include "secrets.h"
#include "assistant_name.h"  // ASSISTANT_NAME (shared/prompts, #29)

// Settings of the smart speaker. The model instructions are in shared/prompts/prompts.h; WiFi,
// keys, passwords and everything about your household (names, addresses, hosts) in secrets.h
// (copy secrets.h.example). The household defaults below only keep a build without them working.

// --- Role in the household ----------------------------------------------------------
// Standalone (no PEER_KEY): this device is the household's assistant and does everything itself.
// Member (PEER_KEY set, the same on every device): a room unit in a network of devices (the
// Echos, devices/echo/docs/18-several-echos.md, #66). Conversations, the wake word, Home Assistant and
// everything outgoing run here; memory, people and tasks are the leader's (an Echo), asked over
// the peer link (system/peer, assistant/shared); without a leader a local emergency memory that
// goes to the leader when it is back. Never the leader itself.
#ifndef PEER_KEY
#define PEER_KEY ""
#endif
constexpr bool MEMBER = sizeof(PEER_KEY) > 16;  // at least 16 characters, as on the Echo
#ifndef DEVICE_NAME
#define DEVICE_NAME ""  // in the network and for the leader's prompt ("" = "M5")
#endif
#ifndef DEVICE_ROOM
#define DEVICE_ROOM ""  // the room it stands in (as the Home Assistant area is named)
#endif
constexpr uint16_t PEER_PORT = 47800;  // UDP; the signed calls on PEER_PORT + 1 (as the Echo)
// The microphone's RMS here times this = comparable with an Echo's (beamformed, echo-cancelled):
// the wake word goes to the louder device. Calibrated side by side with an Echo, the speaker
// about as far from both (19.10.2026): the Echo had 0.36-0.52 of the M5's level, mean 0.42. With
// 0.42 the M5 won every time (its own level is the loudest second of 3.5 s, above the one second
// measured for the calibration): 0.3, so that an Echo answers unless the M5 is clearly nearer.
// The Echo is the better partner for a conversation (it hears the user while it speaks).
constexpr float PEER_LEVEL_SCALE = 0.3f;

// Nothing incoming on this device: no phone line, no Telegram/WhatsApp/mail polling, no Home
// Assistant events, no scheduled tasks, no backups and no Google login renewal. Two devices doing
// those would answer twice and overwrite each other. A member never does them; JARVIS_INBOUND 0
// in secrets.h makes a standalone device a second speaker without the network.
#ifndef JARVIS_INBOUND
#define JARVIS_INBOUND 1
#endif
constexpr bool INBOUND = JARVIS_INBOUND && !MEMBER;

// --- Household (set in secrets.h) -------------------------------------------------
#ifndef OWNER_NAME
#define OWNER_NAME "Admin"  // the admin (assistant/people), with PHONE_OWNER, TELEGRAM_OWNER_ID
#endif
#ifndef OWNER_EMAIL
#define OWNER_EMAIL ""      // "schick mir ...": the admin's own address
#endif
#ifndef PRINTER_HOST
#define PRINTER_HOST ""     // IPP printer in the LAN, e.g. "192.168.1.50"; "" = no printing
#endif
#ifndef PRINTER_EMAIL
#define PRINTER_EMAIL ""    // e-mail print service of the printer (PDFs, images); "" = none
#endif
#ifndef MAIL_SMTP_HOST
#define MAIL_SMTP_HOST ""   // The assistant's own mailbox (MAIL_ADDRESS / MAIL_PASSWORD)
#endif
#ifndef MAIL_IMAP_HOST
#define MAIL_IMAP_HOST ""
#endif
#ifndef MAIL_SENT_FOLDER
#define MAIL_SENT_FOLDER "Sent"  // the provider's folder for sent mail
#endif
#ifndef MAIL_AUTHSERV_ID
#define MAIL_AUTHSERV_ID ""  // the provider's Authentication-Results name, e.g. "example.com" (mail commands)
#endif
#ifndef WATCHED_MAILBOXES
// Private mailboxes the assistant watches for their owners (assistant/mail_watch), comma separated:
// {person, label, host, user, secret}. person: name or id in the register, "" = the admin;
// label: how the assistant names the mailbox; host: IMAP host (port and CA as the assistant's own mailbox) or
// "gmail" (Gmail API); secret: the IMAP password, for Gmail the account's refresh token (""
// = GOOGLE_REFRESH_TOKEN, the admin's).
#define WATCHED_MAILBOXES
#endif
#ifndef JARVIS_PHONE_NUMBER
#define JARVIS_PHONE_NUMBER ""  // the number people call to reach the assistant (welcome mail); "" = not mentioned
#endif
#ifndef SIP_REGISTRAR
#define SIP_REGISTRAR "192.168.178.1"  // the phone system (a FritzBox's default address)
#endif

// --- GPT Live (voice conversation) ----------------------------------------
constexpr const char* LIVE_HOST = "api.openai.com";
constexpr const char* LIVE_PATH = "/v1/live/sessions";
constexpr const char* LIVE_MODEL = "gpt-live-1";
constexpr const char* LIVE_VOICE = "cedar";
// The assistant's cheekiness by default (shared/prompts/persona.h): 0 polite, 1 dry, 2 cheeky, 3 biting. Per
// person: people register (set_humor); children are talked to child-friendly.
constexpr int PERSONA_LEVEL = 2;  // also accepted: marin (default), meridian, ...

// Backend model for delegated work (hosted Responses delegation with web search and the
// device's own tools); also runs the mail agent. nullptr = client delegation without tools.
constexpr const char* DELEGATION_MODEL = "gpt-6-luna";
// The Decisions API's model (quick yes/no and choice questions, e.g. whether a conversation is
// over: voice/conversation_end; the Echo's quick commands as QUICK_INTENT_MODEL).
constexpr const char* DECISIONS_MODEL = "gpt-6-luna";
// Its reasoning in a conversation: "low" answers in seconds; the default thought 8-9 s per
// response (measured on the Echo, 16.10.2026).
constexpr const char* DELEGATION_EFFORT = "low";

// Told to the model at session start together with the current time. Empty = unknown.
constexpr const char* LOCATION = "";
constexpr const char* TIMEZONE = "CET-1CEST,M3.5.0,M10.5.0/3";
constexpr const char* CALENDAR_TIMEZONE = "Europe/Berlin";
constexpr uint32_t GOOGLE_LOGIN_REMIND_MS = 12 * 3600 * 1000;  // login link again while still expired
constexpr uint32_t GOOGLE_LOGIN_LINK_S = 7 * 24 * 3600;          // a link's state is replaced after that
constexpr uint32_t GOOGLE_DIRECTORY_MS = 6 * 3600 * 1000;  // contacts' numbers for greeting callers by name  // the same zone for Google Calendar (IANA name)

// --- Agenda: the admin's appointments and due to-dos until tomorrow night (assistant/agenda) ---
constexpr uint32_t AGENDA_REFRESH_MS = 20 * 60 * 1000;  // fetched again between conversations
constexpr uint32_t AGENDA_RETRY_MS = 5 * 60 * 1000;     // after a failed fetch
constexpr size_t AGENDA_MAX_LINES = 16;                 // in a prompt; the rest as "... and n more"
constexpr size_t AGENDA_LINE_CHARS = 160;
constexpr int AGENDA_SOON_MIN = 60;  // an appointment coming this close during a conversation: a notice
constexpr int AGENDA_URGENT_MIN = 15;  // ... marked urgent
// Before hanging up, entries not mentioned yet are offered once per day: today's always,
// tomorrow's from this hour on.
constexpr int AGENDA_EVENING_HOUR = 18;

// --- Notices during a conversation (live::notice) ---------------------------------
constexpr uint32_t NOTICE_QUIET_MS = 1000;      // quiet before a notice goes to the model (END_CHECK_QUIET_MS is later)
constexpr uint32_t NOTICE_MAX_WAIT_MS = 45000;  // then it goes anyway

// --- Main loop ------------------------------------------------------------------
// Stack of the background tasks (tools worker, Telegram, memory, scheduled tasks; internal RAM).
// 24 KB until 29.09.; measured with serial H: ~6 KB used with TLS, mail and agent runs (TLS lives
// in PSRAM). The Telegram task running next to the others took the internal RAM to 2.7 KB and
// the WiFi hung.
constexpr uint32_t TASK_STACK_BYTES = 14 * 1024;
constexpr size_t ERROR_LOG_BYTES = 16 * 1024;      // /errors.log (system/error_log), newer half kept
constexpr uint32_t LOOP_SLOW_MS = 1000;             // a loop step taking longer is logged

// --- WiFi -------------------------------------------------------------------
constexpr uint32_t WIFI_RETRY_MS = 20000;           // new connection attempt while offline
constexpr uint32_t WIFI_RESTART_MS = 180000;        // restart when still offline after this
constexpr uint32_t NETWORK_PROBE_MS = 60000;        // between conversations: can packets get through?
constexpr uint32_t NETWORK_PROBE_TIMEOUT_MS = 2000;
constexpr int ROAM_BELOW_DBM = -70;                 // between conversations, weaker: look for a better AP
constexpr int ROAM_GAIN_DB = 10;                    // switch only to an AP at least this much stronger
constexpr uint32_t ROAM_SCAN_MS = 120000;           // between such scans

// --- Session / cost control ----------------------------------------------
// GPT Live bills every second the session is open, including silence.
// Silence = no audible playback, no transcript, no delegated work running.
constexpr uint32_t IDLE_TIMEOUT_MS = 10000;         // after the assistant spoke last (from the end of its audio)
constexpr uint32_t GREETING_DELAY_MS = 1500;        // nobody spoke after session.started: The assistant greets
// The microphone heard the user but GPT Live has no transcript yet (the speech buffered during
// the connection is still in its turn detection): the greeting waits this long after the voice.
constexpr uint32_t GREETING_AFTER_SPEECH_MS = 5000;
// After the wake word nobody said a word: no greeting, the session ends silently this long after
// session.started (~7 s after the word; a false start, #78). Alexa listens ~8 s.
constexpr uint32_t WAKE_SILENT_END_MS = 4000;
// After the button (the Echo): this long to hear whether the user talks already; quiet -> "Ja, Sir?".
constexpr uint32_t ACK_WAIT_MS = 500;
// After the wake word: no "Ja, Sir?" (both devices). Any wait was too short: the clip fell into the
// breath after "Jarvis" and muted the command ("Hey Jarvis … Licht an" lost, 20.10.2026). No cue
// either since 28.10.2026, only the LEDs and display show it; without words it ends silently.
constexpr uint32_t ANSWER_TIMEOUT_MS = 15000;       // after the user spoke last: answer still due
// Quiet after the assistant spoke: is the conversation over? 1500 plus a 4 s check made the hang-up
// slow (19.10.2026); with the Decisions API the check takes ~0.4 s.
constexpr uint32_t END_CHECK_QUIET_MS = 800;
// Safety cap for delegated work: a web search with reasoning plus a mail can take over 60 s.
constexpr uint32_t THINKING_TIMEOUT_MS = 180000;
constexpr uint32_t MAX_SESSION_MS = 20 * 60 * 1000;  // not mid-task (then up to + THINKING_TIMEOUT_MS)
constexpr uint32_t CONNECT_TIMEOUT_MS = 15000;
// The assistant speaks up at the speaker itself (start_conversation, e.g. waking the user): until they
// answer he addresses them again after this much silence (default of the tool's repeat_seconds),
// for at most SPEAKER_INSIST_MS.
constexpr uint32_t SPEAKER_REPEAT_MS = 5000;
constexpr uint32_t SPEAKER_INSIST_MS = 10 * 60 * 1000;
constexpr uint32_t SPEAKER_START_WINDOW_MS = 60000;  // a greeting not started by then is dropped

// --- Wake word -------------------------------------------------------------
// microWakeWord model "Hey Jarvis" (include/wake_model.h, from esphome/micro-wake-word-models).
// JARVIS_WAKE_WORD 0 in secrets.h: only the button starts a conversation (a second speaker in the
// room of another assistant listening for the same word).
#ifndef JARVIS_WAKE_WORD
#define JARVIS_WAKE_WORD 1
#endif
constexpr bool WAKE_WORD_ENABLED = JARVIS_WAKE_WORD;
// Model default 0.97; higher = fewer false triggers. 0.85 needed many tries (19.10.2026, as the
// Echo's 0.65): 0.6, at the cost of more false starts.
constexpr float WAKE_PROBABILITY_CUTOFF = 0.6f;
constexpr int WAKE_SLIDING_WINDOW = 5;              // inferences averaged (30 ms each)

// --- Audio ----------------------------------------------------------------
constexpr int DEFAULT_VOLUME = 65;
constexpr uint32_t AMPLIFIER_CHECK_MS = 5000;      // is the Pyramid's amplifier still set up?
constexpr uint32_t VOLUME_DISPLAY_MS = 1500;        // LED level bar after a volume change
constexpr uint32_t MUTE_HOLD_MS = 2000;             // holding the button this long: microphone off / on (ui/mute)
constexpr bool LED_FIRST_AT_BOTTOM = true;          // flip if the bars grow downwards
constexpr uint32_t JITTER_MS = 250;                 // playback buffer against network jitter (150: gaps of 254 ms ran dry)

// Until there is echo cancellation: GPT Live gets silence while the assistant is audible (plus
// ECHO_TAIL_MS), so it cannot hear itself. Costs voice interruption during answers.
constexpr bool ECHO_GATE = true;
constexpr uint8_t DISPLAY_ROTATION = 2;  // 0..3 in 90 degree steps; 2: USB cable at the back
constexpr uint32_t ECHO_TAIL_MS = 300;              // "speaking" lasts this long after audio
constexpr uint32_t SPEAKER_LAG_MS = 500;            // more audio waiting than this: silent packets are dropped
constexpr int SILENCE_PEAK = 300;                   // |sample| below this in a whole packet: silence
// Local voice detection at the microphone (room at rest: level 0-1 %, peaks below ~330).
constexpr int VOICE_PEAK = 1000;                    // ~ -30 dBFS in a 10 ms block: someone speaks
constexpr uint32_t VOICE_MIN_MS = 100;              // this much of it: speech, not a click
// A session's audio goes into the error log from this many speaker underruns during speech, or
// this long a hang of the loop or between GPT Live's audio events while the assistant speaks.
constexpr uint32_t AUDIO_REPORT_UNDERRUNS = 3;
constexpr uint32_t AUDIO_REPORT_GAP_MS = 500;

// NLMS echo canceller (audio/aec.cpp). Off: the Pyramid has no speaker loopback on any ES7210
// channel (see docs/architecture.md). With the constant MIC2 as "reference" the filter acts as a ~1 kHz
// high-pass and removes most of the voice. Needs a digital reference before it can be used.
constexpr bool ECHO_CANCELLER = false;
// Freeze echo canceller adaptation while residual > RATIO * echo (user talking).
constexpr float DOUBLE_TALK_RATIO = 1.0f;

// --- HTTPS to OpenAI (mail agent, speech, images) --------------------------
// Whole request incl. waiting: the API sends nothing until reasoning, a web search or an
// image is done.
constexpr uint32_t OPENAI_TIMEOUT_MS = 180000;

// --- Commands by e-mail (agent over the Responses API, no voice session) ---------
constexpr int AGENT_MAX_ROUNDS = 20;                // tool rounds per task, then one answer without tools
constexpr uint32_t MAIL_POLL_MS = 60000;            // inbox check between conversations
// After a conversation the background TLS starts staggered (at the same time it ran out of
// internal RAM): memory, then the mail check, then the scheduled tasks' agent and backup.
constexpr uint32_t MEMORY_IDLE_DELAY_MS = 2000;
constexpr uint32_t MAIL_IDLE_DELAY_MS = 10000;
constexpr uint32_t TASKS_IDLE_DELAY_MS = 15000;
constexpr const char* TTS_MODEL = "gpt-4o-mini-tts-2025-12-15";
// Voice notes and the first words after the wake word (#106). On that evening's 9 first utterances
// (28.10.2026): gpt-transcribe all right, gpt-4o-transcribe all, whisper-1 one wrong,
// gpt-4o-mini-transcribe "Ich dann im Schlauchzimmer" for "Licht an im Schlafzimmer".
constexpr const char* TRANSCRIBE_MODEL = "gpt-transcribe";
constexpr int SPEECH_MAX_SECONDS = 90;              // longest announcement (24 kHz PCM in PSRAM)

// --- Telegram (services/telegram; TELEGRAM_BOT_TOKEN / TELEGRAM_OWNER_ID in secrets.h) ----
constexpr const char* TELEGRAM_HOST = "api.telegram.org";
constexpr uint32_t TELEGRAM_IDLE_DELAY_MS = 5000;        // polling starts this long after a conversation
constexpr int TELEGRAM_POLL_S = 2;                       // long polling: a new message comes at once
constexpr uint32_t TELEGRAM_YIELD_MS = 3000;             // handed the TLS gate over: wait before taking it again
constexpr uint32_t TELEGRAM_RETRY_MS = 30000;            // Telegram unreachable: next try
constexpr uint32_t TELEGRAM_TIMEOUT_MS = 30000;          // one request (sending a photo: 60 s)
constexpr uint32_t TELEGRAM_THREAD_MS = 30 * 60 * 1000;  // chat quiet this long: archived, the next message starts anew
constexpr time_t TELEGRAM_MAX_AGE_S = 2 * 3600;          // older messages (device was off) are not carried out
constexpr size_t TELEGRAM_MESSAGE_CHARS = 4000;          // per message (Telegram's limit is 4096)
constexpr int TELEGRAM_VOICE_MAX_S = 300;                // longer voice notes are not transcribed
constexpr size_t TELEGRAM_PHOTO_BYTES = 1024 * 1024;     // largest photo size taken (Telegram offers several)
constexpr size_t TELEGRAM_DOCUMENT_BYTES = 1024 * 1024;  // text files and PDFs sent to the bot
constexpr size_t FILE_MAX_BYTES = 1024 * 1024;           // a file kept in Drive and sent on (assistant/files)
constexpr size_t TELEGRAM_SPOKEN_CHARS = 1500;           // voice reply: longer answers are cut (text comes too)

// --- WhatsApp (services/whatsapp; WHATSAPP_* in secrets.h, the relay in services/whatsapp-relay/)
// Thread length, age limit, voice and photo sizes: as for Telegram (TELEGRAM_* above).
constexpr const char* WHATSAPP_GRAPH_HOST = "graph.facebook.com";
constexpr const char* WHATSAPP_GRAPH_VERSION = "v26.0";
constexpr int WHATSAPP_POLL_S = 20;                      // long polling at the relay (it waits at most 25 s)
constexpr uint32_t WHATSAPP_RETRY_MS = 30000;            // relay unreachable: next try
constexpr uint32_t WHATSAPP_TIMEOUT_MS = 30000;          // one request to the Graph API
constexpr size_t WHATSAPP_MESSAGE_CHARS = 4000;          // per message (WhatsApp's limit is 4096)
// Announcements from a household server (the relay's /event) wait for someone in the room:
constexpr uint32_t ANNOUNCE_HOLD_MS = 12UL * 3600 * 1000;  // forgotten after (the report is in Telegram)
constexpr size_t ANNOUNCE_HOLD_MAX = 3;                    // the oldest dropped beyond

// --- Phone (phone/; SIP_USER / SIP_PASSWORD / PHONE_CALLERS / PHONE_PIN in secrets.h) -------
constexpr const char* PHONE_REGISTRAR = SIP_REGISTRAR;
constexpr uint16_t SIP_PORT = 5060;
constexpr uint16_t PHONE_RTP_PORT = 40000;
constexpr int PHONE_REGISTER_S = 300;                    // registration renewed after 80 % of this
constexpr uint32_t PHONE_RETRY_MS = 60000;               // registration failed: next try
constexpr uint32_t PHONE_JITTER_MS = 60;                 // The assistant's voice buffered before it goes out
constexpr uint32_t PHONE_CONNECT_TIMEOUT_MS = 20000;     // it rings at most this long while GPT Live starts
constexpr uint32_t PHONE_PIN_TIMEOUT_MS = 30000;
constexpr uint32_t PHONE_RING_TIMEOUT_MS = 30000;        // The assistant calls: nobody picks up after this long
constexpr uint32_t PHONE_CALL_WAIT_MS = 30 * 60 * 1000;  // a requested call waits for a running conversation
constexpr uint32_t PHONE_GREETING_DELAY_MS = 300;        // The assistant answers the call itself
constexpr uint32_t PHONE_WAITING_MS = 60000;             // a call during a conversation at home rings this long
constexpr uint32_t PHONE_IDLE_TIMEOUT_MS = 20000;        // silence on the line before the assistant hangs up

// --- Long-term memory (assistant/memory) ---------------------------------------
// Tiered by age: the prompt has the facts, the last hours and the current topics; the flash
// (LittleFS, "spiffs" partition) an index of all conversations; the mailbox the details
// (topic dossiers, transcripts) and a backup of the core.
constexpr int MEMORY_MAX_FACTS = 60;                // consolidation merges beyond this (also in prompts.h)
constexpr int MEMORY_RECENT_HOURS = 48;             // short-term memory: conversation summaries in the prompt
constexpr size_t MEMORY_PENDING_CHARS = 3000;       // end of a just-ended, not yet consolidated transcript in the prompt
constexpr int MEMORY_RECENT = 12;                   // ... at most this many
constexpr size_t MEMORY_RECENT_MINOR = 3;           // short commands only while among the newest this many
constexpr int MEMORY_ACTIVE_DAYS = 14;              // topics changed since: with status in the prompt
constexpr int MEMORY_TOPIC_TITLES = 25;             // topics named in the prompt (older ones by title only)
constexpr int MEMORY_TOPICS = 40;                   // topics known on the device; older ones only in the mailbox
constexpr size_t MEMORY_INDEX_BYTES = 400 * 1024;   // conversation index in flash; older ones only in the mailbox
constexpr size_t MEMORY_TRANSCRIPT_CHARS = 16000;   // kept per conversation (~15 min of talking)
constexpr size_t MEMORY_RECALL_CHARS = 4000;        // search result / dossier given to the model
constexpr size_t MEMORY_DOSSIER_CHARS = 24000;      // topic history kept in its mail (oldest entries go)
// The core's backup mail (JSON): its body is an Arduino String when stored and read back, and a
// String corrupts content beyond 64 KB. Larger cores are not backed up (an error in the log).
constexpr size_t MEMORY_BACKUP_CHARS = 60000;
constexpr uint32_t MEMORY_OUTBOX_RETRY_MS = 300000; // mailbox unreachable: next try
// Dreaming (assistant/memory_dream): the nightly review across conversations.
constexpr int DREAM_HOUR = 3;                       // local hour it runs in (once a day, between conversations)
constexpr int DREAM_DAYS = 7;                       // conversations of the last days it looks at
constexpr size_t DREAM_CONVERSATION_CHARS = 40000;  // ... at most this much of their summaries (newest first)
constexpr int DREAM_MIN_DAYS = 2;                   // a pattern needs conversations on this many distinct days
constexpr int DREAM_MAX_REMOVED_PERCENT = 60;       // more facts gone in one night: the dream is not applied
constexpr int RULE_MAX = 40;                        // behaviour rules (all in the prompt; the dream merges)
constexpr size_t RULE_CHARS = 240;                  // one rule: one or two sentences
constexpr int SKILL_MAX = 20;                       // skills (all named in the prompt)
constexpr size_t SKILL_NAME_CHARS = 40;
constexpr size_t SKILL_WHEN_CHARS = 200;            // in the prompt of every conversation
constexpr size_t SKILL_INSTRUCTION_CHARS = 4000;    // loaded by use_skill
constexpr const char* MEMORY_FOLDER_CORE = ASSISTANT_NAME "-Gedaechtnis";      // backup of the core (one mail)
constexpr const char* MEMORY_FOLDER_TOPICS = ASSISTANT_NAME "-Themen";         // one dossier per topic
constexpr const char* MEMORY_FOLDER_CONVERSATIONS = ASSISTANT_NAME "-Gespraeche";  // one mail per conversation

// --- Scheduled tasks (assistant/tasks) ------------------------------------------------
constexpr int TASK_MAX = 30;
constexpr time_t TASK_CATCHUP_S = 2 * 3600;         // a run missed longer ago is skipped

// --- Further MCP servers (services/mcp), added by the admin at run time -------------------
constexpr int MCP_MAX_SERVERS = 5;
constexpr int MCP_MAX_TOOLS = 25;                   // per server; more: connect with only_tools
constexpr size_t MCP_DESCRIPTION_CHARS = 600;       // of each tool description given to the model
constexpr size_t MCP_RESULT_CHARS = 12000;          // of a tool result given to the model
constexpr size_t MCP_REPLY_BYTES = 512 * 1024;      // largest answer read (tools/list of a big server)
constexpr uint16_t MCP_TIMEOUT_MS = 30000;          // one request (HTTPClient: at most 65 s)
constexpr uint32_t MCP_RETRY_MS = 5 * 60000;        // retry loading a server's tools while idle
constexpr uint32_t MCP_IDLE_CLOSE_MS = 60000;       // a kept connection unused this long is closed

// --- Home Assistant (MCP server in the LAN, HA_URL / HA_TOKEN in secrets.h) --
constexpr uint32_t HA_EVENTS_RETRY_MS = 10000;      // reconnect of the event WebSocket
constexpr uint32_t HA_RETRY_MS = 30000;             // retry loading the tools while idle
constexpr uint32_t HA_AWAY_MS = 5 * 60000;          // after a failed connection: no tries from the main loop
// Quick home commands (voice/quick_home, #75): GPT Live's first answer at the speaker is held
// until the first utterance is judged; a home command goes to HA's own sentence matching.
constexpr uint16_t QUICK_HOME_TIMEOUT_MS = 3000;    // per try; HA answers in ~0.1-0.5 s (the first after its restart >5 s)
constexpr uint32_t QUICK_SILENCE_MS = 300;          // GPT Live's transcript of the user rested: judge it
constexpr uint32_t QUICK_SETTLED_MS = 1200;         // no new words: the sentence is over (no full stop needed)
constexpr uint32_t QUICK_HOLD_MAX_MS = 8000;        // the hold ends at the latest (after session.started)
constexpr uint32_t QUICK_HOLD_AUDIO_MS = 6000;      // GPT Live's held voice kept in PSRAM (24 kHz: 288 KB)
// The first words after the wake word (voice/first_words, #106; the Echo's cmd/assistant/capture.go):
// the device records and decides before GPT Live gets any audio (quick command, no words, not meant
// for the assistant, or GPT Live with the recording). GPT Live connects ahead without audio meanwhile.
constexpr uint32_t FIRST_WORDS_PAUSE_MS = 600;      // a pause that ends the utterance
constexpr uint32_t FIRST_WORDS_QUIET_MS = 4000;     // nothing (more) said: decided without words
constexpr uint32_t FIRST_WORDS_CUT_MS = 10000;      // an utterance without a pause goes to the transcription now
constexpr uint32_t FIRST_WORDS_MIN_VOICE_MS = 80;   // less voice is a click
constexpr uint32_t FIRST_WORDS_MAX_MS = 15000;      // kept in PSRAM (24 kHz: 720 KB)
// Below this probability that the words were meant for the assistant (QUICK_ADDRESSED_QUESTION) a start by
// the wake word ends silently; low on purpose: a real request lost costs more than a false start.
constexpr float FIRST_WORDS_ADDRESSED_NOT = 0.2f;
// GPT Live takes "no more than 1.2x real-time speed with bursts of at most 5 seconds": the recording
// goes out as a burst of up to this much credit, then at 1.1x (the Echo's live.micBurst).
constexpr uint32_t MIC_BURST_MS = 3500;
constexpr uint32_t PRESENCE_POLL_MS = 60000;        // person.* states (who is at home, services/presence)
constexpr uint32_t PRESENCE_STALE_MS = 5 * 60000;   // older data is not used
// Presence sensor at the speaker (system/occupancy). The chip's presence flag is unreliable both
// ways: someone sitting still fades from it within a minute, and after someone walked out it can
// stay on in the empty room for good. So: motion <= -ROOM_LEAVE_MOTION (a warm body leaving the
// view) stops trusting the presence flag until motion comes in again (motion flag, positive);
// presence without any motion flag counts for ROOM_PRESENCE_ONLY_MS at most. The room is empty
// ROOM_EMPTY_AFTER_MS after the last sign once walked out, else ROOM_STILL_EMPTY_MS after it.
// The state goes to HA again every ROOM_REPORT_MS (HA forgets it on restart).
constexpr uint32_t ROOM_EMPTY_AFTER_MS = 60000;
constexpr uint32_t ROOM_STILL_EMPTY_MS = 10 * 60000;
constexpr uint32_t ROOM_PRESENCE_ONLY_MS = 30 * 60000;
constexpr int ROOM_LEAVE_MOTION = 1000;   // walking out (04.10.): -2415, -2464, -1811
constexpr int ROOM_COME_IN_MOTION = 400;  // coming in: 518 at once, then 1700-2400; noise up to ~220
constexpr uint32_t ROOM_REPORT_MS = 5 * 60000;
constexpr uint32_t ROOM_REPORT_MIN_MS = 5000;  // changes to HA at most this often (the flag pulses)
// After a start the sensor settles: its first flags and a motion spike made the empty room count
// as occupied (and trusted) for 10 min after each restart (25.10.2026). Not judged until then.
constexpr uint32_t ROOM_WARMUP_MS = 60000;

// --- Printer (IPPS in the LAN) and image generation -----------------------
constexpr uint16_t PRINTER_PORT = 631;                   // PRINTER_HOST in secrets.h
constexpr const char* PRINTER_PATH = "/ipp/print";
constexpr uint32_t PRINTER_WAIT_MS = 180000;             // wait for the previous page / job
constexpr const char* IMAGE_MODEL = "gpt-image-2";
constexpr const char* IMAGE_SIZE = "1024x1536";          // portrait, like A4
constexpr const char* IMAGE_SIZE_LANDSCAPE = "1536x1024";  // create_image landscape (A4 sideways)
constexpr const char* IMAGE_QUALITY = "medium";          // ~30 s per image
constexpr int IMAGE_QUEUE_LENGTH = 3;                    // image jobs waiting at most
constexpr int IMAGE_ATTEMPTS = 3;                        // only when no image was made (no connection, 5xx)

// --- E-mail (The assistant's own mailbox; hosts, MAIL_ADDRESS / MAIL_PASSWORD in secrets.h) ---------
constexpr uint16_t MAIL_SMTP_PORT = 465;                 // implicit TLS
constexpr uint16_t MAIL_IMAP_PORT = 993;
constexpr uint32_t MAIL_LIST_LIMIT = 10;                // newest mails in list_emails
constexpr size_t MAIL_READ_LIMIT = 6000;                 // characters of a mail given to the model
constexpr size_t NOTE_READ_LIMIT = 32000;                // characters of a memory note read back
constexpr uint32_t MAIL_SLOW_MS = 3000;                  // longer waits for the mailbox are logged
constexpr unsigned long MAIL_TLS_TIMEOUT_S = 15;         // TLS handshake (default 120 s: The assistant hung for minutes)

// --- Watched private mailboxes (WATCHED_MAILBOXES in secrets.h; assistant/mail_watch) --------
constexpr uint32_t MAILWATCH_POLL_MS = 5 * 60 * 1000;   // new mails checked between conversations
// During a conversation: the listener's mailboxes only, once it runs this long, then every
// MAILWATCH_LIVE_POLL_MS while the backend is not working (in the Telegram/WhatsApp task, not
// the tools worker: the conversation's calls come first).
// On 05.10. each such check stalled the conversation 2-3 s: not its network traffic but writing
// /mailwatch.json (125 KB, 3.5-6.7 s of flash writes that stall both cores). Since 06.10. the
// state is written only after the conversation (mail_watch.cpp, flushSave). false: no check
// during conversations at all, should hangs come back with it.
constexpr bool MAILWATCH_IN_CONVERSATION = true;
constexpr uint32_t MAILWATCH_LIVE_AFTER_MS = 2 * 60 * 1000;
constexpr uint32_t MAILWATCH_LIVE_POLL_MS = 3 * 60 * 1000;
constexpr size_t MAILWATCH_BATCH = 10;                   // mails per mailbox and check (the rest the next time)
constexpr size_t MAILWATCH_TEXT_CHARS = 1500;            // start of a mail's text given to the classifier
constexpr int MAILWATCH_DIGEST_HOUR = 18;                // the day's less urgent mails in one message
constexpr int MAILWATCH_LOG_DAYS = 60;                   // sorted mails kept (index for mail_news and the search)
constexpr size_t MAILWATCH_LOG_MAX = 800;
constexpr int MAILWATCH_INDEX_DAYS = 14;                 // older mails filed once when a mailbox is new
constexpr uint32_t MAILWATCH_INDEX_MS = 30000;           // one batch of them per this time
constexpr size_t MAILWATCH_SEARCH_MAX = 40;              // mails per mailbox in a search result (Gmail ~0.23 s each)
constexpr int MAILWATCH_SEARCH_DAYS = 365;               // furthest back a search goes
constexpr int MAILWATCH_SHIPMENT_DAYS = 21;              // a shipment without news is dropped after this (from its mail's date)
constexpr int MAILWATCH_SHIPMENT_LATE_DAYS = 2;          // expected this many days ago without news: no longer on the way
constexpr int MAILWATCH_ATTEMPTS = 3;                    // failed classifications before the mails are skipped
// Sorting into folders and rules (assistant/mail_sort)
constexpr size_t MAILSORT_MAX_MAILS = 200;               // mails moved by one sort_mail call
constexpr size_t MAILSORT_RULE_MAX = 30;                 // rules per person
constexpr int MAILSORT_SUGGEST_MIN = 10;                 // mails of a kind in a week before the digest suggests a rule
constexpr int MAILSORT_SUGGEST_DAYS = 14;                // the same suggestion again after this

// --- Spotify (services/spotify; SPOTIFY_* in secrets.h, tokens from scripts/spotify_auth.py) ---------
#ifndef SPOTIFY_CLIENT_ID
#define SPOTIFY_CLIENT_ID ""
#endif
#ifndef SPOTIFY_CLIENT_SECRET
#define SPOTIFY_CLIENT_SECRET ""
#endif
#ifndef SPOTIFY_REFRESH_TOKEN
#define SPOTIFY_REFRESH_TOKEN ""  // the admin's account; "" = no Spotify
#endif
#ifndef SPOTIFY_ACCOUNTS
// Other people's accounts, comma separated: {person (name or id in the register), refresh token}.
#define SPOTIFY_ACCOUNTS
#endif
#ifndef SPOTIFY_ROOM_SPEAKERS
// The Spotify devices in the assistant's room, comma separated, as Spotify names them (music_speakers
// list), speaker groups that include the room too. Music on them is turned down while the assistant
// talks at the speaker; the first one plays "spiel ..." when nothing plays anywhere. "" = music on
// every speaker at home is turned down (not on phones or computers), and no default speaker.
#define SPOTIFY_ROOM_SPEAKERS ""
#endif
constexpr uint32_t SPOTIFY_TIMEOUT_MS = 15000;
constexpr int SPOTIFY_DUCK_VOLUME = 20;          // music in the assistant's room lower than this while it talks; 0 = off
constexpr size_t SPOTIFY_LIST_TRACKS = 15;       // songs of a list the model gives (one search each, ~0.3 s)
constexpr int SPOTIFY_PLAYLIST_PAGES = 4;        // of 50 playlists each, searched by name
constexpr uint32_t SPOTIFY_SETTLE_MS = 700;      // after next/previous/play before the state shows the new song

// --- Google Maps Platform (GOOGLE_MAPS_API_KEY in secrets.h) --------
// Requests per calendar month (the free tier), counted in /quota.json before each request.
constexpr int PLACES_MONTHLY_LIMIT = 1000;
constexpr int ROUTES_MONTHLY_LIMIT = 1000;
constexpr int WEATHER_MONTHLY_LIMIT = 9000;              // free: 10,000 (now, hours, days, alerts)
constexpr int POLLEN_MONTHLY_LIMIT = 4500;               // free: 5,000
constexpr int AIR_QUALITY_MONTHLY_LIMIT = 9000;          // free: 10,000
constexpr int GEOCODING_MONTHLY_LIMIT = 9000;            // free: 10,000 (locate_person: position -> address)
constexpr int YOUTUBE_MONTHLY_LIMIT = 2500;             // searches; Google allows 100 a day
constexpr uint32_t TV_DISCOVERY_MS = 60 * 60 * 1000;     // DIAL TVs found in the LAN are kept this long
constexpr uint32_t YOUTUBE_SEEK_DELAY_MS = 5000;         // after the video is sent: then seek (the TV resumes otherwise)
constexpr uint32_t LOUNGE_SESSION_MS = 10 * 60 * 1000;   // a TV's YouTube session is reused this long, then opened anew
// summarize_video (GEMINI_API_KEY in secrets.h): Gemini watches the YouTube video.
constexpr const char* GEMINI_MODEL = "gemini-3.8-flash";
constexpr uint32_t GEMINI_TIMEOUT_MS = 180000;           // a 24 min video took 13.6 s (30.09.)
constexpr int GEMINI_MONTHLY_LIMIT = 1000;               // requests counted in /quota.json
// web_search over Tavily (TAVILY_API_KEY in secrets.h; services/websearch).
constexpr int TAVILY_MONTHLY_LIMIT = 1000;               // basic searches (1 credit each); free: 1,000
constexpr int TAVILY_RESULTS = 5;                        // sources per search
constexpr size_t TAVILY_EXCERPT_CHARS = 600;             // of each source's text given to the model
constexpr uint32_t TAVILY_TIMEOUT_MS = 20000;
constexpr int QUOTA_WARN_PERCENT = 80;                   // Telegram to the admin, once a month
constexpr int PLACES_RESULTS = 5;                        // places per search (billed per request)
constexpr int PLACES_BIAS_METERS = 20000;                // prefer places this close to home (HA zone.home)
