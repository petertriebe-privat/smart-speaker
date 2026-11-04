#pragma once

// YouTube: find_video searches with the YouTube Data API (the Google API key
// GOOGLE_MAPS_API_KEY, counted as "youtube" in the monthly quota; strict SafeSearch for children).
// play_video plays a video on a TV the way phones cast ("watch on TV"): the TVs are found in the
// LAN by SSDP (DIAL: smart TVs, Fire TV), nothing is configured; DIAL starts the YouTube app,
// whose screen id then takes the video over YouTube's Lounge API. The TV has to be on (or in
// network standby).
namespace youtube {

void addTools();

}  // namespace youtube
