#pragma once

#include <Arduino.h>
#include <ArduinoJson.h>

// Backups of the assistant's JSON files (memory core, people register, scheduled tasks). The truth is
// the file in the flash; the backup restores it on a new or erased device. It goes to Google
// Drive (services/drive, folder "Jarvis") when the Google account is set up, else to the
// mailbox as before. Blocks for seconds: background tasks only, holding the TLS gate.
namespace backup {

bool available();
bool onDrive();  // false: the mailbox
// Stores `doc` as the file `name` in Drive; in the mailbox as the mail `subject`, with `summary`
// for a person in front. Returns "stored" or an error.
String store(const char* name, const char* subject, const String& summary, const JsonDocument& doc);
// Reads it back: from Drive, and while Drive has none yet from the mailbox (the backups made
// before 30.09.). False with `error` if unreachable or not readable (never overwrite it then);
// true with `doc` null if there is none.
bool read(const char* name, const char* subject, JsonDocument& doc, String& error);
// True if this device has not stored `name` on Drive yet: the first backup there is due.
bool due(const char* name);
void addTools();  // backup_check (serial tests only)

}  // namespace backup
