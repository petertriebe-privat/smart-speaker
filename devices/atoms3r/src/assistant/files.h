#pragma once

#include <Arduino.h>

#include "services/psram_sink.h"

// Files the assistant received or made (Telegram and WhatsApp documents, files the model wrote), kept in
// The assistant's Google Drive folder (The assistant/Dateien), not in the flash, so they can be sent on later:
// "druck das PDF aus, das ich dir geschickt habe", "leite die Rechnung an Anna weiter". Up to
// FILE_MAX_BYTES each; the user tidies the folder in Drive. One tool sends a stored or new file
// to a Telegram chat, an e-mail address or the printer (send_file); list_files shows the store.
// Calls block for seconds (Drive): from a background task holding the TLS gate or a slow tool.
namespace files {

// Keeps a copy of `data` under `name` (a same name is replaced). `source`: where it came from
// ("Telegram von Anna"). Returns "" or an error.
String store(const String& name, const String& type, const uint8_t* data, size_t size, const String& source);
// MIME type for a file name, e.g. "text/csv" for .csv.
String mimeOf(const String& name);

// Registers send_file and list_files (assistant/tools). After memory::begin() (LittleFS).
void addTools();

}  // namespace files
