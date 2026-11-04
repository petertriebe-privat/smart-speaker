#pragma once

#include <Arduino.h>
#include <time.h>

// Local time (TIMEZONE, set by system/network via NTP).
namespace timeutil {

// False until NTP has set the clock (time() then counts from 1970).
inline bool synced() { return time(nullptr) > 1700000000; }

// strftime in local time, e.g. format(t, "%Y-%m-%d %H:%M").
inline String format(time_t t, const char* pattern)
{
    tm local;
    localtime_r(&t, &local);
    char text[40];
    strftime(text, sizeof(text), pattern, &local);
    return text;
}

inline String today() { return format(time(nullptr), "%Y-%m-%d"); }

}  // namespace timeutil
