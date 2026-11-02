#include "services/images.h"

#include <HTTPClient.h>
#include <atomic>
#include <esp_heap_caps.h>
#include <mbedtls/base64.h>

#include "assistant/agent.h"
#include "assistant/people.h"
#include "assistant/tools.h"
#include "config.h"
#include "services/mail.h"
#include "services/openai.h"
#include "services/printer.h"
#include "services/telegram.h"
#include "system/tls_gate.h"
#include "voice/live_session.h"

namespace images {
namespace {

struct Job {
    String prompt, title, emailTo;
    String person;  // who asked (assistant/people id): Telegram goes to their chat
    bool print = false, color = false, telegram = false, byMail = false;
    bool landscape = false;  // 1536x1024, printed in landscape on A4
    bool reuseLast = false;  // send_last_image: deliver the last image, generate nothing
};
struct Result {
    String message;
    bool ok, spoken;
};

QueueHandle_t jobs = nullptr, results = nullptr;
portMUX_TYPE taskMux = portMUX_INITIALIZER_UNLOCKED;
bool taskRunning = false;          // guarded by taskMux
std::atomic<int> pending{0};       // queued or running jobs

// The last image, used only by the image task.
uint8_t* last = nullptr;
size_t lastLength = 0;
bool lastLandscape = false;
String lastTitle, lastPrompt;

uint8_t* psramAlloc(size_t bytes)
{
    return static_cast<uint8_t*>(heap_caps_malloc(bytes, MALLOC_CAP_SPIRAM | MALLOC_CAP_8BIT));
}

void logMemory(const char* when)
{
    Serial.printf("[Image] %s: internal free %u (largest block %u), PSRAM free %u\n", when,
                  unsigned(heap_caps_get_free_size(MALLOC_CAP_INTERNAL)),
                  unsigned(heap_caps_get_largest_free_block(MALLOC_CAP_INTERNAL)),
                  unsigned(heap_caps_get_free_size(MALLOC_CAP_SPIRAM)));
}

// "Katze im Weltall" -> "katze-im-weltall.jpg"
String filenameFor(const String& title)
{
    String name;
    for (size_t i = 0; i < title.length() && name.length() < 40; ++i) {
        const uint8_t c = title[i];
        if (isalnum(c)) name += char(tolower(c));
        else if (c == 0xC3 && i + 1 < title.length()) {  // UTF-8 umlauts
            const uint8_t next = title[++i];
            if (next == 0xA4 || next == 0x84) name += "ae";
            else if (next == 0xB6 || next == 0x96) name += "oe";
            else if (next == 0xBC || next == 0x9C) name += "ue";
            else if (next == 0x9F) name += "ss";
        } else if (name.length() && !name.endsWith("-")) name += '-';
    }
    while (name.endsWith("-")) name.remove(name.length() - 1);
    return (name.length() ? name : String("assistant-bild")) + ".jpg";
}

// --- Generation -----------------------------------------------------------

// One request to the Images API. Returns the JPEG in PSRAM (caller frees) or nullptr with
// `error` set; `status` is the HTTP status or a negative HTTPClient error.
uint8_t* requestImage(const String& prompt, bool landscape, size_t& length, int& status, String& error)
{
    JsonDocument body;
    body["model"] = IMAGE_MODEL;
    body["prompt"] = prompt;
    body["size"] = landscape ? IMAGE_SIZE_LANDSCAPE : IMAGE_SIZE;
    body["quality"] = IMAGE_QUALITY;
    body["output_format"] = "jpeg";
    body["output_compression"] = 85;
    PsramSink sink;
    status = openai::post("/v1/images/generations", body, sink);
    if (status != 200) {
        error = status == HTTPC_ERROR_READ_TIMEOUT ? String("Zeitueberschreitung beim Generieren")
                                                   : openai::errorMessage(status, sink);
        return nullptr;
    }
    // The body is ~300 KB, almost all of it the base64 image: find it without a JSON parser.
    const char* text = reinterpret_cast<const char*>(sink.data);
    const char* key = text ? strstr(text, "\"b64_json\"") : nullptr;
    const char* start = key ? strchr(key + 10, '"') : nullptr;
    const char* end = start ? strchr(start + 1, '"') : nullptr;
    if (!end) { error = "no image data in the answer"; return nullptr; }
    ++start;
    const size_t encoded = end - start;
    uint8_t* jpeg = psramAlloc(encoded / 4 * 3 + 3);
    if (!jpeg) { error = "not enough memory"; return nullptr; }
    if (mbedtls_base64_decode(jpeg, encoded / 4 * 3 + 3, &length,
                              reinterpret_cast<const uint8_t*>(start), encoded) != 0) {
        free(jpeg);
        error = "image data not decodable";
        return nullptr;
    }
    return jpeg;
}

// Retries only where no image was made (and billed): no connection (TLS handshake, e.g. short
// of internal RAM while other connections are open), rate limit, server errors. A timeout is
// not retried: the server may have finished the image anyway.
uint8_t* generate(const String& prompt, bool landscape, size_t& length, String& error)
{
    for (int attempt = 1;; ++attempt) {
        int status = 0;
        uint8_t* jpeg = requestImage(prompt, landscape, length, status, error);
        if (jpeg) return jpeg;
        const bool retry = status == HTTPC_ERROR_CONNECTION_REFUSED || status == 429 || status >= 500;
        Serial.printf("[Image] attempt %d: %s\n", attempt, error.c_str());
        if (!retry || attempt >= IMAGE_ATTEMPTS) return nullptr;
        delay(5000 * attempt);
    }
}

// --- Jobs -----------------------------------------------------------------

// Mailing or printing an image during a conversation dropped the GPT Live WebSocket every
// time ("Connection lost" a few seconds into the upload, even in 4 KB TLS records): the
// upload fills the network stack's send buffers, and a failed WebSocket write ends the
// connection. So the image is generated during the conversation but delivered after it.
void waitUntilNoConversation()
{
    if (!live::open()) return;
    Serial.println("[Image] done, delivery waits for the end of the conversation");
    const uint32_t since = millis();
    while (live::open() && millis() - since < MAX_SESSION_MS + THINKING_TIMEOUT_MS) delay(500);
    delay(1000);  // let the WebSocket close
}

void report(const Job& job, bool ok, const String& message)
{
    // Nobody may be looking at the display. After the conversation the assistant says it out loud,
    // during one the main loop tells the model, which tells the user.
    bool spoken = false;
    if (!ok && !live::open()) {
        String speech = "Sir, das Bild " + job.title + " hat leider nicht geklappt. " + message + ".";
        if (message.indexOf("Inhaltsfilter") >= 0)
            speech += " Geschützte Figuren, Marken oder echte Personen lehnt der Filter oft ab.";
        spoken = !agent::announce(speech).startsWith("Error");
    }
    auto* result = new Result{message, ok, spoken};
    if (xQueueSend(results, &result, 0) != pdTRUE) delete result;
    // After a mail command or a chat nobody sees the display: the owner learns of failures where
    // he waits for the image.
    if (!ok && job.telegram) {
        telegram::send("Das Bild „" + job.title + "“ hat nicht geklappt: " + message, job.person);
        return;
    }
    const bool ownerWaits = job.byMail || job.emailTo.equalsIgnoreCase(OWNER_EMAIL);
    if (!ok && ownerWaits && mail::configured())
        mail::send(OWNER_EMAIL, "Bild: " + job.title,
                   "<p>Das Bild &bdquo;" + job.title + "&ldquo; hat nicht geklappt: " + message + "</p>");
}

void run(Job job)
{
    const uint32_t started = millis();
    if (job.reuseLast) {
        if (!last) {
            report(job, false, "Seit dem letzten Neustart wurde noch kein Bild generiert");
            return;
        }
        job.title = lastTitle;
        job.prompt = lastPrompt;
    } else {
        logMemory("Start");
        String error;
        size_t length = 0;
        uint8_t* jpeg = generate(job.prompt, job.landscape, length, error);
        if (!jpeg) {
            report(job, false, "image not generated: " + error);
            return;
        }
        Serial.printf("[Image] %u bytes after %lu s\n", unsigned(length), (millis() - started) / 1000);
        free(last);
        last = jpeg;
        lastLength = length;
        lastLandscape = job.landscape;
        lastTitle = job.title;
        lastPrompt = job.prompt;
    }
    waitUntilNoConversation();
    // One background TLS job at a time; Telegram hands over its polling connection meanwhile.
    tls_gate::Hold gate;
    logMemory("Versand");

    bool ok = true;
    String done, error;
    if (job.print) {
        const int id = printer::printJpeg(last, lastLength, ASSISTANT_NAME ": " + job.title, job.color, lastLandscape, error);
        ok = id != 0;
        done = ok ? "printed (job " + String(id) + ")" : "printing failed: " + error;
    }
    if (job.emailTo.length()) {
        String html = "<p>Hier ist das Bild &bdquo;" + job.title + "&ldquo;.</p><p style=\"color:#888\">"
                      "Beschreibung: " + job.prompt + "</p>";
        const uint32_t mailStarted = millis();
        const String sent = mail::send(job.emailTo, job.title, html, last, lastLength, filenameFor(job.title));
        Serial.printf("[Image] mail: %s after %lu s\n", sent.c_str(), (millis() - mailStarted) / 1000);
        if (sent != "sent") ok = false;
        done += (done.length() ? ", " : "") +
                (sent == "sent" ? "mailed to " + job.emailTo : "mail failed: " + sent);
    }
    if (job.telegram) {
        const uint32_t sendStarted = millis();
        const String sent = telegram::sendPhoto(last, lastLength, job.title, job.person);
        Serial.printf("[Image] Telegram: %s after %lu s\n", sent.c_str(), (millis() - sendStarted) / 1000);
        if (sent != "sent") ok = false;
        done += (done.length() ? ", " : "") +
                (sent == "sent" ? String("sent by Telegram") : "Telegram failed: " + sent);
    }
    report(job, ok, "image \"" + job.title + "\" " + done + ", " + String((millis() - started) / 1000) + " s");
}

void worker(void*)
{
    for (;;) {
        Job* job = nullptr;
        if (xQueueReceive(jobs, &job, 0) == pdTRUE) {
            run(*job);
            delete job;
            --pending;
            continue;
        }
        // Queue empty: end the task (its stack is internal RAM, which TLS needs). queue()
        // queues first and checks the flag second, so no job is left behind.
        bool exit = false;
        taskENTER_CRITICAL(&taskMux);
        if (!uxQueueMessagesWaiting(jobs)) {
            taskRunning = false;
            exit = true;
        }
        taskEXIT_CRITICAL(&taskMux);
        if (exit) vTaskDelete(nullptr);
    }
}

String queue(Job* job)
{
    if (!jobs) {
        jobs = xQueueCreate(IMAGE_QUEUE_LENGTH, sizeof(Job*));
        results = xQueueCreate(IMAGE_QUEUE_LENGTH + 1, sizeof(Result*));
    }
    String error;
    if (!job->print && job->emailTo.isEmpty() && !job->telegram) error = "give print, email_to or telegram";
    else if (job->print && !printer::configured()) error = "no printer configured";
    else if (job->telegram && !telegram::configured()) error = "Telegram is not set up";
    else if (job->emailTo.length() && (!mail::configured() || job->emailTo.indexOf('@') < 1))
        error = "e-mail not possible (address invalid or no mailbox)";
    if (error.length()) {
        delete job;
        return "Error: " + error;
    }
    // Before queueing: from then on the job belongs to the image task.
    const bool generates = !job->reuseLast;
    String what;
    for (const String& way : {String(job->print ? "printed" : ""),
                              job->emailTo.length() ? "mailed to " + job->emailTo : String(),
                              String(job->telegram ? "sent by Telegram" : "")})
        if (way.length()) what += (what.length() ? " and " : "") + way;
    const int ahead = pending++;
    if (xQueueSend(jobs, &job, 0) != pdTRUE) {
        --pending;
        delete job;
        return "Error: " + String(IMAGE_QUEUE_LENGTH) + " image jobs are already waiting, please try again later.";
    }
    bool startTask = false;
    taskENTER_CRITICAL(&taskMux);
    if (!taskRunning) taskRunning = startTask = true;
    taskEXIT_CRITICAL(&taskMux);
    // TLS (OpenAI, printer, SMTP) needs a large stack.
    // CPU 1 like the main loop: on CPU 0 a long upload starves IDLE0 and the task watchdog
    // reboots the device (happened while mailing a 285 KB image).
    if (startTask && xTaskCreatePinnedToCore(worker, "image", TASK_STACK_BYTES, nullptr, 1, nullptr, 1) != pdPASS) {
        taskENTER_CRITICAL(&taskMux);
        taskRunning = false;
        taskEXIT_CRITICAL(&taskMux);
        return "Error: the image task could not start (not enough memory)";  // job stays queued
    }
    String answer = generates ? "The image is being generated now (about a minute) and then " + what + "."
                              : "The last image is being " + what + ".";
    if (ahead) answer += " Vorher ist noch ein anderer Bildauftrag dran.";
    if (live::open())
        answer += " Gedruckt bzw. verschickt wird erst nach dem Gespraech, damit die Verbindung stabil "
                  "bleibt: sag dem Nutzer, dass das Bild kommt, sobald ihr fertig seid.";
    return answer;
}

// Queues the job and returns what the model is told right away. `byMail`: nobody is at the
// speaker (agent), so failures are reported to the owner by mail.
String call(const char* name, JsonObjectConst arguments, bool byMail, const String& person)
{
    auto* job = new Job;
    job->person = person;
    job->reuseLast = !strcmp(name, "send_last_image");
    job->prompt = arguments["prompt"] | "";
    job->title = arguments["title"] | "Bild";
    job->print = arguments["print"] | false;
    job->color = arguments["color"] | false;
    job->landscape = arguments["landscape"] | false;
    job->emailTo = arguments["email_to"] | "";
    job->emailTo.trim();
    if (job->emailTo.equalsIgnoreCase("me")) {  // the asking person's address, the admin's by default
        people::Person asking;
        job->emailTo = people::find(person, asking) && asking.email.length() ? asking.email : String(OWNER_EMAIL);
    }
    job->telegram = arguments["telegram"] | false;
    job->byMail = byMail;
    if (!job->reuseLast && job->prompt.isEmpty()) {
        delete job;
        return "Error: no image description";
    }
    return queue(job);
}

}  // namespace

void addTools()
{
    if (!printer::configured() && !mail::configured() && !telegram::configured()) return;
    using tools::Tool;
    // The delivery parameters both tools share.
    String delivery =
        R"json("print": {"type": "boolean", "description": "Print the image"},
              "color": {"type": "boolean", "description": "Print in colour (true) or black and white (false)"},
              "email_to": {"type": "string", "description": "E-mail the image to this address, \"me\" = the user's own, \"\" = no mail"})json";
    String required = R"json("print", "color", "email_to")json";
    if (telegram::configured()) {
        delivery += R"json(,
              "telegram": {"type": "boolean", "description": "Send the image to the user's Telegram chat (his phone)"})json";
        required += R"json(, "telegram")json";
    }
    const bool chat = telegram::configured();
    // Both queue a background job (~30-60 s); the model gets told so. Outside a conversation
    // nobody is at the speaker: failures are then reported by mail (or Telegram).
    tools::add(Tool{
        "create_image",
        chat ? "Generates an image with AI, then prints it on the A4 printer at home, e-mails it as a JPEG "
               "attachment and/or sends it to the user's Telegram chat. Takes about a minute; returns right away "
               "and keeps running in the background. For colouring pages ask for black line art on white and set "
               "color to false. Never name protected characters, brands or real people in the prompt (the image "
               "model rejects them): describe the look instead. If the user did not say where it should go, ask."
             : "Generates an image with AI, then prints it on the A4 printer at home and/or e-mails it as a JPEG "
               "attachment. Takes about a minute; returns right away and keeps running in the background. For "
               "colouring pages ask for black line art on white and set color to false. Never name protected "
               "characters, brands or real people in the prompt (the image model rejects them): describe the look "
               "instead. If the user did not say where it should go, ask.",
        tools::keep(R"json({"type": "object", "additionalProperties": false, "properties": {)json" + delivery +
                    R"json(,
              "landscape": {"type": "boolean", "description": "Landscape format (Querformat: wider than high, printed sideways on A4); false = portrait"},
              "prompt": {"type": "string", "description": "Detailed image description, in the chosen format"},
              "title": {"type": "string", "description": "Short name (print job, mail subject, file name)"}},
            "required": [)json" + required + R"json(, "landscape", "prompt", "title"]})json"),
        [](JsonObjectConst arguments, const tools::Context& context) {
            return call("create_image", arguments, context.channel != tools::Voice, context.person);
        },
        0, tools::Everywhere});
    tools::add(Tool{
        "send_last_image",
        chat ? "Prints, e-mails and/or sends to Telegram the image generated last with create_image again, "
               "without generating a new one (e.g. 'druck das Bild auch noch aus', 'schick es auch an ...', "
               "'schick es mir aufs Handy'). If an image is still being generated, it takes that one."
             : "Prints and/or e-mails the image generated last with create_image again, without generating a new "
               "one (e.g. 'druck das Bild auch noch aus', 'schick es auch an ...'). If an image is still being "
               "generated, it takes that one.",
        tools::keep(R"json({"type": "object", "additionalProperties": false, "properties": {)json" + delivery +
                    R"json(}, "required": [)json" + required + R"json(]})json"),
        [](JsonObjectConst arguments, const tools::Context& context) {
            return call("send_last_image", arguments, context.channel != tools::Voice, context.person);
        },
        0, tools::Everywhere});
}


bool takeResult(String& message, bool& ok, bool& spoken)
{
    Result* result = nullptr;
    if (!results || xQueueReceive(results, &result, 0) != pdTRUE) return false;
    message = result->message;
    ok = result->ok;
    spoken = result->spoken;
    delete result;
    return true;
}

}  // namespace images
