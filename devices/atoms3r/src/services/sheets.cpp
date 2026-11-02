#include "services/sheets.h"

#include <ArduinoJson.h>
#include <vector>

#include "assistant/tools.h"
#include "services/drive.h"
#include "services/google.h"
#include "services/psram_json.h"
#include "services/psram_sink.h"
#include "system/utf8.h"

namespace sheets {
namespace {

constexpr const char* HOST = "sheets.googleapis.com";
constexpr const char* FOLDER = "Tabellen";  // in the assistant's Drive folder
constexpr const char* MIME = "application/vnd.google-apps.spreadsheet";
constexpr int DEFAULT_ROWS = 20, MAX_ROWS = 200;
constexpr size_t MAX_TEXT = 8000;  // characters of rows given to the model

String link(const String& id) { return "https://docs.google.com/spreadsheets/d/" + id; }

int request(const char* method, const String& path, const String& body, JsonDocument& answer, String& error)
{
    PsramSink reply(512 * 1024);
    const int status = google::request(method, HOST, "/v4/spreadsheets/" + path, "application/json",
                                       reinterpret_cast<const uint8_t*>(body.c_str()), body.length(), reply, error);
    answer.clear();
    if (status == 200 && reply.data) deserializeJson(answer, reply.data, reply.size);
    return status;
}

// The sheet named `name` in Jarvis/Tabellen: its id, "" if there is none. False on an error.
bool findSheet(const String& name, String& id, String& error)
{
    std::vector<drive::Entry> found;
    if (!drive::list(FOLDER, "name=" + drive::quoted(name) + " and mimeType='" + MIME + "'", 1, found, error))
        return false;
    id = found.empty() ? String() : found[0].id;
    return true;
}

String missing(const String& name) { return "Error: no sheet \"" + name + "\" (read_sheet with sheet '' lists them)"; }

// One row as "a | b | c".
String rowText(JsonArrayConst row)
{
    String text;
    for (JsonVariantConst cell : row) text += String(text.length() ? " | " : "") + cell.as<String>();
    return text;
}

// The values of a row for the API: numbers stay numbers, text is read as typed in the sheet's
// locale (USER_ENTERED: dates, formulas starting with =).
bool rowBody(JsonArrayConst values, JsonDocument& body, String& error)
{
    if (values.size() == 0) {
        error = "Error: values missing";
        return false;
    }
    JsonArray row = body["values"].to<JsonArray>().add<JsonArray>();
    for (JsonVariantConst value : values) row.add(value);
    return true;
}

// --- Tools -------------------------------------------------------------------------------

String readSheet(JsonObjectConst arguments, const tools::Context&)
{
    const String name = arguments["sheet"] | "";
    String error;
    if (name.isEmpty()) {
        std::vector<drive::Entry> found;
        if (!drive::list(FOLDER, String("mimeType='") + MIME + "'", 50, found, error)) return error;
        if (found.empty()) return "No sheets yet (add_sheet_row makes one).";
        String text = "Sheets (newest change first):\n";
        for (const drive::Entry& sheet : found) text += "- " + sheet.name + ": " + link(sheet.id) + "\n";
        return text;
    }
    String id;
    if (!findSheet(name, id, error)) return error;
    if (id.isEmpty()) return missing(name);
    JsonDocument answer(PsramJson::instance());
    if (request("GET", id + "/values/A1:Z5000?majorDimension=ROWS", "", answer, error) != 200) return error;
    JsonArrayConst rows = answer["values"];
    const int wanted = constrain(int(arguments["last_rows"] | DEFAULT_ROWS), 1, MAX_ROWS);
    String text = "Sheet \"" + name + "\" (" + link(id) + "), " + String(rows.size() ? rows.size() - 1 : 0) +
                  " rows below the header:\n";
    if (rows.size() == 0) return text + "(empty)";
    text += "row 1 (header): " + rowText(rows[0]) + "\n";
    const int first = max(1, int(rows.size()) - wanted);
    if (first > 1) text += "(rows 2-" + String(first) + " left out: ask for more with last_rows)\n";
    for (int i = first; i < int(rows.size()); ++i) text += "row " + String(i + 1) + ": " + rowText(rows[i]) + "\n";
    return utf8::prefix(text, MAX_TEXT);
}

String addSheetRow(JsonObjectConst arguments, const tools::Context&)
{
    String name = arguments["sheet"] | "";
    name.trim();
    if (name.isEmpty()) return "Error: sheet name missing";
    JsonDocument row;
    String error, id;
    if (!rowBody(arguments["values"], row, error)) return error;
    if (!findSheet(name, id, error)) return error;
    String made;
    if (id.isEmpty()) {  // a new sheet: header row first
        JsonArrayConst columns = arguments["columns"];
        if (columns.size() == 0) return missing(name) + "; to make it, give columns (the header row)";
        const String result = drive::createEmpty(FOLDER, name, MIME, id);
        if (result != "stored") return result;
        JsonDocument header, answer;
        rowBody(columns, header, error);
        String body;
        serializeJson(header, body);
        if (request("PUT", id + "/values/A1?valueInputOption=USER_ENTERED", body, answer, error) != 200) return error;
        made = "New sheet \"" + name + "\" with the columns " + rowText(columns) + ". ";
    }
    String body;
    serializeJson(row, body);
    JsonDocument answer;
    if (request("POST", id + "/values/A1:append?valueInputOption=USER_ENTERED&insertDataOption=INSERT_ROWS", body,
                answer, error) != 200)
        return error;
    // "Tabellenblatt1!A5:C5" -> row 5
    const String range = answer["updates"]["updatedRange"] | "";
    int number = 0;
    for (int i = range.indexOf('!') + 1; i < int(range.length()); ++i)
        if (isdigit(uint8_t(range[i]))) {
            number = range.substring(i).toInt();
            break;
        }
    return made + "Row " + String(number) + " added to \"" + name + "\": " + rowText(arguments["values"]) + " (" +
           link(id) + ")";
}

String updateSheetRow(JsonObjectConst arguments, const tools::Context&)
{
    const String name = arguments["sheet"] | "";
    const int number = arguments["row"] | 0;
    if (number < 1) return "Error: row as in read_sheet (1 = header)";
    JsonDocument row;
    String error, id;
    if (!rowBody(arguments["values"], row, error)) return error;
    if (!findSheet(name, id, error)) return error;
    if (id.isEmpty()) return missing(name);
    String body;
    serializeJson(row, body);
    JsonDocument answer;
    if (request("PUT", id + "/values/A" + String(number) + "?valueInputOption=USER_ENTERED", body, answer, error) != 200)
        return error;
    return "Row " + String(number) + " of \"" + name + "\" is now: " + rowText(arguments["values"]);
}

}  // namespace

void addTools()
{
    if (!google::configured()) return;
    using tools::Tool;
    tools::add(Tool{
        "read_sheet",
        "Reads one of the tables the user keeps with the assistant (Google Sheets, e.g. fuel stops, expenses, meter "
        "readings): the header and the last rows with their row numbers, and the link. sheet '' lists all tables "
        "with links. Do sums and averages yourself from the rows.",
        R"json({"type": "object", "additionalProperties": false,
            "properties": {"sheet": {"type": "string", "description": "table name, '' = list all"},
                           "last_rows": {"type": "integer", "description": "default 20, at most 200"}},
            "required": ["sheet"]})json",
        readSheet, tools::Slow | tools::Private, tools::Everywhere});
    tools::add(Tool{
        "add_sheet_row",
        "Adds a row to one of the user's tables (Google Sheets), e.g. 'trag die Tankfüllung ein'. A table that does "
        "not exist yet is made with columns as its header: choose short German column names and keep the same "
        "order in values. Look at the header with read_sheet first if unsure. Numbers as JSON numbers, dates as "
        "DD.MM.YYYY.",
        R"json({"type": "object", "additionalProperties": false,
            "properties": {"sheet": {"type": "string", "description": "table name, e.g. 'Tanken'"},
                           "values": {"type": "array", "items": {"type": ["string", "number"]}},
                           "columns": {"type": "array", "items": {"type": "string"},
                                       "description": "header for a new table, [] if it exists"}},
            "required": ["sheet", "values", "columns"]})json",
        addSheetRow, tools::Slow | tools::Private, tools::Everywhere});
    tools::add(Tool{
        "update_sheet_row",
        "Replaces one row of a table (row number from read_sheet), to correct an entry.",
        R"json({"type": "object", "additionalProperties": false,
            "properties": {"sheet": {"type": "string"},
                           "row": {"type": "integer"},
                           "values": {"type": "array", "items": {"type": ["string", "number"]}}},
            "required": ["sheet", "row", "values"]})json",
        updateSheetRow, tools::Slow | tools::Private, tools::Everywhere});
    // Only for tests over serial: the test table goes to Drive's trash again.
    tools::add(Tool{
        "delete_sheet", "Moves a table to Drive's trash.",
        R"json({"type": "object", "additionalProperties": false,
            "properties": {"sheet": {"type": "string"}}, "required": ["sheet"]})json",
        [](JsonObjectConst arguments, const tools::Context&) {
            const String name = arguments["sheet"] | "";
            String id, error;
            if (!findSheet(name, id, error)) return error;
            return id.isEmpty() ? missing(name) : drive::trashFile(id);
        },
        tools::Slow, tools::Console});
}

}  // namespace sheets
