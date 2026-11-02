#pragma once

#include <ArduinoJson.h>
#include <LittleFS.h>
#include <esp_heap_caps.h>

// Small helpers for the JSON files in LittleFS and their backups in the mailbox.
namespace fileutil {

// Writes `doc` to a temporary file and renames it over `path`: a crash while writing leaves
// the old file intact (on 27.09. a crash left the memory's core file empty).
// Serialized into PSRAM first and written in one call: straight into the file ArduinoJson wrote
// in tiny pieces, and the mail watch's 125 KB took 3.5-6.7 s, a flash write that stalls both
// cores (06.10.).
inline bool saveJson(const char* path, const JsonDocument& doc)
{
    const size_t size = measureJson(doc);
    char* buffer = static_cast<char*>(heap_caps_malloc(size + 1, MALLOC_CAP_SPIRAM | MALLOC_CAP_8BIT));
    if (!buffer) return false;
    serializeJson(doc, buffer, size + 1);
    const String temporary = String(path) + ".tmp";
    File file = LittleFS.open(temporary, "w");
    const bool written = file && size && file.write(reinterpret_cast<const uint8_t*>(buffer), size) == size;
    file.close();
    free(buffer);
    return written && LittleFS.rename(temporary, path);
}

// The JSON part of a backup mail: after `marker`, or the whole text if it starts with it (the
// backups of 27.09. were JSON only: serializeJson(doc, String) replaces the string's content).
inline const char* backupJson(const String& note, const char* marker)
{
    const int at = note.indexOf(marker);
    if (at >= 0) return note.c_str() + at + strlen(marker);
    const int start = note.indexOf('{');
    return start >= 0 ? note.c_str() + start : nullptr;
}

}  // namespace fileutil
