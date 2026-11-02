#include "system/console.h"
#include "system/error_log.h"
#include "services/ha_device.h"

#include <Arduino.h>
#include <M5Unified.h>
#include <esp_heap_caps.h>
#include <Wire.h>
#include <hal/usb_serial_jtag_ll.h>

#include "assistant/agenda.h"
#include "assistant/memory.h"
#include "assistant/people.h"
#include "assistant/prompt.h"
#include "assistant/shared.h"
#include "assistant/tools.h"
#include "audio/audio.h"
#include "audio/wakeword.h"
#include "config.h"
#include "phone/phone.h"
#include "services/home_assistant.h"
#include "services/openai.h"
#include "services/psram_json.h"
#include "services/psram_sink.h"
#include "system/network.h"
#include "system/occupancy.h"
#include "system/peer.h"
#include "ui/mute.h"
#include "voice/live_session.h"
#include "voice/quick_home.h"

namespace console {
namespace {

bool wakeDebug = false;
String firstWordsText;  // serial ~
extern bool portMonitor;
void startPortMonitor();

// Slow tools (mail, mailbox) in the registry's worker, as in a conversation: its stack is large
// enough for TLS, the main loop's is not. The worker prints the result.
void startSlowTool(const char* name, const char* arguments, const String& person = "")
{
    String error;
    if (!tools::start(name, arguments, "", tools::Console, error, person)) Serial.println(error);
}

void runCommand(int command)
{
    switch (command) {
        case 's':
            live::toggle();
            break;
        case 'K':  // the session of a call the assistant makes for the user (phone_call), without dialling anyone
            if (!live::open()) live::startCall("00000", "Test task: only start the session, call nobody.", true);
            break;
        case 'W':  // as if "Hey Jarvis" was heard (answer clip, early recording, claim on the peer link)
            if (!live::open()) {
                live::start(true);
                peer::claim(0);
            }
            break;
        case 'Y':  // the peer link: this device, the others, the level now (system/peer)
            peer::print();
            shared::print();  // the leader's context and what it has not got yet
            break;
        case 'O':  // as if the leader were away (tests of the emergency memory), again: back
            peer::setOffline(!peer::offline());
            Serial.printf("\n[Peer] leader %s (test)\n", peer::offline() ? "away" : "back");
            break;
        case 'c':
            // No fixed subject: the test conversations land in the memory, and a subject told
            // again and again became the assistant's "favourite topic" (30.09.).
            live::sendCommentary("Erzaehle jetzt ohne Rueckfrage eine kurze Geschichte von etwa 30 "
                                 "Sekunden zu einem Thema deiner Wahl.");
            break;
        case 'E':  // tests the end of a conversation (voice/conversation_end)
            live::sendCommentary("The user just said goodbye. Say a short goodbye now.");
            break;
        case 'h':
            if (!live::open()) ha::loadTools();
            break;
        case 'Q': {  // the quick home commands' rules on fixed sentences (voice/quick_home), then HA's
                     // Assist with a room that does not exist: the request and its answer, nothing switched
            quick_home::selfTest();
            ha::AssistResult r;
            const bool answered = ha::assist("Licht aus im Regressionstestzimmer", r, QUICK_HOME_TIMEOUT_MS);
            Serial.printf("[Quick test] HA: %s %s\n", answered ? r.type.c_str() : "no answer", r.code.c_str());
            break;
        }
        case 'p':
            startSlowTool("print_document",
                          "{\"title\":\"" ASSISTANT_NAME " Testseite\",\"html\":\"<h1>Testseite</h1><p>Umlaute: "
                          "äöü ÄÖÜ ß</p><ul><li>250 g Mehl</li><li><b>500 ml</b> Milch</li></ul>\"}");
            break;
        case 'i': {  // costs one image (~5 ct)
            JsonDocument arguments;
            arguments["prompt"] = "Einfache Strichzeichnung eines Hauses, schwarz auf weiss";
            arguments["title"] = ASSISTANT_NAME " Testbild";
            arguments["print"] = false;
            arguments["color"] = false;
            arguments["email_to"] = "me";
            String json;
            serializeJson(arguments, json);
            Serial.println(tools::run("create_image", json.c_str(), tools::Console));
            break;
        }
        case 'm':
            startSlowTool("list_emails", "{\"unread_only\":false}");
            break;
        case 'g':
            memory::print();
            break;
        case 'V':  // switches the amplifier off as a brown-out would (tests board::checkAmplifier)
            Wire1.beginTransmission(0x5B);
            Wire1.write(0x01);  // SYSCTRL
            Wire1.write(0x00);
            Serial.printf("[Board] amplifier switched off (test): %d\n", Wire1.endTransmission());
            break;
        case 'C':  // The assistant's character as the prompts get it: nobody named, then each person
            Serial.printf("[Persona] unknown: %s", prompt::persona("").c_str());
            for (const people::Person& p : people::all())
                Serial.printf("[Persona] %s: %s", p.id.c_str(), prompt::persona(p.id).c_str());
            break;
        case 'J':  // Home Assistant's MQTT device: connection and states (services/ha_device)
            ha_device::print();
            break;
        case 'L':  // the error log (system/error_log)
            errorlog::print();
            break;
        case 'P':  // the people register
            people::print();
            break;
        case 'U':  // this month's conversations: delegations and backend tokens (voice/live_session)
            live::printUsage();
            break;
        case 'F':  // FritzBox: phone registration and call
            phone::print();
            break;
        case 'H':  // heap: TLS needs a large internal block (~40 KB per handshake)
            Serial.printf("[RAM] heap total internal %u, PSRAM %u\n", unsigned(heap_caps_get_total_size(MALLOC_CAP_INTERNAL)),
                          unsigned(heap_caps_get_total_size(MALLOC_CAP_SPIRAM)));
            Serial.printf("[RAM] internal free %u, largest block %u, lowest %u; PSRAM free %u\n",
                          unsigned(heap_caps_get_free_size(MALLOC_CAP_INTERNAL)),
                          unsigned(heap_caps_get_largest_free_block(MALLOC_CAP_INTERNAL)),
                          unsigned(heap_caps_get_minimum_free_size(MALLOC_CAP_INTERNAL)),
                          unsigned(heap_caps_get_free_size(MALLOC_CAP_SPIRAM)));
            // Stack never used so far per long-lived task: what their size could shrink by.
            for (const char* name : {"loopTask", "tools", "telegram", "memory", "tasks", "audio", "wakeword", "rtp", "image"}) {
                const TaskHandle_t task = xTaskGetHandle(name);
                if (task) Serial.printf("[RAM] stack %s: never used %u bytes\n", name,
                                        unsigned(uxTaskGetStackHighWaterMark(task)));
            }
            break;
        case 't':
            Serial.println(tools::run("list_tasks", "{}", tools::Console));
            break;
        case 'a':  // the agenda as the admin's chats get it (assistant/agenda)
            agenda::print();
            break;
        case '>': {  // ">create_task {...}": any tool of the backend model, result to the log;
                     // ">@anna create_task {...}": as that person (assistant/people)
            String line = Serial.readStringUntil('\n');
            line.trim();
            String person;
            if (line.startsWith("@")) {
                const int end = line.indexOf(' ');
                person = end < 0 ? line.substring(1) : line.substring(1, end);
                line = end < 0 ? String() : line.substring(end + 1);
            }
            const int space = line.indexOf(' ');
            const String name = space < 0 ? line : line.substring(0, space);
            const String arguments = space < 0 ? String("{}") : line.substring(space + 1);
            if (tools::isSlow(name.c_str())) {
                startSlowTool(name.c_str(), arguments.c_str(), person);
                break;
            }
            Serial.printf("[Tool] %s %s\n-> %s\n[Tool-End]\n", name.c_str(), arguments.c_str(),
                          tools::run(name.c_str(), arguments.c_str(), tools::Console, person).c_str());
            break;
        }
        case 'D':    // the tool definitions of a conversation's backend (tests/device: contract test)
        case 'A': {  // ... and of the agent (mail commands, scheduled tasks)
            JsonDocument list(PsramJson::instance());
            tools::describe(list.to<JsonArray>(), command == 'D' ? tools::Voice : tools::Agent);
            Serial.println("[TOOLS-BEGIN]");
            serializeJson(list, Serial);
            Serial.println("\n[TOOLS-END]");
            break;
        }
        case 'X':  // capital X on purpose: deletes all memory
            if (!live::open()) memory::wipe();
            break;
        case 'N':  // WiFi: the access points in range (which one is joined: system/network)
            network::printScan();
            break;
        case 'M':  // the Grove port: I2C scan, then pin levels until M again
            portMonitor = !portMonitor;
            if (portMonitor) startPortMonitor();
            break;
        case 'R':  // presence sensor: raw values every second
            occupancy::toggleDebug();
            break;
        case 'Z':  // the microphone off / on, as holding the button (ui/mute)
            mute::set(!mute::on());
            break;
        case '~': {  // "~Wie spät ist es?": spoken by the TTS as the first words after the wake word
                     // (voice/first_words); a few tenths of a cent
            firstWordsText = Serial.readStringUntil('\n');
            firstWordsText.trim();
            if (firstWordsText.isEmpty() || live::open()) {
                Serial.println("[First words test] needs a text and no conversation");
                break;
            }
            // TLS needs the worker's stack; the main loop takes the speech over (live::update)
            tools::post([] {
                PsramSink speech(2 * 1024 * 1024);
                const String failed = openai::speech(firstWordsText, "pcm", speech);
                if (failed.length()) Serial.printf("[First words test] %s\n", failed.c_str());
                else {
                    const size_t samples = speech.size / 2;
                    live::testFirstWords(reinterpret_cast<int16_t*>(speech.release()), samples);
                }
            });
            break;
        }
        case 'w':
            wakeDebug = !wakeDebug;
            Serial.printf("Wake word: %s, display %s\n", wakeword::ready() ? "active" : "OFF",
                          wakeDebug ? "on" : "off");
            break;
    }
}

// Diagnostics for a sensor on the Grove port (G1/G2): which I2C devices answer there, and the
// level changes of both pins as digital inputs (pull-down) plus their analog values.
bool portMonitor = false;

void startPortMonitor()
{
    bool found[128] = {};
    M5.Ex_I2C.begin();
    M5.Ex_I2C.scanID(found);
    M5.Ex_I2C.release();
    Serial.printf("[Port] I2C on G%d/G%d:", M5.Ex_I2C.getSDA(), M5.Ex_I2C.getSCL());
    for (int address = 8; address < 120; ++address)
        if (found[address]) Serial.printf(" 0x%02X", address);
    Serial.println();
    // The pyramid's own bus (codec, amplifier, touch controller) also reaches its Grove port.
    Serial.print("[Port] I2C of the pyramid (Wire1):");
    for (int address = 8; address < 120; ++address) {
        Wire1.beginTransmission(address);
        if (Wire1.endTransmission() == 0) Serial.printf(" 0x%02X", address);
    }
    Serial.println();
    for (int pin : {1, 2}) pinMode(pin, INPUT_PULLDOWN);
    Serial.println("[Port] watching G1/G2 (M again: stop)");
}

void logPort()
{
    static int last[2] = {-1, -1};
    static uint32_t lastAnalog = 0;
    if (!portMonitor) return;
    const int pins[2] = {1, 2};
    for (int i = 0; i < 2; ++i) {
        const int level = digitalRead(pins[i]);
        if (level != last[i]) Serial.printf("[Port] %lu ms G%d -> %d\n", millis(), pins[i], level);
        last[i] = level;
    }
    if (millis() - lastAnalog < 2000) return;
    lastAnalog = millis();
    Serial.printf("[Port] analog G1 %d mV, G2 %d mV\n", analogReadMilliVolts(1), analogReadMilliVolts(2));
    for (int pin : pins) pinMode(pin, INPUT_PULLDOWN);  // analogRead takes the pin over
}

void logWakeWord()
{
    static uint32_t lastLog = 0;
    if (!wakeDebug || !wakeword::ready() || millis() - lastLog < 500) return;
    lastLog = millis();
    Serial.printf("[wake] %3d/255  mic %d%%\n", wakeword::takePeakProbability(), audio::micLevel());
}

// Output of the S3's USB serial (HWCDC, Arduino 2.x) could sit in the device for a minute, even
// the second half of a single printf, until the next output pushed it out. Two ways: bytes in
// the ring buffer while the "send FIFO empty" interrupt is off (its routine switches it off when
// it finds the buffer empty; a concurrent write then waits), and bytes in the hardware FIFO that
// were never flushed as a packet (then the FIFO is not empty and the interrupt never comes).
// Every 100 ms: flush the FIFO and switch the interrupt on (with nothing to send it goes off).
void kickSerialOutput()
{
    static uint32_t last = 0;
    if (millis() - last < 100) return;
    last = millis();
    usb_serial_jtag_ll_txfifo_flush();
    usb_serial_jtag_ll_ena_intr_mask(USB_SERIAL_JTAG_INTR_SERIAL_IN_EMPTY);
}

}  // namespace

void update()
{
    if (Serial.available()) runCommand(Serial.read());
    kickSerialOutput();
    logWakeWord();
    logPort();
}

}  // namespace console
