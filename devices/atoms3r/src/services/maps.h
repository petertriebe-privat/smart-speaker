#pragma once

#include <Arduino.h>

// Google Maps Platform with an API key (GOOGLE_MAPS_API_KEY in secrets.h).
// Tool find_place (Places API (New) Text Search): businesses and places with address, phone
// number, opening hours and rating, biased towards home (Home Assistant's zone.home). Tool
// travel_time (Routes API): by car with traffic, public transport with connections, bike, walk.
// Tool get_weather (Weather, Pollen and Air Quality APIs): now, hourly, daily, warnings, pollen,
// air quality, for home or a place looked up with Places.
// Requests are counted per API and calendar month in /quota.json, before they are sent; at the
// monthly limit the tool refuses, at QUOTA_WARN_PERCENT the admin gets one Telegram message.
namespace maps {

bool configured();
void addTools();
// Counts one request of `api` in this month's quota (/quota.json), for other users of the same
// key (services/youtube). False with `error` at `limit`.
bool count(const char* api, int limit, String& error);
// The street address of a position (Google Geocoding API, counted as "geocoding"). "" with
// `error` if there is none or the API refused.
String address(double latitude, double longitude, String& error);
// Straight-line distance in km from home (Home Assistant's zone.home), < 0 if home is unknown.
double kmFromHome(double latitude, double longitude);

}  // namespace maps
