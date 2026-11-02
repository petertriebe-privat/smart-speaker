#include "assistant/agenda.h"

#include <algorithm>
#include <map>
#include <vector>

#include "assistant/mail_watch.h"
#include "assistant/people.h"
#include "assistant/tools.h"
#include "config.h"
#include "prompts.h"
#include "services/google.h"
#include "services/todos.h"
#include "system/network.h"
#include "system/scoped_lock.h"
#include "system/time_util.h"
#include "system/tls_gate.h"
#include "system/utf8.h"
#include "voice/live_session.h"

namespace agenda {
namespace {

struct Item {
    String key;    // the event's or to-do's id
    String title;  // for spotting it in a transcript
    String line;   // for the model (an event as list_events shows it, a to-do's title)
    time_t start = 0, end = 0;  // a to-do: midnight of its due day
    bool allDay = false, info = false, todo = false;
    bool mail = false;  // a deadline from one of the person's mails (mailwatch::deadlines)
    String list;  // a to-do's list when there are several
};

std::vector<Item> items;            // events by start, then to-dos
std::vector<Item> mailItems;        // the mail deadlines in the agenda of the current conversation
bool fetched = false;
time_t fetchedTime = 0;             // wall clock of the last fetch, for the heading
String fetchedDay;                  // a new day fetches again (tomorrow's window moves)
uint32_t nextAt = 0;                // next regular fetch (millis)
volatile bool dirty = false;        // refreshSoon()
std::map<String, String> mentioned;  // key -> the day it came up in a conversation
std::map<String, String> offered;    // key -> the day it was offered before hanging up (closing())
std::vector<String> handed;          // keys in the agenda of the current conversation
std::vector<String> noticed;         // events passed on as a notice in the current conversation
time_t handedAt = 0;                 // when the current conversation got the agenda, 0 = none
SemaphoreHandle_t lock = xSemaphoreCreateMutex();

struct Locked : ScopedLock {
    Locked() : ScopedLock(lock) {}
};

// Local midnight `days` days after the day of `t`.
time_t dayStart(time_t t, int days)
{
    tm local;
    localtime_r(&t, &local);
    local.tm_hour = local.tm_min = local.tm_sec = 0;
    local.tm_mday += days;
    local.tm_isdst = -1;
    return mktime(&local);
}

// Local midnight of `day` (YYYY-MM-DD).
time_t midnightOf(const String& day)
{
    tm local{};
    sscanf(day.c_str(), "%4d-%2d-%2d", &local.tm_year, &local.tm_mon, &local.tm_mday);
    local.tm_year -= 1900;
    local.tm_mon -= 1;
    local.tm_isdst = -1;
    return mktime(&local);
}

bool allowed(const String& person, bool atHome)
{
    return person.length() ? person == people::admin() : atHome;
}

// --- Fetching (registry's worker, between conversations) ----------------------------------

void refresh()
{
    if (!timeutil::synced() || !network::connected() || live::open()) return;
    const bool newDay = fetched && fetchedDay != timeutil::today();
    if (!dirty && !newDay && int32_t(millis() - nextAt) < 0) return;
    tls_gate::TryHold gate;  // one background TLS job at a time; else again in a second
    if (!gate) return;
    dirty = false;
    fetchedDay = timeutil::today();
    const time_t now = time(nullptr);
    std::vector<google::Event> events;
    std::vector<todos::Todo> due;
    String error;
    const uint32_t started = millis();
    if (!google::events(dayStart(now, 0), dayStart(now, 2), events, error) ||
        !todos::due(timeutil::format(dayStart(now, 1), "%Y-%m-%d"), due, error)) {
        Serial.printf("[Agenda] %s\n", error.c_str());
        nextAt = millis() + AGENDA_RETRY_MS;
        return;
    }
    std::vector<Item> fresh;
    for (const google::Event& e : events) {
        Item item;
        item.key = e.id;
        item.title = e.title;
        item.line = e.line;
        item.start = e.start;
        item.end = e.end;
        item.allDay = e.allDay;
        item.info = e.info;
        fresh.push_back(item);
    }
    for (const todos::Todo& t : due) {
        Item item;
        item.key = t.id;
        item.title = item.line = t.title;
        item.todo = item.allDay = true;
        item.list = t.list;
        item.start = item.end = midnightOf(t.due);
        fresh.push_back(item);
    }
    Locked guard;
    items = fresh;
    fetched = true;
    fetchedTime = now;
    nextAt = millis() + AGENDA_REFRESH_MS;
    Serial.printf("[Agenda] %u events, %u to-dos due (%lu ms)\n", unsigned(events.size()), unsigned(due.size()),
                  millis() - started);
}

// --- For the prompts -------------------------------------------------------------------

// The item's line for `now` ("" once it is over); `order` sorts: overdue, today, tomorrow.
String describe(const Item& item, time_t now, int& order)
{
    const time_t today = dayStart(now, 0), tomorrow = dayStart(now, 1);
    if (item.mail) {
        order = item.start < tomorrow ? 0 : 1;
        return String("deadline ") + (order == 0 ? "today" : "tomorrow") + " from a mail (" + item.line + ")";
    }
    if (item.todo) {
        order = item.start < today ? -1 : item.start < tomorrow ? 0 : 1;
        const String when = order < 0   ? "overdue since " + timeutil::format(item.start, "%d.%m.")
                            : order == 0 ? String("due today")
                                         : String("due tomorrow");
        return "to-do " + when + ": " + item.line + (item.list.length() ? " (list " + item.list + ")" : String());
    }
    if (item.end <= now || (item.allDay && item.end <= today)) return "";
    order = item.start < tomorrow ? 0 : 1;
    const bool running = !item.allDay && item.start <= now;
    return String(order == 0 ? "today, " : "tomorrow, ") + item.line + (running ? " (running now)" : "");
}

// Whether `text` contains `word` (lower case), ignoring the case of ASCII letters; no copy of
// the transcript (up to MEMORY_TRANSCRIPT_CHARS) in the internal RAM.
bool contains(const String& text, const String& word)
{
    const size_t n = word.length();
    if (!n || text.length() < n) return false;
    for (size_t i = 0; i + n <= text.length(); ++i) {
        size_t j = 0;
        while (j < n && tolower(uint8_t(text[i + j])) == uint8_t(word[j])) ++j;
        if (j == n) return true;
    }
    return false;
}

// Whether a distinctive word of `title` (4 bytes or more, not a common one) occurs in `transcript`.
bool comesUp(const String& title, const String& transcript)
{
    static const char* const common[] = {"termin", "heute", "morgen", "noch", "beim", "aufgabe", "erinnerung",
                                         "with", "the", "und", "oder", "fuer", "für"};
    String word;
    auto check = [&]() {
        bool found = false;
        if (word.length() >= 4) {
            bool skip = false;
            for (const char* c : common) skip |= word == c;
            found = !skip && contains(transcript, word);
        }
        word = "";
        return found;
    };
    for (size_t i = 0; i < title.length(); ++i) {
        const uint8_t c = title[i];
        if (isalnum(c) || c >= 0x80) word += char(tolower(c));
        else if (check()) return true;
    }
    return check();
}

// The item with `key` in the calendar or the current conversation's mail deadlines, nullptr if gone.
// Caller holds the lock.
const Item* find(const String& key)
{
    for (const std::vector<Item>* list : {&items, &mailItems})
        for (const Item& item : *list)
            if (item.key == key) return &item;
    return nullptr;
}

// The deadlines of `reader`'s mails for today and tomorrow as items.
std::vector<Item> mailDeadlines(const String& reader, time_t now)
{
    std::vector<Item> found;
    if (reader.isEmpty() || !timeutil::synced()) return found;
    for (const mailwatch::Deadline& d : mailwatch::deadlines(reader, timeutil::format(dayStart(now, 0), "%Y-%m-%d"),
                                                              timeutil::format(dayStart(now, 1), "%Y-%m-%d"))) {
        Item item;
        item.key = d.key;
        item.title = d.subject;
        item.line = d.line;
        item.mail = item.allDay = true;
        item.start = item.end = midnightOf(d.day);
        found.push_back(item);
    }
    return found;
}

}  // namespace

String context(const String& person, bool atHome, bool voice)
{
    const time_t now = time(nullptr);
    // Deadlines from the mails of whoever it is for: their own mailboxes, nobody named at home = the admin.
    const std::vector<Item> mails = mailDeadlines(person.length() ? person : atHome ? people::admin() : String(), now);
    Locked guard;
    if (voice) {  // a new conversation: nothing of an earlier one carries over
        handed.clear();
        noticed.clear();
        handedAt = 0;
        mailItems = mails;
    }
    const bool calendar = allowed(person, atHome) && fetched;
    if (!calendar && mails.empty()) return "";
    const String today = timeutil::today();
    struct Line {
        int order;
        bool todo;
        time_t start;
        String text, key;
    };
    std::vector<Line> lines;
    auto add = [&](const Item& item) {
        int order = 0;
        const String text = describe(item, now, order);
        if (text.length())
            lines.push_back({order, item.todo || item.mail, item.start, utf8::prefix(text, AGENDA_LINE_CHARS), item.key});
    };
    if (calendar)
        for (const Item& item : items) add(item);
    for (const Item& item : mails) add(item);
    std::stable_sort(lines.begin(), lines.end(), [](const Line& a, const Line& b) {
        if (a.order != b.order) return a.order < b.order;
        if (a.todo != b.todo) return !a.todo;  // the day's events, then its to-dos
        return a.start < b.start;
    });
    String heading = AGENDA_HEADING;
    heading.replace("{time}", timeutil::format(calendar ? fetchedTime : now, "%H:%M"));
    if (voice) handedAt = now;
    if (lines.empty()) return heading + "(nothing in the calendar, no to-do due)\n";
    String fresh, known;
    size_t shown = 0;
    for (const Line& line : lines) {
        if (shown == AGENDA_MAX_LINES) break;
        ++shown;
        const bool already = voice && mentioned.count(line.key) && mentioned[line.key] == today;
        (already ? known : fresh) += "- " + line.text + "\n";
        if (voice) handed.push_back(line.key);
    }
    String text = heading;
    if (!voice) text += fresh;
    else {
        if (fresh.length()) text += String(AGENDA_NEW) + fresh;
        if (known.length()) text += String(AGENDA_MENTIONED) + known;
    }
    if (lines.size() > shown)
        text += "(and " + String(lines.size() - shown) + " more: list_events / list_todos)\n";
    return text;
}

void update()
{
    static uint32_t last = 0;
    if (millis() - last < 20000) return;
    last = millis();
    if (live::state() != live::State::Live || !live::privateFor(people::admin())) return;
    std::vector<std::pair<String, bool>> due;  // notice, urgent
    {
        Locked guard;
        if (!fetched || !handedAt) return;
        const time_t now = time(nullptr);
        for (const Item& item : items) {
            if (item.allDay || item.info || item.todo || item.start <= now) continue;
            const long ahead = item.start - now;
            // Already this close when the conversation started: it was in its agenda.
            if (ahead > AGENDA_SOON_MIN * 60L || item.start - handedAt <= AGENDA_SOON_MIN * 60L) continue;
            if (std::find(noticed.begin(), noticed.end(), item.key) != noticed.end()) continue;
            noticed.push_back(item.key);
            const long minutes = (ahead + 59) / 60;
            due.push_back({"Appointment at " + timeutil::format(item.start, "%H:%M") + " (in " + String(minutes) +
                               " minutes): " + utf8::prefix(item.line, AGENDA_LINE_CHARS),
                           minutes <= AGENDA_URGENT_MIN});
        }
    }
    for (const auto& n : due) live::notice(n.first, n.second);
}

String closing(const String& transcript)
{
    Locked guard;
    if (!handedAt) return "";
    const time_t now = time(nullptr);
    tm local;
    localtime_r(&now, &local);
    const String today = timeutil::today();
    String text;
    for (const String& key : handed) {
        const Item* item = find(key);
        if (!item || item->info) continue;
        int order = 0;
        const String line = describe(*item, now, order);
        if (line.isEmpty() || (order > 0 && local.tm_hour < AGENDA_EVENING_HOUR)) continue;
        if ((mentioned.count(key) && mentioned[key] == today) || (offered.count(key) && offered[key] == today)) continue;
        if (comesUp(item->title, transcript)) continue;
        offered[key] = today;
        text += "- " + utf8::prefix(line, AGENDA_LINE_CHARS) + "\n";
    }
    text.trim();
    return text;
}

void conversationOver(const String& transcript)
{
    Locked guard;
    if (handedAt) {
        const String today = timeutil::today();
        for (auto* days : {&mentioned, &offered})  // yesterday's are of no use
            for (auto it = days->begin(); it != days->end();)
                it = it->second == today ? std::next(it) : days->erase(it);
        std::vector<String> keys = handed;
        keys.insert(keys.end(), noticed.begin(), noticed.end());
        for (const String& key : keys) {
            const Item* item = find(key);
            if (item && comesUp(item->title, transcript)) {
                if (!mentioned.count(key)) Serial.printf("[Agenda] came up: %s\n", item->title.c_str());
                mentioned[key] = today;
            }
        }
    }
    handed.clear();
    noticed.clear();
    mailItems.clear();
    handedAt = 0;
}

void refreshSoon() { dirty = true; }

void addTools()
{
    if (!google::configured()) return;
    tools::addBackgroundWork(refresh);
}

void print()
{
    if (!fetched) {
        Serial.println("[Agenda] not fetched yet");
        return;
    }
    Serial.print(context(people::admin(), true, false));
    Locked guard;
    Serial.printf("[Agenda] %u items, %u came up today, next fetch in %ld s\n", unsigned(items.size()),
                  unsigned(mentioned.size()), long(int32_t(nextAt - millis())) / 1000);
}

}  // namespace agenda
