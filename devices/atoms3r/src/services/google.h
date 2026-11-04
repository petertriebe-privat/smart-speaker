#pragma once

#include <Arduino.h>
#include <vector>

#include "services/psram_sink.h"

// The user's Google account: calendars (all shown in Google Calendar; writing in their own) and
// contacts (read only), over the Calendar and People REST APIs. Only events in the user's own
// calendars marked busy take their time; holidays and subscribed calendars are information.
// OAuth 2.0 with a refresh token from tools/google_auth.py
// (GOOGLE_CLIENT_ID / GOOGLE_CLIENT_SECRET / GOOGLE_REFRESH_TOKEN in secrets.h); the access
// token is fetched when needed and kept until shortly before it expires (1 h).
// Tools (all slow, in the registry's worker): list_events, find_free_time, create_event, update_event,
// delete_event, find_contact, upcoming_birthdays. Contacts reach the backend model only through
// these calls; the events of today and tomorrow also through the agenda (assistant/agenda).
namespace google {

bool configured();
void addTools();
// The relay's answer to a login link (google_login, sent by itself when the login expired):
// queues the code for the worker if `state` is the pending link's. From any task, does not block.
void loginCallback(const String& code, const String& state);

// An event for the agenda (assistant/agenda).
struct Event {
    time_t start = 0, end = 0;  // all day: midnight of the first day / of the day after the last
    bool allDay = false;
    bool info = false;  // does not take the user's time (holidays, subscriptions, marked free)
    String id;          // "<calendar id>/<event id>", stable across requests
    String title;
    String line;        // as list_events shows it ("Mo 05.10.2026 09:00–10:00: ... | place: ...")
};
// The events of all calendars shown in Google Calendar from `from` until `to` (exclusive),
// declined ones left out, sorted by start. Blocks for seconds (one request per calendar).
bool events(time_t from, time_t to, std::vector<Event>& out, String& error);
// One request to a Google API with the account's access token (services/drive), or with that of
// another account's `refreshToken` (a household member's Gmail, assistant/mail_watch; same OAuth
// client). `body` may be a PSRAM buffer of any size. Returns the HTTP status (0 without a token);
// `error` ("Error: ...") is set unless it is 2xx. Blocks for seconds.
int request(const char* method, const char* host, const String& path, const char* contentType, const uint8_t* body,
            size_t length, PsramSink& reply, String& error, const String& refreshToken = "");
// Name of the contact with this phone number ("" if none), for greeting callers. Looks it up in
// a copy of all contacts' numbers in PSRAM (refreshed every GOOGLE_DIRECTORY_MS by the registry's
// worker between conversations): instant, no network during the call.
String callerName(const String& number);

}  // namespace google
