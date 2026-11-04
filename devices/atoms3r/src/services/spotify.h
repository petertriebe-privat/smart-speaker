#pragma once

#include <Arduino.h>

// Spotify over its Web API (issue #39): play anything on any Spotify Connect device (Echos,
// phones, ...), control playback, manage playlists and the library, read top tracks and history.
// Needs Premium. One account per person: the admin's (SPOTIFY_REFRESH_TOKEN) and others
// (SPOTIFY_ACCOUNTS), refresh tokens from tools/spotify_auth.py; whoever talks uses their own,
// else the admin's. Development Mode (since 2026-03): no recommendations, related artists, artist
// top tracks, audio features or Spotify's own playlists (docs/architecture.md).
// Tools (all slow): play_music, music_control, music_speakers, music_playlist, music_library,
// music_insights.
namespace spotify {

bool configured();
void addTools();
// Every loop: whether the assistant is talking at the speaker at home (a voice conversation, not a phone
// call, Telegram or WhatsApp: then nobody needs to hear the assistant over the music at home). Music
// playing in the assistant's room (SPOTIFY_ROOM_SPEAKERS; without that list on any speaker at home, not on
// phones or computers), any account, is turned down to SPOTIFY_DUCK_VOLUME while it lasts and back
// afterwards, unless someone changed the volume meanwhile. Runs in the registry's
// worker, never delays the conversation.
void setConversation(bool open);

}  // namespace spotify
