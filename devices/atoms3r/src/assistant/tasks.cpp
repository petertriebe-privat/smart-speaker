#include "assistant/tasks.h"

#include <LittleFS.h>
#include <esp_heap_caps.h>
#include <time.h>

#include "assistant/agent.h"
#include "assistant/memory.h"
#include "assistant/people.h"
#include "assistant/prompt.h"
#include "assistant/tools.h"
#include "config.h"
#include "prompts.h"
#include "assistant/backup.h"
#include "services/mail.h"
#include "services/psram_json.h"
#include "system/file_util.h"
#include "system/scoped_lock.h"
#include "system/time_util.h"
#include "system/tls_gate.h"
#include "voice/live_session.h"

namespace tasks {
namespace {

// A member of a network of devices (config.h MEMBER) keeps only its emergency tasks here, made
// while the leader was away (assistant/shared): a file of their own, no backup and no restore,
// so the tasks of a standalone past neither run twice nor come back.
constexpr const char* FILE_PATH = MEMBER ? "/tasks_member.json" : "/tasks.json";
constexpr const char* BACKUP_FILE = "tasks.json";          // in Drive
constexpr const char* BACKUP_SUBJECT = ASSISTANT_NAME "-Aufgaben";  // in the mailbox (MEMORY_FOLDER_CORE), not "Kern"
constexpr const char* NO_CLOCK = "Error: the clock is not set yet";
constexpr const char* WEEKDAYS[] = {"", "Mo", "Di", "Mi", "Do", "Fr", "Sa", "So"};

struct Job {
    int id;
    String kind, title, text, deliver;
    String person;  // who created it (assistant/people id): the run is theirs, results go to them
};

// {"next_id": 1, "tasks": [{"id", "title", "kind": "reminder"|"prompt", "text",
//   "deliver": "email"|"speak"|"both"|"telegram"|"none", "schedule": {"type", "time", "date", "days",
//   "day_of_month", "every_hours"}, "next": epoch, "last": epoch, "paused", "created", "person"}]}
JsonDocument doc(PsramJson::instance());  // lives as long as the device: not in internal RAM
SemaphoreHandle_t lock = xSemaphoreCreateMutex();  // main loop, task worker, mail agent
QueueHandle_t jobs = nullptr;
portMUX_TYPE taskMux = portMUX_INITIALIZER_UNLOCKED;
bool taskRunning = false;  // guarded by taskMux
volatile bool idle = false;
volatile bool backupPending = false, restorePending = false;
uint32_t backupRetryAt = 0;

bool backupOn() { return !MEMBER && ::backup::available(); }

struct Locked : ScopedLock {
    Locked() : ScopedLock(lock) {}
};


// --- Schedules ------------------------------------------------------------------

// Local time on day `day` (relative to `base`'s date) at hh:mm; mktime normalizes overflow and DST.
time_t atDay(time_t base, int dayOffset, int hour, int minute, int monthOffset = 0, int dayOfMonth = 0)
{
    tm t;
    localtime_r(&base, &t);
    if (monthOffset || dayOfMonth) {
        t.tm_mon += monthOffset;
        t.tm_mday = 1;
    }
    t.tm_mday += dayOffset;
    t.tm_hour = hour;
    t.tm_min = minute;
    t.tm_sec = 0;
    t.tm_isdst = -1;
    return mktime(&t);
}

bool parseTime(const char* text, int& hour, int& minute)
{
    return text && sscanf(text, "%d:%d", &hour, &minute) == 2 && hour >= 0 && hour < 24 && minute >= 0 && minute < 60;
}

int daysInMonth(int year, int month)  // month 0..11
{
    static const int days[] = {31, 28, 31, 30, 31, 30, 31, 31, 30, 31, 30, 31};
    const bool leap = (year % 4 == 0 && year % 100 != 0) || year % 400 == 0;
    return month == 1 && leap ? 29 : days[month];
}

// First run of `schedule` after `after`, 0 if there is none (a once-task in the past) or the
// schedule is incomplete (`error` says why).
time_t nextRun(JsonObjectConst schedule, time_t after, String& error)
{
    const String type = schedule["type"] | "";
    int hour = 0, minute = 0;
    const bool hasTime = parseTime(schedule["time"] | "", hour, minute);
    if (type == "interval") {
        const int hours = schedule["every_hours"] | 0;
        if (hours < 1 || hours > 24 * 31) {
            error = "every_hours must be between 1 and 744";
            return 0;
        }
        return after + time_t(hours) * 3600;
    }
    if (!hasTime) {
        error = "time missing (HH:MM)";
        return 0;
    }
    if (type == "once") {
        int year = 0, month = 0, day = 0;
        const char* date = schedule["date"] | "";
        if (!*date) {  // today, or tomorrow if the time has passed
            const time_t today = atDay(after, 0, hour, minute);
            return today > after ? today : atDay(after, 1, hour, minute);
        }
        if (sscanf(date, "%d-%d-%d", &year, &month, &day) != 3) {
            error = "date must be YYYY-MM-DD";
            return 0;
        }
        if (year > 2037) {  // time_t is 32 bits here (IDF 4): 2038 overflows
            error = "dates after 2037 are not possible";
            return 0;
        }
        tm t = {};
        t.tm_year = year - 1900;
        t.tm_mon = month - 1;
        t.tm_mday = day;
        t.tm_hour = hour;
        t.tm_min = minute;
        t.tm_isdst = -1;
        const time_t at = mktime(&t);
        return at > after ? at : 0;
    }
    if (type == "daily") {
        for (int d = 0; d <= 1; ++d) {
            const time_t at = atDay(after, d, hour, minute);
            if (at > after) return at;
        }
    }
    if (type == "weekly") {
        bool wanted[8] = {};
        for (JsonVariantConst day : schedule["days"].as<JsonArrayConst>()) {
            const int d = day | 0;
            if (d >= 1 && d <= 7) wanted[d] = true;
        }
        for (int d = 0; d <= 7; ++d) {
            const time_t at = atDay(after, d, hour, minute);
            tm t;
            localtime_r(&at, &t);
            const int weekday = t.tm_wday == 0 ? 7 : t.tm_wday;  // 1 = Monday
            if (at > after && wanted[weekday]) return at;
        }
        error = "days missing (1 = Monday ... 7 = Sunday)";
        return 0;
    }
    if (type == "monthly") {
        const int wantedDay = schedule["day_of_month"] | 0;
        if (wantedDay < 1 || wantedDay > 31) {
            error = "day_of_month must be 1 to 31";
            return 0;
        }
        for (int m = 0; m <= 12; ++m) {
            const time_t first = atDay(after, 0, hour, minute, m, 1);
            tm t;
            localtime_r(&first, &t);
            const int day = min(wantedDay, daysInMonth(t.tm_year + 1900, t.tm_mon));
            const time_t at = atDay(first, day - 1, hour, minute);
            if (at > after) return at;
        }
    }
    error = "unknown schedule (once, daily, weekly, monthly, interval)";
    return 0;
}

String describeSchedule(JsonObjectConst schedule)
{
    const String type = schedule["type"] | "";
    const String time = schedule["time"] | "";
    if (type == "interval") return "alle " + String(schedule["every_hours"] | 0) + " Stunden";
    if (type == "daily") return "täglich " + time;
    if (type == "weekly") {
        String days;
        for (JsonVariantConst day : schedule["days"].as<JsonArrayConst>()) {
            const int d = day | 0;
            if (d >= 1 && d <= 7) days += (days.length() ? ", " : "") + String(WEEKDAYS[d]);
        }
        return days + " " + time;
    }
    if (type == "monthly") return "monatlich am " + String(schedule["day_of_month"] | 0) + ". um " + time;
    return "einmalig";
}

String describeTask(JsonObjectConst task)
{
    static const char* const deliveries[] = {"email", "per Mail", "speak", "als Ansage", "both",
                                             "per Mail und Ansage", "telegram", "per Telegram",
                                             "none", "ohne Meldung"};
    String deliver;
    for (int i = 0; i < 10; i += 2)
        if (!strcmp(task["deliver"] | "", deliveries[i])) deliver = deliveries[i + 1];
    const bool reminder = !strcmp(task["kind"] | "", "reminder");
    const String person = people::name(task["person"] | "");
    String text = "#" + String(task["id"].as<int>()) + " " + (task["title"] | "") + " (" +
                  describeSchedule(task["schedule"]) + ", " +
                  (!reminder                                         ? deliver
                   : !strcmp(task["deliver"] | "", "speak")    ? String("Erinnerung als Ansage")
                   : !strcmp(task["deliver"] | "", "both")     ? String("Erinnerung als Ansage und Nachricht")
                   : !strcmp(task["deliver"] | "", "call")     ? String("Erinnerung per Anruf")
                                                                     : String("Erinnerung per Nachricht")) +
                  (person.length() ? ", für " + person : String()) + ")";
    if (task["paused"] | false) text += ", pausiert";
    else if (task["next"] | 0LL) {
        const time_t next = task["next"].as<time_t>();
        tm t;
        localtime_r(&next, &t);
        const time_t now = time(nullptr);
        tm today;
        localtime_r(&now, &today);
        text += ", nächster Lauf " + String(WEEKDAYS[t.tm_wday == 0 ? 7 : t.tm_wday]) + " " +
                timeutil::format(next, t.tm_year == today.tm_year ? "%d.%m. %H:%M" : "%d.%m.%Y %H:%M");
    }
    return text;
}

// --- Storage (caller holds the lock) ---------------------------------------------

void save()
{
    if (!fileutil::saveJson(FILE_PATH, doc)) Serial.println("[Tasks] saving failed");
    backupPending = backupOn() && !restorePending;  // an incomplete list must not replace the backup
}

JsonObject findTask(int id)
{
    for (JsonObject task : doc["tasks"].as<JsonArray>())
        if (task["id"].as<int>() == id) return task;
    return JsonObject();
}

bool removeTask(int id)
{
    JsonArray list = doc["tasks"];
    for (size_t i = 0; i < list.size(); ++i) {
        if (list[i]["id"].as<int>() != id) continue;
        list.remove(i);
        return true;
    }
    return false;
}

void normalize()
{
    if (!doc["next_id"].is<int>()) doc["next_id"] = 1;
    if (!doc["tasks"].is<JsonArray>()) doc["tasks"].to<JsonArray>();
}

// --- Worker: runs tasks and the backup, then ends (its stack is internal RAM) -------

void runJob(const Job& job)
{
    // The agent's TLS next to a conversation's: wait until it is over (at most half an hour).
    for (uint32_t waited = 0; job.kind != "reminder" && !idle && waited < 1800; ++waited) delay(1000);
    tls_gate::Hold gate;  // one background TLS job at a time (the agent's tools do not take it)
    const uint32_t started = millis();
    if (job.kind == "reminder") {
        // Planned reminders say so (and for whom, when not the admin's); announcements from Home
        // Assistant (id 0) speak for themselves.
        const String name = job.person != people::admin() ? people::name(job.person) : String();
        const String text =
            job.id ? (name.length() ? "Erinnerung für " + name + ": " : String("Erinnerung: ")) + job.text : job.text;
        // call: The assistant phones the person and tells it (very important ones); a failed call becomes
        // a message
        if (job.deliver == "call") {
            JsonDocument arguments;
            arguments["to"] = people::name(job.person);
            arguments["task"] = "Erinnerung, die jetzt fällig ist; sag sie dem Nutzer: " + job.text;
            String json;
            serializeJson(arguments, json);
            const String out = tools::run("phone_call", json.c_str(), tools::Agent, job.person);
            Serial.printf("[Tasks] #%d reminder: call %.120s\n", job.id, out.c_str());
            if (!out.startsWith("Error")) return;
        }
        // telegram (the default): a message to the person (WhatsApp for whom has no Telegram).
        // The assistant cannot know which room they are in: announced only on their own words, 27.10.2026.
        // A failed message is announced here rather than lost.
        if (job.deliver != "speak") {
            const String via = people::message(job.person, text);
            Serial.printf("[Tasks] #%d reminder: message %s\n", job.id, via.c_str());
            if (job.deliver != "both" && !via.startsWith("Error")) return;
        }
        const String result = agent::announce(text);
        Serial.printf("[Tasks] #%d reminder: %s\n", job.id, result.c_str());
        return;
    }
    String delivery;
    people::Person owner;
    const String email = people::find(job.person, owner) && owner.email.length() ? owner.email : String(OWNER_EMAIL);
    if (job.deliver == "email" || job.deliver == "both")
        delivery += "Send the result with send_email to " + email + ", subject: " + job.title + ". ";
    if (job.deliver == "speak" || job.deliver == "both")
        delivery += "Announce the result with speak on the speaker (short, spoken German). ";
    if (job.deliver == "telegram")
        delivery += "Send the result with send_telegram to the user's Telegram chat (short, bold title, links as "
                    "<a href>). ";
    if (job.deliver == "call")
        delivery += "Tell the result by phone: phone_call with to = " +
                    (job.person.length() ? people::name(job.person) : String("''")) + " and the result as task. ";
    if (job.deliver == "none") delivery = "Report nothing, only carry out the instruction. ";
    const String task = "Scheduled task \"" + job.title + "\" is due.\nInstruction: " + job.text +
                        "\nDelivery: " + delivery;
    const String answer = agent::run(task, TASK_INSTRUCTIONS, nullptr, "", job.person, nullptr, "geplante Aufgabe");
    Serial.printf("[Tasks] #%d %s done after %lu s: %.200s\n", job.id, job.title.c_str(),
                  (millis() - started) / 1000, answer.c_str());
}

void backup()
{
    String text;
    JsonDocument copy(PsramJson::instance());
    {
        Locked guard;
        text = "Geplante Aufgaben von " ASSISTANT_NAME ", Stand " + timeutil::format(time(nullptr), "%d.%m.%Y %H:%M") +
               ". Stellt sie nach einem Gerätewechsel wieder her; bitte nicht bearbeiten.\n\n";
        for (JsonObjectConst task : doc["tasks"].as<JsonArrayConst>())
            text += "- " + describeTask(task) + ": " + (task["text"] | "") + "\n";
        copy = doc;
        backupPending = false;
    }
    const String result = ::backup::store(BACKUP_FILE, BACKUP_SUBJECT, text, copy);
    Serial.printf("[Tasks] backup: %s\n", result.c_str());
    if (result != "stored") {
        backupPending = true;
        backupRetryAt = millis() + MEMORY_OUTBOX_RETRY_MS;
    }
}

void restore()
{
    String error;
    JsonDocument backup(PsramJson::instance());
    if (!::backup::read(BACKUP_FILE, BACKUP_SUBJECT, backup, error)) {
        Serial.printf("[Tasks] restore: %s\n", error.c_str());
        backupRetryAt = millis() + MEMORY_OUTBOX_RETRY_MS;
        return;
    }
    restorePending = false;
    if (backup.isNull()) return;
    Locked guard;
    if (doc["tasks"].size()) {  // created meanwhile: those count, the list becomes the backup
        backupPending = backupOn();
        return;
    }
    doc = backup;
    normalize();
    const time_t now = time(nullptr);
    for (JsonObject task : doc["tasks"].as<JsonArray>()) {  // runs missed meanwhile are skipped
        String ignored;
        if ((task["next"] | 0LL) < now) task["next"] = nextRun(task["schedule"], now, ignored);
    }
    save();
    backupPending = false;
    Serial.printf("[Tasks] restored from the mailbox: %u\n", unsigned(doc["tasks"].size()));
}

bool backupDue() { return idle && (backupPending || restorePending) && int32_t(millis() - backupRetryAt) >= 0; }

void worker(void*)
{
    for (;;) {
        Job* job = nullptr;
        if (xQueueReceive(jobs, &job, pdMS_TO_TICKS(500)) == pdTRUE) {
            runJob(*job);
            delete job;
            continue;
        }
        if (backupDue()) {
            tls_gate::Hold gate;
            if (restorePending) restore();
            else backup();
            continue;
        }
        bool done = false;
        taskENTER_CRITICAL(&taskMux);
        if (!uxQueueMessagesWaiting(jobs) && !backupDue()) {
            taskRunning = false;
            done = true;
        }
        taskEXIT_CRITICAL(&taskMux);
        if (done) vTaskDelete(nullptr);
    }
}

void startTask()
{
    bool start = false;
    taskENTER_CRITICAL(&taskMux);
    if (!taskRunning) taskRunning = start = true;
    taskEXIT_CRITICAL(&taskMux);
    if (!start) return;
    // Like the mail task: the agent's TLS, JSON and ReadyMail need a large stack; CPU 1.
    static bool failed = false;  // logged once until it starts again
    if (xTaskCreatePinnedToCore(worker, "tasks", TASK_STACK_BYTES, nullptr, 1, nullptr, 1) != pdPASS) {
        taskENTER_CRITICAL(&taskMux);
        taskRunning = false;
        taskEXIT_CRITICAL(&taskMux);
        if (!failed)
            Serial.printf("[Tasks] task could not start (largest block %u), trying again every second\n",
                          unsigned(heap_caps_get_largest_free_block(MALLOC_CAP_INTERNAL)));
        failed = true;
    } else if (failed) {
        failed = false;
        Serial.println("[Tasks] task runs again");
    }
}

bool queue(Job* job)
{
    if (!jobs || xQueueSend(jobs, &job, 0) != pdTRUE) {
        delete job;
        return false;
    }
    startTask();
    return true;
}

bool queue(JsonObjectConst task)
{
    return queue(new Job{task["id"].as<int>(), task["kind"] | "", task["title"] | "", task["text"] | "",
                         task["deliver"] | "", task["person"] | ""});
}

void reminderInConversation(const String& text)
{
    live::notice("A reminder or message that is due now: " + text, true);
}

// --- Tools -----------------------------------------------------------------------

// The backend fills unused optional parameters with empty values ("" / 0 / []): drop them.
void copyIfSet(JsonObject to, JsonObjectConst from, const char* key)
{
    JsonVariantConst value = from[key];
    if (value.isNull() || (value.is<const char*>() && !strlen(value | "")) || (value.is<int>() && !(value | 0)) ||
        (value.is<JsonArrayConst>() && !value.size()))
        return;
    to[key] = value;
}

String createTask(JsonObjectConst arguments, const String& person)
{
    const String title = arguments["title"] | "";
    const String kind = arguments["kind"] | "";
    const String text = arguments["text"] | "";
    if (title.isEmpty() || text.isEmpty()) return "Error: give title and text";
    if (kind != "reminder" && kind != "prompt") return "Error: kind is reminder or prompt";
    JsonDocument schedule;
    schedule["type"] = arguments["schedule"] | "";
    for (const char* key : {"time", "date", "days", "day_of_month", "every_hours"})
        copyIfSet(schedule.as<JsonObject>(), arguments, key);
    const time_t now = time(nullptr);
    const int inMinutes = arguments["in_minutes"] | 0;
    if (inMinutes > 0 && schedule["type"] == "once") {  // "in 10 Minuten"
        const time_t at = now + time_t(inMinutes) * 60;
        schedule["date"] = timeutil::format(at, "%Y-%m-%d");
        schedule["time"] = timeutil::format(at, "%H:%M");
    }
    String error;
    const time_t next = nextRun(schedule.as<JsonObjectConst>(), now, error);
    if (!next) return "Error: " + (error.length() ? error : String("the time is in the past"));

    Locked guard;
    if (doc["tasks"].size() >= size_t(TASK_MAX)) return "Error: already " + String(TASK_MAX) + " tasks, delete one first";
    const int id = doc["next_id"].as<int>();
    doc["next_id"] = id + 1;
    JsonObject task = doc["tasks"].as<JsonArray>().add<JsonObject>();
    task["id"] = id;
    task["title"] = title;
    task["kind"] = kind;
    task["text"] = text;
    const String deliver = arguments["deliver"] | "";
    // a reminder goes to the phone unless the user names this speaker: The assistant cannot know the room
    task["deliver"] = kind == "reminder" ? (deliver == "speak" || deliver == "both" || deliver == "call" ? deliver
                                                                                                         : String("telegram"))
                      : deliver.length()  ? deliver
                                          : String("email");
    task["schedule"] = schedule;
    task["next"] = next;
    task["paused"] = false;
    task["created"] = timeutil::format(now, "%Y-%m-%d");
    if (person.length()) task["person"] = person;
    save();
    Serial.printf("[Tasks] created: %s\n", describeTask(task).c_str());
    return "Created: " + describeTask(task);
}

String listTasks()
{
    Locked guard;
    if (!doc["tasks"].size()) return "No scheduled tasks.";
    String text;
    for (JsonObjectConst task : doc["tasks"].as<JsonArrayConst>())
        text += describeTask(task) + ": " + (task["text"] | "") + "\n";
    return text;
}

String deleteTask(int id)
{
    Locked guard;
    if (!removeTask(id)) return "Error: no task #" + String(id);
    save();
    return "Task #" + String(id) + " deleted.";
}

String pauseTask(int id, bool paused)
{
    Locked guard;
    JsonObject task = findTask(id);
    if (!task) return "Error: no task #" + String(id);
    task["paused"] = paused;
    if (!paused) {  // continue from now on, no catching up
        String ignored;
        task["next"] = nextRun(task["schedule"], time(nullptr), ignored);
    }
    save();
    return (paused ? "Paused: " : "Running again: ") + describeTask(task);
}

String runTask(int id)
{
    Locked guard;
    JsonObject task = findTask(id);
    if (!task) return "Error: no task #" + String(id);
    if (!queue(task)) return "Error: too many tasks at once";
    return "Task #" + String(id) + " runs now" +
           (live::open() && strcmp(task["kind"] | "", "reminder") ? " (once the conversation is over)." : ".");
}

}  // namespace

void begin()
{
    jobs = xQueueCreate(4, sizeof(Job*));
    Locked guard;
    File file = LittleFS.exists(FILE_PATH) ? LittleFS.open(FILE_PATH, "r") : File();
    if (!file || deserializeJson(doc, file)) doc.clear();
    normalize();
    restorePending = !file && backupOn();
    if (file && backupOn() && ::backup::due(BACKUP_FILE)) backupPending = true;  // the first backup in Drive
    Serial.printf("[Tasks] %u scheduled%s\n", unsigned(doc["tasks"].size()),
                  restorePending ? ", looking for the backup" : "");
}

void update()
{
    static uint32_t lastCheck = 0;
    if (millis() - lastCheck < 1000 || !jobs) return;
    lastCheck = millis();
    // Queued jobs whose worker could not start (right after a phone call the memory's worker and
    // TLS take the internal RAM; on 28.09. two call reports waited silently for the next task).
    if (uxQueueMessagesWaiting(jobs) && !taskRunning) startTask();
    if (!timeutil::synced()) return;
    const time_t now = time(nullptr);
    Locked guard;
    bool changed = false;
    JsonArray list = doc["tasks"];
    for (size_t i = 0; i < list.size();) {
        JsonObject task = list[i];
        const time_t due = task["next"] | 0LL;
        if ((task["paused"] | false) || !due || due > now) {
            ++i;
            continue;
        }
        const bool reminder = !strcmp(task["kind"] | "", "reminder");
        const char* deliver = task["deliver"] | "";
        // a reminder by message or call needs no quiet
        const bool aloud = reminder && (!strcmp(deliver, "speak") || !strcmp(deliver, "both"));
        if (now - due > TASK_CATCHUP_S) {
            Serial.printf("[Tasks] #%d missed (%s), skipped\n", task["id"].as<int>(),
                          timeutil::format(due, "%d.%m. %H:%M").c_str());
        } else if (aloud && live::state() == live::State::Live) {
            reminderInConversation(task["text"] | "");  // in a conversation the assistant says it itself
        } else if ((aloud && live::open()) || (!reminder && (live::open() || !idle))) {
            ++i;  // after the conversation
            continue;
        } else if (!queue(task)) {
            ++i;  // queue full: next second
            continue;
        }
        task["last"] = now;
        String ignored;
        const time_t next = strcmp(task["schedule"]["type"] | "", "once") ? nextRun(task["schedule"], now, ignored) : 0;
        changed = true;
        if (next) {
            task["next"] = next;
            ++i;
        } else {
            list.remove(i);  // once-tasks are done
        }
    }
    if (changed) save();
}

void setIdle(bool on)
{
    idle = on;
    if (on && jobs && backupDue()) startTask();
}

void runNow(const char* kind, const String& title, const String& text, const String& deliver, const String& person)
{
    if (text.isEmpty()) return;
    if (!strcmp(kind, "reminder") && live::state() == live::State::Live) {
        reminderInConversation(text);
        return;
    }
    if (!queue(new Job{0, kind, title, text, deliver.length() ? deliver : String("speak"), person}))
        Serial.println("[Tasks] queue full, event dropped");
}

bool exists(int id)
{
    Locked guard;
    return id && !findTask(id).isNull();
}

String context()
{
    Locked guard;
    if (!doc["tasks"].size()) return "";
    String text = "Scheduled tasks:\n";
    for (JsonObjectConst task : doc["tasks"].as<JsonArrayConst>()) text += "- " + describeTask(task) + "\n";
    return text;
}

void addTools()
{
    using tools::Tool;  // everything here needs the clock (schedules, next runs): NO_CLOCK before NTP
    tools::add(Tool{
        "create_task",
        "Plans a task for later or on a schedule, after the user confirmed it. kind reminder: text is "
        "sent to the person's phone as a message at the time (deliver telegram, the default: you do not know "
        "which room they will be in); announced on this speaker only when they ask for it (deliver speak: an "
        "alarm clock, 'sag es mir hier'); deliver call (a phone call) when they ask or it is very important. "
        "kind prompt: text is an instruction the assistant carries "
        "out at the time with all its tools (news, weather, web search, smart home, mail) and delivers as "
        "deliver says; a condition in the instruction ('only if it rains') lets it stay silent otherwise. "
        "One-off or delayed device actions ('in 10 minutes switch the light on') are a prompt task with "
        "deliver none; recurring device control belongs in create_home_automation. The instruction names the "
        "tool for then: wake-up with music = play_music at low volume; 'ruf mich um 7 an' = phone_call; "
        "'weck mich hier' = start_conversation (deliver none, until they say they are awake); a sleep timer = "
        "music_control pause; 'erinnere mich rechtzeitig loszufahren' = an hour before, travel_time, then say "
        "when to leave. 'In 10 Minuten' is in_minutes. To change a task: delete_task, then create_task.",
        R"json({"type": "object", "additionalProperties": false,
            "properties": {
              "title": {"type": "string", "description": "short German title, e.g. 'Tech-News'"},
              "kind": {"type": "string", "enum": ["reminder", "prompt"]},
              "text": {"type": "string", "description": "reminder: the exact German sentence the assistant will say, addressed to the user (e.g. 'Die Performance Messe ist am Samstag, 4. September, in Metzingen.'); prompt: the complete instruction in German"},
              "deliver": {"type": "string", "enum": ["email", "speak", "both", "telegram", "call", "none"], "description": "prompt tasks: email to the owner, speak (announced on this speaker), both (email and announcement), a message to the person's chat, call (told by phone) or none (only act). reminders: telegram (default, a message to the person's phone), speak (announced here), both (announced and messaged) or call (told by phone)"},
              "schedule": {"type": "string", "enum": ["once", "daily", "weekly", "monthly", "interval"]},
              "time": {"type": "string", "description": "HH:MM (24 h), all but interval"},
              "date": {"type": "string", "description": "once: YYYY-MM-DD, '' = today or tomorrow"},
              "in_minutes": {"type": "integer", "description": "once: in this many minutes instead of date/time"},
              "days": {"type": "array", "items": {"type": "integer"}, "description": "weekly: 1 = Monday … 7 = Sunday"},
              "day_of_month": {"type": "integer"},
              "every_hours": {"type": "integer", "description": "interval: hours between runs"}},
            "required": ["title", "kind", "text", "schedule"]})json",
        [](JsonObjectConst arguments, const tools::Context& context) {
            return timeutil::synced() ? createTask(arguments, context.person) : String(NO_CLOCK);
        },
        0, tools::Everywhere});
    tools::add(Tool{
        "list_tasks", "Lists the planned tasks with id, schedule and next run.",
        R"json({"type": "object", "additionalProperties": false, "properties": {}, "required": []})json",
        [](JsonObjectConst, const tools::Context&) { return timeutil::synced() ? listTasks() : String(NO_CLOCK); },
        0, tools::Everywhere});
    tools::add(Tool{
        "delete_task", "Deletes a planned task by id.",
        R"json({"type": "object", "additionalProperties": false, "properties": {"id": {"type": "integer"}}, "required": ["id"]})json",
        [](JsonObjectConst arguments, const tools::Context&) {
            return timeutil::synced() ? deleteTask(arguments["id"] | 0) : String(NO_CLOCK);
        },
        0, tools::Everywhere});
    tools::add(Tool{
        "pause_task", "Pauses a planned task (paused true) or resumes it (false).",
        R"json({"type": "object", "additionalProperties": false,
            "properties": {"id": {"type": "integer"}, "paused": {"type": "boolean"}}, "required": ["id", "paused"]})json",
        [](JsonObjectConst arguments, const tools::Context&) {
            return timeutil::synced() ? pauseTask(arguments["id"] | 0, arguments["paused"] | true) : String(NO_CLOCK);
        },
        0, tools::Everywhere});
    tools::add(Tool{
        "run_task", "Runs a planned task right now once (e.g. to try it out).",
        R"json({"type": "object", "additionalProperties": false, "properties": {"id": {"type": "integer"}}, "required": ["id"]})json",
        [](JsonObjectConst arguments, const tools::Context&) {
            return timeutil::synced() ? runTask(arguments["id"] | 0) : String(NO_CLOCK);
        },
        0, tools::Everywhere});
}

}  // namespace tasks
