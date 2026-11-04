#include "assistant/prompt.h"

#include <time.h>

#include "assistant/memory.h"
#include "assistant/people.h"
#include "persona.h"
#include "config.h"
#include "system/time_util.h"

namespace prompt {

String context()
{
    static const char* const weekdays[] = {"Sonntag", "Montag", "Dienstag", "Mittwoch",
                                           "Donnerstag", "Freitag", "Samstag"};
    const time_t now = time(nullptr);
    if (!timeutil::synced()) return "";
    tm local;
    localtime_r(&now, &local);
    char text[160];
    const int n = snprintf(text, sizeof(text), "Current time: %s, %02d.%02d.%04d, %02d:%02d.",
                           weekdays[local.tm_wday], local.tm_mday, local.tm_mon + 1,
                           local.tm_year + 1900, local.tm_hour, local.tm_min);
    // The home town from the profile unless LOCATION is set.
    const String home = *LOCATION ? String(LOCATION) : memory::profileValue("home");
    if (home.length()) snprintf(text + n, sizeof(text) - n, " Location: %s.", home.c_str());
    return text;
}

namespace {

String tone(const people::Person& person)
{
    if (person.child) return PERSONA_CHILD;
    const int level = people::humor(person);
    return String(PERSONA_LEVELS[level]) + (level > 0 ? PERSONA_LIMITS : "");
}

}  // namespace

String persona(const String& person)
{
    String text = PERSONA_CORE;
    people::Person known;
    if (person.length() && people::find(person, known)) return text + tone(known);
    // Nobody named: the default, and for whom something else holds once it is clear who talks.
    const int level = min(max(PERSONA_LEVEL, 0), PERSONA_MAX_LEVEL);
    text += String(PERSONA_LEVELS[level]) + (level > 0 ? PERSONA_LIMITS : "");
    for (const people::Person& p : people::all())
        if (p.child || people::humor(p) != level)
            text += "# Once it is clear that you are talking with " + p.name + ", this applies instead\n" + tone(p);
    return text;
}

}  // namespace prompt
