#include "services/maps.h"

#include <ArduinoJson.h>
#include <HTTPClient.h>
#include <LittleFS.h>

#include "assistant/tools.h"
#include "ca_certs.h"
#include "config.h"
#include "secrets.h"
#include "services/home_assistant.h"
#include "services/https.h"
#include "services/psram_json.h"
#include "services/psram_sink.h"
#include "services/telegram.h"
#include "system/file_util.h"
#include "system/scoped_lock.h"
#include "system/time_util.h"
#include "system/utf8.h"

namespace maps {
namespace {

constexpr const char* PLACES_HOST = "places.googleapis.com";
constexpr const char* ROUTES_HOST = "routes.googleapis.com";
constexpr const char* GEOCODING_HOST = "maps.googleapis.com";
constexpr const char* WEATHER_HOST = "weather.googleapis.com";
constexpr const char* POLLEN_HOST = "pollen.googleapis.com";
constexpr const char* AIR_QUALITY_HOST = "airquality.googleapis.com";
const char* const WEEKDAYS[] = {"So", "Mo", "Di", "Mi", "Do", "Fr", "Sa"};
constexpr const char* QUOTA_FILE = "/quota.json";
constexpr uint32_t TIMEOUT_MS = 20000;
// Enterprise SKU (phone, opening hours, rating): what find_place is for.
constexpr const char* PLACE_FIELDS =
    "places.displayName,places.formattedAddress,places.internationalPhoneNumber,places.websiteUri,"
    "places.rating,places.userRatingCount,places.businessStatus,places.currentOpeningHours.openNow,"
    "places.regularOpeningHours.weekdayDescriptions";
// Pro SKU with TRAFFIC_AWARE; the steps only for public transport (connections).
constexpr const char* ROUTE_FIELDS = "routes.duration,routes.staticDuration,routes.distanceMeters,routes.description,"
                                     "routes.warnings";
constexpr const char* TRANSIT_FIELDS = "routes.duration,routes.distanceMeters,routes.warnings,"
                                       "routes.legs.steps.travelMode,routes.legs.steps.staticDuration,"
                                       "routes.legs.steps.transitDetails";

SemaphoreHandle_t quotaLock = xSemaphoreCreateMutex();
struct Locked : ScopedLock {
    Locked() : ScopedLock(quotaLock) {}
};

// --- Monthly quota: {"month": "2026-09", "count": {"places": 12}, "warned": {"places": true}} ---

// Counts one request of `api` unless the month's `limit` is reached (then false with `error`).
// Warns the admin once a month at QUOTA_WARN_PERCENT.
bool take(const char* api, int limit, String& error)
{
    if (!timeutil::synced()) {
        error = "Error: the clock is not set yet.";
        return false;
    }
    const String month = timeutil::format(time(nullptr), "%Y-%m");
    int used;
    bool warn = false;
    {
        Locked locked;
        JsonDocument quota;
        File file = LittleFS.exists(QUOTA_FILE) ? LittleFS.open(QUOTA_FILE, "r") : File();
        if (!file || deserializeJson(quota, file)) quota.clear();
        file.close();
        if (month != (quota["month"] | "")) {
            quota.clear();
            quota["month"] = month;
        }
        used = quota["count"][api] | 0;
        if (used >= limit) {
            error = "Error: monthly limit reached (" + String(limit) + " " + api +
                    " requests this month); tell the user, it resets on the 1st.";
            return false;
        }
        quota["count"][api] = ++used;
        if (used * 100 >= limit * QUOTA_WARN_PERCENT && !(quota["warned"][api] | false)) {
            quota["warned"][api] = true;
            warn = true;
        }
        if (!fileutil::saveJson(QUOTA_FILE, quota)) Serial.println("[Maps] quota file not written");
    }
    Serial.printf("[Maps] %s request %d of %d this month\n", api, used, limit);
    if (warn)
        telegram::send("⚠️ Kontingent " + String(api) + ": " + String(used) + " von " + String(limit) +
                       " Anfragen in diesem Monat verbraucht. Bei " + String(limit) + " sperrt " ASSISTANT_NAME " weitere.");
    return true;
}

String usage()
{
    Locked locked;
    JsonDocument quota;
    File file = LittleFS.exists(QUOTA_FILE) ? LittleFS.open(QUOTA_FILE, "r") : File();
    if (!file || deserializeJson(quota, file)) return "No requests counted yet.";
    String text;
    serializeJson(quota, text);
    return text;
}

// --- Home (Home Assistant's zone.home), fetched once --------------------------------------

bool homeKnown = false;
double homeLatitude = 0, homeLongitude = 0;

bool home(double& latitude, double& longitude)
{
    if (!homeKnown) {
        JsonDocument filter, zone;
        filter["attributes"]["latitude"] = true;
        filter["attributes"]["longitude"] = true;
        if (ha::rest("GET", "/api/states/zone.home", "", zone, &filter) == 200 &&
            zone["attributes"]["latitude"].is<double>() && zone["attributes"]["longitude"].is<double>()) {
            homeLatitude = zone["attributes"]["latitude"];
            homeLongitude = zone["attributes"]["longitude"];
            homeKnown = true;
        } else {
            Serial.println("[Maps] home coordinates not available from Home Assistant");
        }
    }
    latitude = homeLatitude;
    longitude = homeLongitude;
    return homeKnown;
}

// --- HTTP -----------------------------------------------------------------------------

// A POST with a JSON body, or a GET (body ""). `fieldMask` nullptr: the API has none. The
// connection stays open for further requests to the same host (a TLS handshake costs ~2 s).
int send(https::Client& client, const String& path, const char* fieldMask, const String& body, PsramSink& reply)
{
    client.setTimeout(TIMEOUT_MS);
    if (!client.connect()) return HTTPC_ERROR_CONNECTION_REFUSED;
    String head = String(body.length() ? "POST " : "GET ") + path + " HTTP/1.1\r\nHost: " + client.host +
                  "\r\nX-Goog-Api-Key: " + GOOGLE_MAPS_API_KEY + "\r\n";
    if (fieldMask) head += String("X-Goog-FieldMask: ") + fieldMask + "\r\n";
    if (body.length()) head += "Content-Type: application/json\r\nContent-Length: " + String(body.length()) + "\r\n";
    head += "\r\n";
    if (!client.write(head) || (body.length() && !client.write(body))) {
        client.stop();
        return HTTPC_ERROR_SEND_PAYLOAD_FAILED;
    }
    return client.read(reply);
}

// Over a kept connection (https::Lease); one the server closed meanwhile is opened anew once.
int send(const char* host, const String& path, const char* fieldMask, const String& body, PsramSink& reply)
{
    https::Lease lease(host, CA_CERTS);
    int status = send(lease.client(), path, fieldMask, body, reply);
    if (status < 0 && status != HTTPC_ERROR_READ_TIMEOUT && lease.reused()) {
        lease.client().stop();
        reply.size = 0;
        status = send(lease.client(), path, fieldMask, body, reply);
    }
    return status;
}

// Google's answers nest deeply (transit times sit 11 levels down).
String parse(JsonDocument& parsed, const PsramSink& reply)
{
    const DeserializationError failed =
        deserializeJson(parsed, reply.data, reply.size, DeserializationOption::NestingLimit(20));
    return failed ? "Error: Google's answer is not readable (" + String(failed.c_str()) + ")" : "";
}

String errorText(int status, const PsramSink& reply)
{
    if (status == HTTPC_ERROR_READ_TIMEOUT) return "Error: Google Maps does not answer";
    if (status < 0) return "Error: Google Maps not reachable (" + HTTPClient::errorToString(status) + ")";
    JsonDocument parsed;
    String message;
    if (reply.data && !deserializeJson(parsed, reply.data, reply.size)) message = parsed["error"]["message"] | "";
    return "Error: HTTP " + String(status) + (message.length() ? ": " + utf8::prefix(message, 200) : "");
}

// --- find_place -----------------------------------------------------------------------

String findPlace(JsonObjectConst arguments, const tools::Context&)
{
    String query = arguments["query"] | "";
    query.trim();
    if (query.isEmpty()) return "Error: query missing";
    JsonDocument request;
    request["textQuery"] = query;
    request["languageCode"] = "de";
    request["regionCode"] = "DE";
    request["pageSize"] = PLACES_RESULTS;
    if (arguments["open_now"] | false) request["openNow"] = true;
    double latitude, longitude;
    if (home(latitude, longitude)) {
        JsonObject circle = request["locationBias"]["circle"].to<JsonObject>();
        circle["center"]["latitude"] = latitude;
        circle["center"]["longitude"] = longitude;
        circle["radius"] = PLACES_BIAS_METERS;
    }
    String body, error;
    serializeJson(request, body);
    if (!take("places", PLACES_MONTHLY_LIMIT, error)) return error;
    PsramSink reply(256 * 1024);
    const int status = send(PLACES_HOST, "/v1/places:searchText", PLACE_FIELDS, body, reply);
    if (status != 200) return errorText(status, reply);
    JsonDocument parsed(PsramJson::instance());
    error = parse(parsed, reply);
    if (error.length()) return error;

    JsonArrayConst places = parsed["places"];
    if (places.size() == 0) return "No places found for \"" + query + "\".";
    String text = "Places for \"" + query + "\":\n";
    for (JsonObjectConst place : places) {
        text += "- " + String(place["displayName"]["text"] | "(no name)");
        const char* address = place["formattedAddress"];
        if (address) text += " | " + String(address);
        const char* phone = place["internationalPhoneNumber"];
        if (phone) text += " | phone " + String(phone);
        if (place["rating"].is<float>())
            text += " | rating " + String(place["rating"].as<float>(), 1) + " (" + String(place["userRatingCount"] | 0) + ")";
        const String business = place["businessStatus"] | "";
        if (business.length() && business != "OPERATIONAL") text += " | " + business;
        if (place["currentOpeningHours"]["openNow"].is<bool>())
            text += place["currentOpeningHours"]["openNow"].as<bool>() ? " | open now" : " | closed now";
        String hours;
        for (JsonVariantConst day : place["regularOpeningHours"]["weekdayDescriptions"].as<JsonArrayConst>())
            hours += String(hours.length() ? "; " : "") + day.as<const char*>();
        if (hours.length()) text += " | hours: " + hours;
        const char* website = place["websiteUri"];
        if (website) text += " | " + String(website);
        text += "\n";
    }
    return text;
}

// --- travel_time ----------------------------------------------------------------------

int seconds(JsonVariantConst duration) { return atoi(duration | "0"); }  // "1234s"

String minutesText(int seconds)
{
    const int minutes = (seconds + 30) / 60;
    if (minutes < 60) return String(minutes) + " min";
    return String(minutes / 60) + " h " + String(minutes % 60) + " min";
}

String clockText(time_t t) { return timeutil::format(t, "%H:%M"); }

// "YYYY-MM-DD" ('' = today) and "HH:MM" as local time.
bool parseLocal(const String& date, const String& clock, time_t& out)
{
    const String day = date.length() ? date : timeutil::today();
    int year, month, dayOfMonth, hour, minute;
    if (sscanf(day.c_str(), "%4d-%2d-%2d", &year, &month, &dayOfMonth) != 3 ||
        sscanf(clock.c_str(), "%2d:%2d", &hour, &minute) != 2 || year > 2037 || hour > 23 || minute > 59)
        return false;
    tm t{};
    t.tm_year = year - 1900;
    t.tm_mon = month - 1;
    t.tm_mday = dayOfMonth;
    t.tm_hour = hour;
    t.tm_min = minute;
    t.tm_isdst = -1;
    out = mktime(&t);
    return out != -1;
}

String utcStamp(time_t t)
{
    tm utc;
    gmtime_r(&t, &utc);
    char text[24];
    strftime(text, sizeof(text), "%Y-%m-%dT%H:%M:%SZ", &utc);
    return text;
}

bool isHome(const String& place)
{
    return place.isEmpty() || place.equalsIgnoreCase("home") || place.equalsIgnoreCase("zuhause") ||
           place.equalsIgnoreCase("daheim");
}

// An address or place name, or home (Home Assistant's coordinates). False with `error`.
bool setWaypoint(JsonObject waypoint, const String& place, String& error)
{
    if (!isHome(place)) {
        waypoint["address"] = place;
        return true;
    }
    double latitude, longitude;
    if (!home(latitude, longitude)) {
        error = "Error: home is unknown (Home Assistant's zone.home): give the address";
        return false;
    }
    JsonObject latLng = waypoint["location"]["latLng"].to<JsonObject>();
    latLng["latitude"] = latitude;
    latLng["longitude"] = longitude;
    return true;
}

// Public transport: the rides with times, lines and stops, walks in between.
String connections(JsonArrayConst steps)
{
    String text;
    int walk = 0;
    for (JsonObjectConst step : steps) {
        if (step["travelMode"] != "TRANSIT") {
            walk += seconds(step["staticDuration"]);
            continue;
        }
        if (walk >= 60) text += "  walk " + minutesText(walk) + "\n";
        walk = 0;
        JsonObjectConst details = step["transitDetails"];
        JsonObjectConst line = details["transitLine"];
        const String name = line["nameShort"] | line["name"] | "";
        text += "  " + String(details["localizedValues"]["departureTime"]["time"]["text"] | "?") + " " +
                String(line["vehicle"]["name"]["text"] | "") + " " + name;
        const char* headsign = details["headsign"];
        if (headsign) text += " towards " + String(headsign);
        text += " from " + String(details["stopDetails"]["departureStop"]["name"] | "?") + " to " +
                String(details["stopDetails"]["arrivalStop"]["name"] | "?") + ", arrives " +
                String(details["localizedValues"]["arrivalTime"]["time"]["text"] | "?") + "\n";
    }
    if (walk >= 60) text += "  walk " + minutesText(walk) + "\n";
    return text;
}

String travelTime(JsonObjectConst arguments, const tools::Context&)
{
    if (!timeutil::synced()) return "Error: the clock is not set yet.";
    const String destination = arguments["destination"] | "", origin = arguments["origin"] | "";
    if (isHome(destination) && isHome(origin)) return "Error: origin and destination are both home";
    const String mode = arguments["mode"] | "drive";
    const char* travelMode = mode == "transit" ? "TRANSIT" : mode == "bicycle" ? "BICYCLE" : mode == "walk" ? "WALK"
                             : mode == "drive"                                            ? "DRIVE"
                                                                                          : nullptr;
    if (!travelMode) return "Error: mode is drive, transit, bicycle or walk";

    // When: now, or leaving / arriving at date + time.
    const String clock = arguments["time"] | "";
    const bool arriving = String(arguments["time_is"] | "departure") == "arrival";
    const time_t now = time(nullptr);
    time_t when = now;
    if (clock.length()) {
        if (!parseLocal(arguments["date"] | "", clock, when)) return "Error: date as YYYY-MM-DD, time as HH:MM";
        if (when < now - 60) return "Error: this time is in the past";
    }

    JsonDocument request;
    String error;
    if (!setWaypoint(request["origin"].to<JsonObject>(), origin, error) ||
        !setWaypoint(request["destination"].to<JsonObject>(), destination, error))
        return error;
    request["travelMode"] = travelMode;
    request["languageCode"] = "de";
    request["regionCode"] = "DE";
    request["units"] = "METRIC";
    const bool transit = !strcmp(travelMode, "TRANSIT"), drive = !strcmp(travelMode, "DRIVE");
    if (drive) request["routingPreference"] = "TRAFFIC_AWARE";
    // Arriving by car: the traffic around the arrival time stands in for the one on the way.
    if (transit && arriving) request["arrivalTime"] = utcStamp(when);
    else if ((transit || drive) && when > now + 60) request["departureTime"] = utcStamp(when);

    String body;
    serializeJson(request, body);
    if (!take("routes", ROUTES_MONTHLY_LIMIT, error)) return error;
    PsramSink reply(512 * 1024);
    const int status = send(ROUTES_HOST, "/directions/v2:computeRoutes", transit ? TRANSIT_FIELDS : ROUTE_FIELDS,
                            body, reply);
    if (status != 200) return errorText(status, reply);
    JsonDocument parsed(PsramJson::instance());
    error = parse(parsed, reply);
    if (error.length()) return error;
    JsonObjectConst route = parsed["routes"][0];
    if (route.isNull()) return "No route found (check the places, or try another mode).";

    const int duration = seconds(route["duration"]);
    const String from = isHome(origin) ? "home" : origin, to = isHome(destination) ? "home" : destination;
    const char* by = transit ? "public transport" : drive ? "car" : mode == "bicycle" ? "bicycle" : "foot";
    String text = "By " + String(by) + " from " + from + " to " + to + ": " + minutesText(duration);
    if (drive) {
        const int without = seconds(route["staticDuration"]);
        text += without && without != duration ? " with the traffic expected (" + minutesText(without) + " without)"
                                               : " (no traffic delay)";
    }
    text += ", " + String(route["distanceMeters"].as<float>() / 1000, 1) + " km";
    const char* via = route["description"];
    if (via && *via) text += ", via " + String(via);
    text += ".\n";
    if (arriving && !transit) {
        const time_t leave = when - duration;
        text += leave >= now ? "Leave by " + clockText(leave) + " to arrive at " + clockText(when) + ".\n"
                             : "Too late: leaving now, arrival about " + clockText(now + duration) + " instead of " +
                                   clockText(when) + ".\n";
    } else if (!transit) {
        text += "Leaving " + (when > now + 60 ? "at " + clockText(when) : String("now")) + ", arrival about " +
                clockText(when + duration) + ".\n";
    }
    if (transit) text += connections(route["legs"][0]["steps"]);
    for (JsonVariantConst warning : route["warnings"].as<JsonArrayConst>())
        text += "Note: " + String(warning.as<const char*>()) + "\n";
    return text;
}

// --- get_weather ----------------------------------------------------------------------

// "2026-09-30T05:19:42.43Z" (UTC) -> time_t, 0 if malformed (days from the civil calendar).
time_t fromUtc(const char* stamp)
{
    int year, month, day, hour, minute, second;
    if (!stamp || sscanf(stamp, "%d-%d-%dT%d:%d:%d", &year, &month, &day, &hour, &minute, &second) != 6) return 0;
    year -= month <= 2;
    const int era = year / 400;
    const int yearOfEra = year - era * 400;
    const int dayOfYear = (153 * (month + (month > 2 ? -3 : 9)) + 2) / 5 + day - 1;
    const int dayOfEra = yearOfEra * 365 + yearOfEra / 4 - yearOfEra / 100 + dayOfYear;
    const long days = long(era) * 146097 + dayOfEra - 719468;
    return time_t(days * 86400L + hour * 3600L + minute * 60L + second);
}

// {"year", "month", "day"} -> "Mi 30.09."
String dayLabel(JsonObjectConst date)
{
    tm t{};
    t.tm_year = (date["year"] | 1970) - 1900;
    t.tm_mon = (date["month"] | 1) - 1;
    t.tm_mday = date["day"] | 1;
    t.tm_hour = 12;
    t.tm_isdst = -1;
    const time_t noon = mktime(&t);
    return String(WEEKDAYS[t.tm_wday]) + timeutil::format(noon, " %d.%m.");
}

String celsius(JsonVariantConst temperature) { return String(temperature["degrees"].as<float>(), 1) + " °C"; }

// Condition, rain chance and amount, wind: the parts every forecast shares.
String conditions(JsonObjectConst forecast)
{
    String text = forecast["weatherCondition"]["description"]["text"] | "?";
    text += ", rain " + String(forecast["precipitation"]["probability"]["percent"] | 0) + " %";
    const float amount = forecast["precipitation"]["qpf"]["quantity"] | 0.0f;
    if (amount >= 0.1f) text += " (" + String(amount, 1) + " mm)";
    text += ", wind " + String(forecast["wind"]["speed"]["value"] | 0) + " km/h";
    const int gusts = forecast["wind"]["gust"]["value"] | 0;
    if (gusts >= 40) text += " (gusts " + String(gusts) + ")";
    const int thunder = forecast["thunderstormProbability"] | 0;
    if (thunder >= 20) text += ", thunderstorm " + String(thunder) + " %";
    return text;
}

struct Spot {
    double latitude = 0, longitude = 0;
    String name;
};

// Home, or a place looked up with Places (one request of that quota).
bool locate(const String& place, Spot& spot, String& error)
{
    if (isHome(place)) {
        spot.name = "home";
        if (home(spot.latitude, spot.longitude)) return true;
        error = "Error: home is unknown (Home Assistant's zone.home): give a place";
        return false;
    }
    JsonDocument request;
    request["textQuery"] = place;
    request["languageCode"] = "de";
    request["pageSize"] = 1;
    String body;
    serializeJson(request, body);
    if (!take("places", PLACES_MONTHLY_LIMIT, error)) return false;
    PsramSink reply(64 * 1024);
    const int status = send(PLACES_HOST, "/v1/places:searchText", "places.location,places.formattedAddress", body, reply);
    JsonDocument parsed;
    if (status != 200) error = errorText(status, reply);
    else error = parse(parsed, reply);
    if (error.length()) return false;
    JsonObjectConst found = parsed["places"][0];
    if (!found["location"]["latitude"].is<double>()) {
        error = "Error: place not found: " + place;
        return false;
    }
    spot.latitude = found["location"]["latitude"];
    spot.longitude = found["location"]["longitude"];
    spot.name = found["formattedAddress"] | place.c_str();
    return true;
}

// One counted request over `client` (kept open between the parts of a call); false with `error`.
// A connection the server closed meanwhile fails at once: then once more on a new one.
bool fetch(const char* api, int limit, https::Client& client, const String& path, const String& body,
           JsonDocument& doc, String& error)
{
    if (!take(api, limit, error)) return false;
    PsramSink reply(256 * 1024);
    const bool reused = client.connected();
    int status = send(client, path, nullptr, body, reply);
    if (status < 0 && status != HTTPC_ERROR_READ_TIMEOUT && reused) {
        client.stop();
        reply.size = 0;
        status = send(client, path, nullptr, body, reply);
    }
    error = status == 200 ? parse(doc, reply) : errorText(status, reply);
    return error.isEmpty();
}

String weatherNow(https::Client& client, const String& where)
{
    JsonDocument doc(PsramJson::instance());
    String error;
    if (!fetch("weather", WEATHER_MONTHLY_LIMIT, client, "/v1/currentConditions:lookup?" + where, "", doc, error))
        return "Now: " + error + "\n";
    JsonObjectConst history = doc["currentConditionsHistory"];
    return "Now: " + celsius(doc["temperature"]) + " (feels like " + celsius(doc["feelsLikeTemperature"]) + "), " +
           conditions(doc.as<JsonObjectConst>()) + ", humidity " + String(doc["relativeHumidity"] | 0) + " %, UV " +
           String(doc["uvIndex"] | 0) + "; last 24 h " + celsius(history["minTemperature"]) + " to " +
           celsius(history["maxTemperature"]) + ".\n";
}

String weatherHours(https::Client& client, const String& where, int hours)
{
    JsonDocument doc(PsramJson::instance());
    String error;
    const String count = String(hours);
    if (!fetch("weather", WEATHER_MONTHLY_LIMIT, client,
               "/v1/forecast/hours:lookup?" + where + "&hours=" + count + "&pageSize=" + count, "", doc, error))
        return "Hourly: " + error + "\n";
    String text = "Hourly:\n";
    for (JsonObjectConst hour : doc["forecastHours"].as<JsonArrayConst>()) {
        char clock[8];
        snprintf(clock, sizeof(clock), "%02d:00", int(hour["displayDateTime"]["hours"] | 0));
        text += String("- ") + clock + " " + celsius(hour["temperature"]) + ", " + conditions(hour) + "\n";
    }
    return text;
}

String weatherDays(https::Client& client, const String& where, int days)
{
    JsonDocument doc(PsramJson::instance());
    String error;
    const String count = String(days);
    if (!fetch("weather", WEATHER_MONTHLY_LIMIT, client,
               "/v1/forecast/days:lookup?" + where + "&days=" + count + "&pageSize=" + count, "", doc, error))
        return "Daily: " + error + "\n";
    String text = "Daily:\n";
    for (JsonObjectConst day : doc["forecastDays"].as<JsonArrayConst>()) {
        text += "- " + dayLabel(day["displayDate"]) + ": " + celsius(day["minTemperature"]) + " to " +
                celsius(day["maxTemperature"]) + "; day " + conditions(day["daytimeForecast"]) + ", UV " +
                String(day["daytimeForecast"]["uvIndex"] | 0) + "; night " + conditions(day["nighttimeForecast"]);
        const time_t rise = fromUtc(day["sunEvents"]["sunriseTime"]), set = fromUtc(day["sunEvents"]["sunsetTime"]);
        if (rise && set) text += "; sunrise " + clockText(rise) + ", sunset " + clockText(set);
        text += "\n";
    }
    return text;
}

String weatherAlerts(https::Client& client, const String& where)
{
    JsonDocument doc(PsramJson::instance());
    String error;
    if (!fetch("weather", WEATHER_MONTHLY_LIMIT, client, "/v1/publicAlerts:lookup?" + where, "", doc, error))
        return "Warnings: " + error + "\n";
    JsonArrayConst alerts = doc["weatherAlerts"];
    if (alerts.size() == 0) return "Official weather warnings: none.\n";
    String text = "Official weather warnings:\n";
    for (JsonObjectConst alert : alerts) {
        text += "- " + String(alert["alertTitle"]["text"] | "?") + " (" + String(alert["severity"] | "") + ")";
        const time_t from = fromUtc(alert["startTime"]), until = fromUtc(alert["expirationTime"]);
        if (from && until)
            text += " " + timeutil::format(from, "%d.%m. %H:%M") + " to " + timeutil::format(until, "%d.%m. %H:%M");
        const char* area = alert["areaName"];
        if (area) text += ", " + String(area);
        String description = alert["description"] | "";
        description.replace("\n", " ");
        if (description.length()) text += ": " + utf8::prefix(description, 300);
        const char* instruction = alert["instruction"][0];
        if (instruction) text += " Advice: " + utf8::prefix(instruction, 200);
        text += "\n";
    }
    return text;
}

String pollen(const String& where, int days)
{
    JsonDocument doc(PsramJson::instance());
    String error;
    https::Lease lease(POLLEN_HOST, CA_CERTS);
    https::Client& client = lease.client();
    if (!fetch("pollen", POLLEN_MONTHLY_LIMIT, client,
               "/v1/forecast:lookup?" + where + "&days=" + String(min(days, 5)) + "&plantsDescription=false", "", doc,
               error))
        return "Pollen: " + error + "\n";
    String text = "Pollen (Universal Pollen Index 0-5):\n";
    for (JsonObjectConst day : doc["dailyInfo"].as<JsonArrayConst>()) {
        text += "- " + dayLabel(day["date"]) + ":";
        String advice;
        int highest = 0;
        for (JsonObjectConst type : day["pollenTypeInfo"].as<JsonArrayConst>()) {
            if (!type["indexInfo"].is<JsonObjectConst>()) continue;
            const int value = type["indexInfo"]["value"] | 0;
            text += " " + String(type["displayName"] | "?") + " " + String(value) + " " +
                    String(type["indexInfo"]["category"] | "") + (type["inSeason"] | true ? "" : " (out of season)") + ";";
            if (value > highest) {
                highest = value;
                advice = type["healthRecommendations"][0] | "";
            }
        }
        String plants;
        for (JsonObjectConst plant : day["plantInfo"].as<JsonArrayConst>()) {
            const int value = plant["indexInfo"]["value"] | 0;
            if (value > 0) plants += String(plants.length() ? ", " : "") + (plant["displayName"] | "?") + " " + value;
        }
        if (plants.length()) text += " plants: " + plants + ";";
        if (highest >= 3 && advice.length()) text += " advice: " + advice;
        text += "\n";
    }
    return text;
}

String airQuality(const Spot& spot)
{
    JsonDocument request;
    request["location"]["latitude"] = spot.latitude;
    request["location"]["longitude"] = spot.longitude;
    request["languageCode"] = "de";
    JsonArray extra = request["extraComputations"].to<JsonArray>();
    extra.add("LOCAL_AQI");
    extra.add("HEALTH_RECOMMENDATIONS");
    extra.add("DOMINANT_POLLUTANT_CONCENTRATION");
    String body, error;
    serializeJson(request, body);
    JsonDocument doc(PsramJson::instance());
    https::Lease lease(AIR_QUALITY_HOST, CA_CERTS);
    https::Client& client = lease.client();
    if (!fetch("air", AIR_QUALITY_MONTHLY_LIMIT, client, "/v1/currentConditions:lookup", body, doc, error))
        return "Air quality: " + error + "\n";
    String text = "Air quality:";
    for (JsonObjectConst index : doc["indexes"].as<JsonArrayConst>()) {
        text += " " + String(index["displayName"] | "?");
        if (index["aqiDisplay"].is<const char*>()) text += " " + String(index["aqiDisplay"].as<const char*>());
        text += " " + String(index["category"] | "") + ";";
    }
    JsonObjectConst pollutant = doc["pollutants"][0];
    if (!pollutant.isNull()) {
        const String units = pollutant["concentration"]["units"] | "";
        text += " dominant " + String(pollutant["fullName"] | "?") + " " +
                String(pollutant["concentration"]["value"].as<float>(), 0) +
                (units == "PARTS_PER_BILLION" ? " ppb" : units == "MICROGRAMS_PER_CUBIC_METER" ? " µg/m³" : "") + ";";
    }
    const char* advice = doc["healthRecommendations"]["generalPopulation"];
    if (advice) text += " advice: " + String(advice);
    return text + "\n";
}

String getWeather(JsonObjectConst arguments, const tools::Context&)
{
    JsonArrayConst include = arguments["include"];
    if (include.size() == 0) return "Error: include missing (now, hours, days, alerts, pollen, air)";
    auto wants = [&](const char* part) {
        for (JsonVariantConst item : include)
            if (item == part) return true;
        return false;
    };
    Spot spot;
    String error;
    if (!locate(arguments["place"] | "", spot, error)) return error;
    const String where = "location.latitude=" + String(spot.latitude, 5) + "&location.longitude=" +
                         String(spot.longitude, 5) + "&languageCode=de";
    const String metric = where + "&unitsSystem=METRIC";
    const int days = constrain(int(arguments["days"] | 3), 1, 10);
    String text = "Weather for " + spot.name + " (" + timeutil::format(time(nullptr), "%d.%m. %H:%M") + "):\n";
    {
        https::Lease lease(WEATHER_HOST, CA_CERTS);  // one kept connection for these parts
        https::Client& weather = lease.client();
        if (wants("now")) text += weatherNow(weather, metric);
        if (wants("hours")) text += weatherHours(weather, metric, constrain(int(arguments["hours"] | 12), 1, 24));
        if (wants("days")) text += weatherDays(weather, metric, days);
        if (wants("alerts")) text += weatherAlerts(weather, where);
    }
    if (wants("pollen")) text += pollen(where, days);
    if (wants("air")) text += airQuality(spot);
    return text;
}

}  // namespace

bool configured() { return *GOOGLE_MAPS_API_KEY; }

bool count(const char* api, int limit, String& error) { return take(api, limit, error); }

String address(double latitude, double longitude, String& error)
{
    if (!configured()) {
        error = "Error: no Google Maps key";
        return "";
    }
    if (!take("geocoding", GEOCODING_MONTHLY_LIMIT, error)) return "";
    // The Geocoding web service takes the key as a parameter.
    const String path = "/maps/api/geocode/json?language=de&latlng=" + String(latitude, 6) + "," +
                        String(longitude, 6) + "&key=" + GOOGLE_MAPS_API_KEY;
    PsramSink reply(128 * 1024);
    const int status = send(GEOCODING_HOST, path, nullptr, "", reply);
    JsonDocument parsed(PsramJson::instance());
    if (status != 200 || parse(parsed, reply).length()) {
        error = errorText(status, reply);
        return "";
    }
    const String result = parsed["status"] | "";
    if (result != "OK") {
        error = "Error: Geocoding " + result + " " + String(parsed["error_message"] | "") +
                (result == "REQUEST_DENIED" ? " (enable the Geocoding API and allow it for the key)" : "");
        return "";
    }
    return parsed["results"][0]["formatted_address"] | "";
}

double kmFromHome(double latitude, double longitude)
{
    double homeLatitude, homeLongitude;
    if (!home(homeLatitude, homeLongitude)) return -1;
    const double radians = PI / 180, dLat = (latitude - homeLatitude) * radians,
                 dLon = (longitude - homeLongitude) * radians;
    const double a = sin(dLat / 2) * sin(dLat / 2) +
                     cos(homeLatitude * radians) * cos(latitude * radians) * sin(dLon / 2) * sin(dLon / 2);
    return 6371.0 * 2 * atan2(sqrt(a), sqrt(1 - a));  // haversine
}

void addTools()
{
    if (!configured()) return;
    using tools::Tool;
    tools::add(Tool{
        "find_place",
        "Searches Google Maps for businesses and places (restaurants, shops, doctors, craftsmen, authorities), "
        "preferring those near home: address, phone number, opening hours, whether open now, rating, website. Use it "
        "instead of the web search for such questions and to get the number before phone_call to a business. "
        "Limited to 1000 searches a month: one precise query (what and where, e.g. 'Italienisches Restaurant "
        "Altstadt' or 'Zahnarzt Dr. Müller Hamburg'), not several tries.",
        R"json({"type": "object", "additionalProperties": false,
            "properties": {"query": {"type": "string", "description": "what and, if not near home, where"},
                           "open_now": {"type": "boolean", "description": "only places open right now"}},
            "required": ["query"]})json",
        findPlace, tools::Slow, tools::Everywhere});
    tools::add(Tool{
        "travel_time",
        "Travel time between two places with Google Maps: by car with the traffic expected, by public transport "
        "with the connections (times, lines, stops), by bicycle or on foot. For 'wie lange brauche ich nach …', "
        "'wann muss ich los, um um 9 in … zu sein', 'wann fährt der nächste Bus nach …'. Places as address or name "
        "with town; '' or 'home' is the user's home. Limited to 1000 requests a month.",
        R"json({"type": "object", "additionalProperties": false,
            "properties": {"destination": {"type": "string", "description": "'' = home"},
                           "origin": {"type": "string", "description": "'' = home"},
                           "mode": {"type": "string", "enum": ["drive", "transit", "bicycle", "walk"]},
                           "date": {"type": "string", "description": "YYYY-MM-DD, '' = today"},
                           "time": {"type": "string", "description": "HH:MM, '' = now"},
                           "time_is": {"type": "string", "enum": ["departure", "arrival"]}},
            "required": ["destination", "mode"]})json",
        travelTime, tools::Slow, tools::Everywhere});
    tools::add(Tool{
        "get_weather",
        "Weather from Google for home or another place. include picks the parts: now (current conditions), hours "
        "(hourly forecast), days (daily forecast with sunrise and sunset), alerts (official weather warnings), "
        "pollen (pollen forecast, up to 5 days), air (air quality now). Ask only for what the question needs; a "
        "morning overview: now, days, alerts, pollen. Use it instead of the web search for weather.",
        R"json({"type": "object", "additionalProperties": false,
            "properties": {"include": {"type": "array",
                                       "items": {"type": "string", "enum": ["now", "hours", "days", "alerts", "pollen", "air"]}},
                           "place": {"type": "string", "description": "town or address, '' = home"},
                           "hours": {"type": "integer", "description": "hours ahead, 1-24, default 12"},
                           "days": {"type": "integer", "description": "days ahead incl. today, 1-10, default 3"}},
            "required": ["include"]})json",
        getWeather, tools::Slow, tools::Everywhere});
    // Only for tests over serial: the month's counted requests.
    tools::add(Tool{
        "maps_usage", "Requests to Google Maps counted this month (quota file).",
        R"json({"type": "object", "additionalProperties": false, "properties": {}, "required": []})json",
        [](JsonObjectConst, const tools::Context&) { return usage(); }, 0, tools::Console});
}

}  // namespace maps
