#pragma once

#include <Arduino.h>
#include <ArduinoJson.h>
#include <vector>

#include "services/psram_sink.h"

// The assistant's files in the user's Google Drive: a visible folder "Jarvis" with the
// backups (core.json, people.json, tasks.json) and subfolders for the memory's archive ("Themen",
// "Gespräche"). Scope drive.file: The assistant sees only the files it made itself (or that
// tools/migrate_archive.py made with the same OAuth client). Drive keeps older versions of a
// file (revisions). Folder ids and the backups' file ids are kept in /drive.json. Blocks for
// seconds; call it from a background task holding the TLS gate, or from a slow tool.
namespace drive {

bool configured();  // the Google account is set up (services/google)

// --- Backups: one file per name in the folder the assistant, content replaced ---------------------

// Replaces the content of `name` (makes folder and file when missing). "stored" or "Error: ...".
String store(const char* name, const char* mime, const uint8_t* data, size_t length);
// The content of `name` into `out`: 1 found, 0 there is none, -1 error (`error` set).
int load(const char* name, PsramSink& out, String& error);
// True once this device stored or found `name` (its id is known).
bool known(const char* name);

// --- Files in subfolders (the archive) ---------------------------------------------------

struct Entry {
    String id, name;
    String type, modified, source;  // MIME type, RFC 3339 time, appProperties.source
    size_t size;  // no default member initializer: C++11 aggregates ({id, name} sets it to 0)
};

// `text` as a Drive query literal: 'text' with \ and ' escaped.
String quoted(const String& text);
// Files matching the Drive query `query` ("" = all), newest first, at most `limit`: in the
// subfolder `folder` of the assistant, or anywhere in the assistant's files (folder nullptr). False on an error.
bool list(const char* folder, const String& query, size_t limit, std::vector<Entry>& found, String& error);
bool download(const String& id, PsramSink& out, String& error);
// A new file in the subfolder (made when missing), with `properties` (appProperties, may be
// null) to find it again. One request (multipart). "stored" or "Error: ...".
String create(const char* folder, const String& name, const char* mime, const uint8_t* data, size_t length,
              JsonObjectConst properties);
// A new empty file of `mime` in the subfolder, e.g. a Google Sheet
// (application/vnd.google-apps.spreadsheet); its id into `id`. "stored" or "Error: ...".
String createEmpty(const char* folder, const String& name, const char* mime, String& id);
// Replaces the content of the file `id`. "stored" or "Error: ...".
String replace(const String& id, const char* mime, const uint8_t* data, size_t length);
// Moves one file to Drive's trash. "trashed" or "Error: ...".
String trashFile(const String& id);
// Moves the subfolder with everything in it to Drive's trash (the user can restore it for 30
// days). "trashed" (also when there was none) or "Error: ...".
String trash(const char* folder);

}  // namespace drive
