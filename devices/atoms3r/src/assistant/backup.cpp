#include "assistant/backup.h"

#include "assistant/tools.h"
#include "config.h"
#include "services/drive.h"
#include "services/mail.h"
#include "services/psram_json.h"
#include "services/psram_sink.h"
#include "system/tls_gate.h"

namespace backup {

bool onDrive() { return drive::configured(); }
bool available() { return onDrive() || mail::configured(); }

String store(const char* name, const char* subject, const String& summary, const JsonDocument& doc)
{
    // A second speaker (config.h INBOUND) must not replace the main the assistant's backups.
    if (!INBOUND) return "stored";
    if (!onDrive()) return mail::storeBackup(subject, summary, doc);
    PsramText json(doc);  // the core can pass 64 KB, where an Arduino String breaks
    if (!json.data) return "Error: not enough memory for the backup";
    return drive::store(name, "application/json", reinterpret_cast<const uint8_t*>(json.data), json.size);
}

bool read(const char* name, const char* subject, JsonDocument& doc, String& error)
{
    doc.clear();
    if (onDrive()) {
        PsramSink data(4 * 1024 * 1024);
        const int found = drive::load(name, data, error);
        if (found < 0) return false;
        if (found > 0) {
            if (data.data && !deserializeJson(doc, data.data, data.size)) return true;
            error = String("Error: the backup ") + name + " in Drive is not readable (" + String(data.size) +
                    " bytes); an older version is in its Drive history";
            doc.clear();
            return false;
        }
        if (!mail::configured()) return true;
        Serial.printf("[Backup] %s not in Drive yet, looking in the mailbox\n", name);
    }
    return mail::readBackup(subject, doc, error);
}

bool due(const char* name) { return onDrive() && !drive::known(name); }

void addTools()
{
    // Only for tests over serial: reads every backup the way a restore does, changes nothing.
    tools::add(tools::Tool{
        "backup_check", "Reads the backups back (as a restore would) and counts their entries.",
        R"json({"type": "object", "additionalProperties": false, "properties": {}, "required": []})json",
        [](JsonObjectConst, const tools::Context&) {
            if (!available()) return String("Error: no backup configured");
            tls_gate::Hold gate;
            String text = String("Backups in ") + (onDrive() ? "Drive" : "the mailbox") + ":\n";
            const char* const files[][3] = {{"core.json", "Kern", "facts"},
                                            {"people.json", ASSISTANT_NAME "-Personen", "people"},
                                            {"tasks.json", ASSISTANT_NAME "-Aufgaben", "tasks"}};
            for (const auto& file : files) {
                JsonDocument doc(PsramJson::instance());
                String error;
                if (!read(file[0], file[1], doc, error)) text += String("- ") + file[0] + ": " + error + "\n";
                else if (doc.isNull()) text += String("- ") + file[0] + ": none\n";
                else text += String("- ") + file[0] + ": " + String(doc[file[2]].size()) + " " + file[2] + "\n";
            }
            return text;
        },
        tools::Slow, tools::Console});
}

}  // namespace backup
