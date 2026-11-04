#include "services/presence.h"

#include <ArduinoJson.h>
#include <vector>

#include "assistant/people.h"
#include "assistant/tools.h"
#include "config.h"
#include "services/home_assistant.h"
#include "services/maps.h"
#include "services/psram_json.h"
#include "system/network.h"
#include "system/scoped_lock.h"
#include "system/time_util.h"

namespace presence {
namespace {

struct Entry {
    String person;  // assistant/people id
    String state;   // "home", "not_home", a zone's name
    String since;   // local time of the last change, "HH:MM" today or "DD.MM. HH:MM"
};

std::vector<Entry> entries;  // guarded by lock
uint32_t fetchedAt = 0;      // millis() of the last successful fetch, 0 = never
time_t fetchedTime = 0;      // wall clock of it
SemaphoreHandle_t lock = xSemaphoreCreateMutex();

struct Locked : ScopedLock {
    Locked() : ScopedLock(lock) {}
};

bool fresh() { return fetchedAt && millis() - fetchedAt < PRESENCE_STALE_MS; }

// "2026-09-29T05:10:03.123+00:00" (UTC) -> local "05:10" today, else "28.09. 19:32".
String localTime(const String& iso)
{
    int y, m, d, hour, minute, second;
    if (sscanf(iso.c_str(), "%d-%d-%dT%d:%d:%d", &y, &m, &d, &hour, &minute, &second) != 6) return "";
    // Days since 1970-01-01 of a UTC date (Howard Hinnant's days_from_civil): mktime would take it
    // for local time.
    y -= m <= 2;
    const int era = (y >= 0 ? y : y - 399) / 400;
    const int yearOfEra = y - era * 400;
    const int dayOfYear = (153 * (m + (m > 2 ? -3 : 9)) + 2) / 5 + d - 1;
    const int dayOfEra = yearOfEra * 365 + yearOfEra / 4 - yearOfEra / 100 + dayOfYear;
    const long days = long(era) * 146097 + dayOfEra - 719468;
    const time_t at = time_t(days) * 86400 + hour * 3600 + minute * 60 + second;
    const bool today = timeutil::format(at, "%Y-%m-%d") == timeutil::today();
    return timeutil::format(at, today ? "%H:%M" : "%d.%m. %H:%M");
}

String firstName(const String& s)
{
    int end = 0;
    while (end < int(s.length()) && s[end] != '_' && s[end] != ' ') end++;
    return s.substring(0, end);
}

// person.alex or its name "Alex" -> the register's id; then the first names of both
// (person.alex_smith, "Alex Smith" -> alex: HA names a person by its user's full name, and a new
// instance with person.<first>_<last> found nobody, 20.10.2026). As the Echo's presence.go.
String matchPerson(JsonObjectConst entity)
{
    const String id = String(entity["entity_id"] | "").substring(7);
    const String friendly = entity["attributes"]["friendly_name"] | "";
    for (const String& name : {id, friendly, firstName(id), firstName(friendly)}) {
        if (name.isEmpty()) continue;
        const String person = people::byName(name);
        if (person.length()) return person;
    }
    return "";
}

// Returns "" or an error text.
String fetchNow()
{
    JsonDocument states(PsramJson::instance());
    const int status = ha::states("person", "", 50, states);  // not /api/states (#85)
    if (status != 200) return ha::failure(status, states);
    std::vector<Entry> found;
    for (JsonObjectConst entity : states.as<JsonArrayConst>()) {
        const String id = entity["entity_id"] | "";
        if (!id.startsWith("person.")) continue;
        const String person = matchPerson(entity);
        const String state = entity["state"] | "";
        if (person.length() && state.length() && state != "unknown" && state != "unavailable")
            found.push_back({person, state, localTime(entity["last_changed"] | "")});
    }
    Locked guard;
    for (const Entry& entry : found) {  // changes in the log, not every minute
        bool same = false;
        for (const Entry& old : entries) same |= old.person == entry.person && old.state == entry.state;
        if (!same) Serial.printf("[Anwesenheit] %s: %s\n", entry.person.c_str(), entry.state.c_str());
    }
    entries = found;
    fetchedAt = millis();
    fetchedTime = time(nullptr);
    return "";
}

// Where exactly a person is: the HA app on their phone reports the GPS position as attributes of
// the person entity; Google turns it into an address (maps::address). At home no address is
// looked up.
String locate(const String& who)
{
    const String person = people::byName(who);
    if (person.isEmpty()) return "Error: " + who + " is not a person of the household (list_people).";
    JsonDocument states(PsramJson::instance());
    const int status = ha::states("person", "", 50, states);  // not /api/states (#85)
    if (status != 200) return ha::failure(status, states);
    const String name = people::name(person);
    for (JsonObjectConst entity : states.as<JsonArrayConst>()) {
        const String id = entity["entity_id"] | "";
        if (!id.startsWith("person.")) continue;
        if (matchPerson(entity) != person) continue;
        const String state = entity["state"] | "";
        const String since = localTime(entity["last_changed"] | "");
        JsonObjectConst attributes = entity["attributes"];
        if (state == "home") return name + " is at home (since " + since + ").";
        if (!attributes["latitude"].is<double>() || !attributes["longitude"].is<double>())
            return name + ": Home Assistant shows \"" + state + "\" since " + since +
                   ", but no position (the HA app with location access on their phone sends it).";
        const double latitude = attributes["latitude"], longitude = attributes["longitude"];
        String text = name + ": " + (state == "not_home" ? String("away") : "in the zone \"" + state + "\"") +
                      " since " + since + "; position of " + localTime(entity["last_updated"] | "");
        const int accuracy = attributes["gps_accuracy"] | 0;
        if (accuracy > 0) text += " (accurate to " + String(accuracy) + " m)";
        String error;
        const String address = maps::address(latitude, longitude, error);
        text += address.length() ? ": " + address : "; address unknown (" + error + "), coordinates " +
                                                        String(latitude, 5) + ", " + String(longitude, 5);
        const double km = maps::kmFromHome(latitude, longitude);
        if (km >= 0) text += "; " + String(km, 1) + " km from home (straight line)";
        return text + ". For the way home use travel_time with this address as origin.";
    }
    return "Error: Home Assistant has no person for " + name + " (a person with the HA app on their phone, named " +
           person + " or " + name + ").";
}

// Background work of the registry's worker (about once a second while no call waits).
void fetch()
{
    static uint32_t lastTry = 0;
    static int failures = 0;
    if ((lastTry && millis() - lastTry < PRESENCE_POLL_MS) || !network::connected()) return;
    lastTry = millis();
    const String error = fetchNow();
    if (error.isEmpty()) failures = 0;
    else if (++failures == 1 || failures % 30 == 0) Serial.printf("[Anwesenheit] %s\n", error.c_str());
}

}  // namespace

void begin()
{
    if (!ha::configured()) return;
    tools::addBackgroundWork(fetch);
    tools::add(tools::Tool{
        "get_presence",
        "Who of the household is at home right now, from Home Assistant's phone tracking (the HA app), with "
        "since when; fetched fresh. Use it for 'Wer ist zu Hause?', 'Ist Anna schon da?', 'Zeigt Home Assistant "
        "mich als zu Hause?'. Home Assistant's other tools do not show people or phones.",
        R"json({"type": "object", "additionalProperties": false, "properties": {}, "required": []})json",
        [](JsonObjectConst, const tools::Context&) {
            const String error = fetchNow();
            if (error.length()) return error;
            const String found = text();
            return found.length() ? found : String("Home Assistant tracks nobody of the household.");
        },
        tools::Slow, tools::Everywhere});
    tools::add(tools::Tool{
        "locate_person",
        "Where a person of the household is right now: the address of their phone (Home Assistant app, Google "
        "Maps), since when, distance from home. For 'Wo ist Anna gerade?'. Only for the admin.",
        R"json({"type": "object", "additionalProperties": false,
            "properties": {"person": {"type": "string", "description": "name of the person"}},
            "required": ["person"]})json",
        [](JsonObjectConst arguments, const tools::Context&) { return locate(arguments["person"] | ""); },
        tools::Slow | tools::Private, tools::Everywhere});
}

Where of(const String& person)
{
    Locked guard;
    if (!fresh()) return Where::Unknown;
    for (const Entry& entry : entries)
        if (entry.person == person) return entry.state == "home" ? Where::Home : Where::Away;
    return Where::Unknown;
}

String text()
{
    Locked guard;
    if (!fresh() || entries.empty()) return "";
    String home, away;
    for (const Entry& entry : entries) {
        const String name = people::name(entry.person);
        const String since = entry.since.length() ? "since " + entry.since : String();
        if (entry.state == "home") {
            home += (home.length() ? ", " : "") + name + (since.length() ? " (" + since + ")" : String());
        } else {
            const String where = entry.state != "not_home" ? entry.state : String();
            const String detail = where.length() && since.length() ? where + ", " + since : where + since;
            away += (away.length() ? ", " : "") + name + (detail.length() ? " (" + detail + ")" : String());
        }
    }
    String text = "Presence according to Home Assistant (phone tracking, as of " + timeutil::format(fetchedTime, "%H:%M") +
                  ", updated every minute; get_presence fetches it fresh):";
    if (home.length()) text += " at home: " + home + ";";
    if (away.length()) text += " away: " + away + ";";
    return text.substring(0, text.length() - 1) + ". Anyone not listed is not tracked.\n";
}

}  // namespace presence
