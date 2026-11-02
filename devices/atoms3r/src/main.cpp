// Smart speaker: AtomS3R + Echo Pyramid talking to OpenAI GPT Live.
//   "Hey Jarvis" or the screen button starts a conversation, the button ends it.
//   TP2 / TP3: louder, TP1 / TP4: quieter.
//   Between conversations the assistant takes commands by e-mail and chats over Telegram. It can
//   also be called: an IP phone of the FritzBox.
//
// This file only wires the modules together. One folder per concern:
//   audio/      codec streaming, echo canceller, wake word, volume
//   voice/      GPT Live session, transcript, announcements
//   assistant/  tools of the backend model, long-term memory, scheduled tasks, mail agent,
//               prompt context
//   services/   Home Assistant, images, printer, e-mail, Telegram, OpenAI HTTP
//   phone/      SIP phone at the FritzBox, RTP audio of a call
//   ui/         display, LEDs, touch pads, error messages
//   system/     board setup, WiFi, serial test commands
#include <M5Unified.h>
#include <esp_heap_caps.h>
#include <mbedtls/platform.h>
#include <mbedtls/ssl_ciphersuites.h>

#include "assistant/agenda.h"
#include "assistant/agent.h"
#include "assistant/mail_watch.h"
#include "assistant/memory.h"
#include "assistant/people.h"
#include "assistant/shared.h"
#include "assistant/tasks.h"
#include "assistant/tools.h"
#include "audio/audio.h"
#include "audio/volume.h"
#include "audio/wakeword.h"
#include "config.h"
#include "phone/phone.h"
#include "services/ha_device.h"
#include "services/ha_events.h"
#include "services/home_assistant.h"
#include "services/images.h"
#include "services/mail.h"
#include "services/mcp.h"
#include "services/spotify.h"
#include "services/telegram.h"
#include "services/whatsapp.h"
#include "system/board.h"
#include "system/console.h"
#include "system/error_log.h"
#include "system/network.h"
#include "system/occupancy.h"
#include "system/peer.h"
#include "ui/display.h"
#include "ui/leds.h"
#include "ui/mute.h"
#include "ui/status.h"
#include "ui/touch.h"
#include "voice/acknowledge.h"
#include "voice/conversation_end.h"
#include "voice/announcer.h"
#include "voice/live_session.h"

// mbedTLS in PSRAM. The framework is built with CONFIG_MBEDTLS_INTERNAL_MEM_ALLOC: every TLS
// connection took ~40 KB of internal RAM (OpenAI, Telegram, mailbox, Google at once), which fell to
// 2 KB after a phone call; worker tasks then could not start and WiFi hung. Espressif offers
// CONFIG_MBEDTLS_EXTERNAL_MEM_ALLOC for this; at run time the same goes through the allocator hook.
void* tlsCalloc(size_t count, size_t size)
{
    // All of it, also the two record buffers (~16.7 KB each): keeping those internal (tried on
    // 28.09.) brought the low back to 2.6 KB and the WiFi hang with it, at the same speed.
    void* memory = heap_caps_calloc(count, size, MALLOC_CAP_SPIRAM | MALLOC_CAP_8BIT);
    return memory ? memory : heap_caps_calloc(count, size, MALLOC_CAP_INTERNAL | MALLOC_CAP_8BIT);
}

// The TLS cipher suites the device offers (read by the patched ssl_client.cpp, see
// scripts/patch_libraries.py): only ECDHE with RSA certificates. Google, OpenAI's Cloudflare and
// the others then send their RSA certificates, which the ESP32's RSA accelerator checks fast;
// with ECDSA (P-256/P-384, software) a connection to Google took 2.1-2.9 s, to Telegram
// (RSA) 0.7 s (30.09.).
const int RSA_SUITES[] = {MBEDTLS_TLS_ECDHE_RSA_WITH_AES_128_GCM_SHA256, MBEDTLS_TLS_ECDHE_RSA_WITH_AES_256_GCM_SHA384,
                          MBEDTLS_TLS_ECDHE_RSA_WITH_AES_128_CBC_SHA256, 0};
extern const int* ssl_client_ciphersuites;
const int* ssl_client_ciphersuites = RSA_SUITES;
// Cloudflare's *.workers.dev (the WhatsApp relay) has ECDSA certificates only, and so do many
// self-hosted servers (Let's Encrypt's ECDSA chain; nginx then answers RSA-only hellos with
// handshake_failure, an n8n MCP server on 06.10.): there and for every MCP server the RSA suites
// plus ECDHE-ECDSA. That handshake is checked in software (~2 s), but both keep their connection.
const int ECDSA_SUITES[] = {MBEDTLS_TLS_ECDHE_RSA_WITH_AES_128_GCM_SHA256, MBEDTLS_TLS_ECDHE_ECDSA_WITH_AES_128_GCM_SHA256,
                            MBEDTLS_TLS_ECDHE_ECDSA_WITH_AES_256_GCM_SHA384, 0};
const int* suitesFor(const char* host)
{
    const size_t length = host ? strlen(host) : 0, suffix = strlen(".workers.dev");
    const bool relay = length > suffix && !strcmp(host + length - suffix, ".workers.dev");
    return relay || mcp::connecting(host) ? ECDSA_SUITES : RSA_SUITES;
}
extern const int* (*ssl_client_ciphersuites_for)(const char* host);
const int* (*ssl_client_ciphersuites_for)(const char* host) = suitesFor;

// Runs one step of the loop and logs it when it blocked the loop for over LOOP_SLOW_MS (the
// microphone, the WebSocket and tool calls all wait meanwhile). Silent otherwise.
template <typename Step>
void timed(const char* name, Step step)
{
    const uint32_t started = millis();
    const bool talking = live::state() == live::State::Live;  // not the connection's handshake
    step();
    const uint32_t took = millis() - started;
    if (took > LOOP_SLOW_MS) Serial.printf("[Loop] %s blocked %lu ms\n", name, (unsigned long)took);
    // In a conversation a hang is audible (no audio in or out): kept for later reading.
    if (talking && took >= AUDIO_REPORT_GAP_MS)
        errorlog::add("Loop: %s blocked %lu ms in a conversation", name, (unsigned long)took);
}

// No conversation and no announcement: only then the wake word listens (The assistant saying
// "Jarvis" must not wake itself) and the slow background work runs.
bool idle()
{
    return network::connected() && !live::open() && !announcer::active();
}

// Idle for at least `ms`. Background tasks start staggered after a conversation: right after
// hanging up, the mail check and the memory both opened TLS while the WebSocket was still being
// torn down, and the mail check got "SSL - Memory allocation failed" (internal RAM).
bool idleFor(uint32_t ms)
{
    static uint32_t busySince = 0;
    if (!idle()) busySince = millis();
    return idle() && millis() - busySince >= ms;
}

void handleInput()
{
    // Holding the button switches the microphone off and on (ui/mute, the Echo's mute button). So
    // a press acts on release, unless it was such a hold. During an announcement the button
    // silences it (one-way speech, nothing listens); the next press starts a conversation as usual.
    static bool held = false;
    if (!held && !live::open() && M5.BtnA.pressedFor(MUTE_HOLD_MS)) {
        held = true;
        mute::set(!mute::on());
    }
    if (M5.BtnA.wasReleased()) {
        if (!held) ha_device::fire("button");
        if (held) held = false;
        else if (!live::open() && announcer::active()) announcer::stop();
        else if (!live::open() && mute::on()) audio::queueTone(220, 200);  // muted: no conversation
        else live::toggle();
    }
    touch::update();
    console::update();
}

void handleWakeWord()
{
    if (!wakeword::ready()) return;
    wakeword::setListening(idle() && !mute::on());
    if (wakeword::takeDetection() && idle() && !mute::on()) {
        const int peak = wakeword::takePeakProbability();  // the last one is already low again
        Serial.printf("\n[Hey Jarvis] (%d/255)\n", peak);
        live::start(true);
        peer::claim(peak / 255.0f);  // another device may be nearer
        ha_device::fire("wake_word", peak / 255.0f);
    }
}

// The peer link (system/peer): another device heard the wake word louder and answers it.
void handlePeers()
{
    peer::update();
    String why;
    if (peer::takeLost(why)) live::abandon();
}

// The leader puts an announcement or a conversation into this room (a member of a network, as
// the Echos' rooms.go): {"text"} and {"reason", "person", "insist", "repeat_ms"}. The speech is
// made in the tools worker (~2 s over TLS), the loop only takes the call.
QueueHandle_t peerSpeeches = xQueueCreate(2, sizeof(String*));

void speakForPeer()
{
    String* text = nullptr;
    if (xQueueReceive(peerSpeeches, &text, 0) != pdTRUE) return;
    const String result = agent::announce(*text);
    if (result.startsWith("Error")) Serial.printf("[Peer] announcement: %s\n", result.c_str());
    delete text;
}

String answerText(const String& text)
{
    JsonDocument answer;
    answer["text"] = text;
    String out;
    serializeJson(answer, out);
    return out;
}

void setupPeerCalls()
{
    peer::handle("/speak", [](const String& body) {
        JsonDocument q;
        if (deserializeJson(q, body) || !(q["text"] | "")[0]) return answerText("Error: no text");
        auto* text = new String(q["text"] | "");
        if (xQueueSend(peerSpeeches, &text, 0) != pdTRUE || !tools::post(speakForPeer)) {
            delete text;
            return answerText("Error: the speaker is busy with announcements, try again in a moment");
        }
        return answerText("Being announced on this speaker now.");
    });
    peer::handle("/start_conversation", [](const String& body) {
        JsonDocument q;
        if (deserializeJson(q, body)) return answerText("Error: request not readable");
        String person = q["person"] | "";
        people::Person known;
        if (!people::find(person, known)) person = "";  // not in this device's register: whoever is there
        return answerText(live::requestConversation(q["reason"] | "", person, q["insist"] | false,
                                                    int((q["repeat_ms"] | 0LL) / 1000)));
    });
}

void reportImageResult()
{
    String message;
    bool ok = false, spoken = false;
    if (!images::takeResult(message, ok, spoken)) return;
    Serial.printf("\n[Image] %s\n", message.c_str());
    if (ok) {
        audio::queueTone(660, 80);
    } else {
        status::showError(message, 15000);
        if (!spoken) audio::queueTone(220, 300);  // the announcement brings its own tone
    }
    // During a conversation the assistant tells the user itself, at a pause.
    live::notice(message, !ok);
}

// Results of the other tasks (images, mail, mail agent) and work between conversations.
void handleBackgroundWork()
{
    timed("images", [] { reportImageResult(); });
    String callId, output;
    if (tools::takeResult(callId, output) && !live::sendToolOutput(callId, output))
        Serial.println("[Tool] session already over, result dropped");
    timed("announcer", [] { announcer::update(); });
    timed("volume", [] { volume::update(); });
    timed("ha", [] { ha::update(network::connected() && !live::open()); });
    timed("mcp", [] { mcp::update(network::connected() && !live::open()); });
    timed("room", [] { occupancy::update(network::connected() && !live::open()); });
    // Home Assistant's MQTT device: connects between conversations, keeps the link in one (#103)
    timed("ha_device", [] { ha_device::update(live::open()); });
    // Music down once the conversation runs (not while it connects: its first tool calls wait for the worker).
    timed("spotify", [] { spotify::setConversation(live::state() == live::State::Live && !live::onPhone()); });
    timed("idle", [] {
        mail::setIdle(idleFor(MAIL_IDLE_DELAY_MS));
        mailwatch::setIdle(idleFor(MAIL_IDLE_DELAY_MS));
        memory::setIdle(idleFor(MEMORY_IDLE_DELAY_MS));
        tasks::setIdle(idleFor(TASKS_IDLE_DELAY_MS));
        telegram::setIdle(idleFor(TELEGRAM_IDLE_DELAY_MS));
        whatsapp::setIdle(idleFor(TELEGRAM_IDLE_DELAY_MS));
        people::setIdle(idleFor(TASKS_IDLE_DELAY_MS));
    });
    // Scheduled tasks: the main the assistant runs them; a member its emergency ones (assistant/shared).
    if (INBOUND || MEMBER) timed("tasks", [] { tasks::update(); });
    if (!MEMBER) timed("agenda", [] { agenda::update(); });  // a member's agenda is the leader's
    if (INBOUND) timed("ha_events", [] { ha_events::update(); });
}

// What each start step took of the internal RAM and the PSRAM (serial log at boot).
void ramStep(const char* step)
{
    static size_t internal = 0, psram = 0;
    const size_t nowInternal = heap_caps_get_free_size(MALLOC_CAP_INTERNAL);
    const size_t nowPsram = heap_caps_get_free_size(MALLOC_CAP_SPIRAM);
    if (internal)
        Serial.printf("[RAM boot] %-14s internal %6d, PSRAM %8d (internal free %u)\n", step,
                      int(nowInternal) - int(internal), int(nowPsram) - int(psram), unsigned(nowInternal));
    else
        Serial.printf("[RAM boot] start: internal free %u of %u, PSRAM free %u of %u\n", unsigned(nowInternal),
                      unsigned(heap_caps_get_total_size(MALLOC_CAP_INTERNAL)), unsigned(nowPsram),
                      unsigned(heap_caps_get_total_size(MALLOC_CAP_SPIRAM)));
    internal = nowInternal;
    psram = nowPsram;
}

void setup()
{
    mbedtls_platform_set_calloc_free(tlsCalloc, heap_caps_free);  // before the first connection
    // The loop (WebSocket, GPT Live's audio, microphone) above the background tasks on this core
    // (tools, memory, tasks, Telegram: priority 1): a TLS handshake there took half its CPU time.
    // It sleeps 1 ms every round, so they still run. RTP stays above it (3).
    vTaskPrioritySet(nullptr, 2);
    ramStep("");
    String error = board::begin();
    if (error.isEmpty()) occupancy::begin();
    ramStep("board");
    display::begin();
    mute::begin();
    ramStep("display");
    if (error.isEmpty() && (!audio::begin() || !live::begin())) error = "Zu wenig Speicher";
    ramStep("audio+live");
    if (error.length()) {
        status::setFatal(error);
        return;
    }
    if (!memory::begin()) Serial.println("Memory could not start");
    ramStep("memory");
    // The time zone right away: the RTC keeps the time across restarts, but until network::begin
    // sets the zone, local time would be UTC (the error log's start entry said 18:38 at 20:38).
    setenv("TZ", TIMEZONE, 1);
    tzset();
    errorlog::begin();  // after the file system: why the device started
    people::begin();
    tasks::begin();
    ramStep("people+tasks");
    tools::begin();  // after the modules: the registry asks them for their tools
    shared::begin();
    ramStep("tools");
    acknowledge::begin();  // after the registry: its worker makes the clips
    conversation_end::begin();
    ramStep("ack+end check");
    if (INBOUND && !phone::begin()) Serial.println("Phone could not start");  // the line: the main the assistant only
    ramStep("phone");
    if (INBOUND) ha_events::begin();
    network::begin();
    ha_device::begin();  // the WiFi's MAC is its id
    peer::begin();
    if (MEMBER) setupPeerCalls();
    ramStep("ha events+wifi");
    if (WAKE_WORD_ENABLED && !wakeword::begin()) Serial.println("Wake word could not start");
    ramStep("wake word");
    Serial.println(wakeword::ready() ? "Smart speaker ready - say \"Hey Jarvis\" or press the button"
                                     : "Smart speaker ready - the display button starts a conversation");
}

void loop()
{
    timed("m5", [] { M5.update(); });
    if (!status::fatal()) {
        timed("live", [] { live::update(); });  // WebSocket, microphone, hang-up timers
        if (INBOUND) timed("phone", [] { phone::update(); });  // SIP: registration, calls
        timed("input", [] { handleInput(); });  // incl. serial tool calls
        timed("network", [] { network::update(!live::open() && !announcer::active()); });
        timed("wakeword", [] { handleWakeWord(); });
        timed("peer", [] { handlePeers(); });
        handleBackgroundWork();
        timed("microphone", [] { board::checkMicrophone(); });
        timed("amplifier", [] { board::checkAmplifier(); });
        timed("status", [] { status::update(); });
    }
    timed("leds", [] { leds::update(); });
    timed("display", [] { display::update(); });
    delay(1);
}
