#include "services/todos.h"

#include <ArduinoJson.h>
#include <vector>

#include "assistant/agenda.h"
#include "assistant/tools.h"
#include "services/google.h"
#include "services/psram_json.h"
#include "services/psram_sink.h"
#include "system/time_util.h"

namespace todos {
namespace {

constexpr const char* HOST = "tasks.googleapis.com";
constexpr int MAX_TASKS = 100;  // per list

struct List {
    String id, title;
};

String urlEncode(const String& text)
{
    static const char* hex = "0123456789ABCDEF";
    String out;
    for (size_t i = 0; i < text.length(); ++i) {
        const uint8_t c = text[i];
        if (isalnum(c) || c == '-' || c == '_' || c == '.' || c == '~') {
            out += char(c);
        } else {
            out += '%';
            out += hex[c >> 4];
            out += hex[c & 15];
        }
    }
    return out;
}

// One request with a JSON body ("" = none); the answer parsed into `answer` (may stay empty,
// e.g. DELETE). Returns the HTTP status; `error` is set unless it is 2xx.
int request(const char* method, const String& path, const String& body, JsonDocument& answer, String& error)
{
    PsramSink reply(256 * 1024);
    const int status = google::request(method, HOST, "/tasks/v1" + path, "application/json",
                                       reinterpret_cast<const uint8_t*>(body.c_str()), body.length(), reply, error);
    answer.clear();
    if (status >= 200 && status < 300 && reply.data && reply.size) deserializeJson(answer, reply.data, reply.size);
    return status;
}

bool lists(std::vector<List>& found, String& error)
{
    JsonDocument answer;
    if (request("GET", "/users/@me/lists?maxResults=100", "", answer, error) != 200) return false;
    found.clear();
    for (JsonObjectConst list : answer["items"].as<JsonArrayConst>()) found.push_back({list["id"] | "", list["title"] | ""});
    return true;
}

// The list named `name` (case-insensitive; "" = the first one, Google's default). `make`: made
// when missing. False with `error` if there is none.
bool findList(const String& name, bool make, List& list, String& error)
{
    std::vector<List> all;
    if (!lists(all, error)) return false;
    for (const List& candidate : all)
        if (name.isEmpty() || candidate.title.equalsIgnoreCase(name)) {
            list = candidate;
            return true;
        }
    if (name.isEmpty() || !make) {
        String names;
        for (const List& candidate : all) names += String(names.length() ? ", " : "") + "\"" + candidate.title + "\"";
        error = "Error: no list \"" + name + "\" (lists: " + names + ")";
        return false;
    }
    JsonDocument body, answer;
    body["title"] = name;
    String json;
    serializeJson(body, json);
    if (request("POST", "/users/@me/lists", json, answer, error) != 200) return false;
    list = {answer["id"] | "", name};
    return true;
}

// "YYYY-MM-DD" -> Google's due value (Tasks keeps only the date). "" if malformed.
String dueValue(const String& date)
{
    int year, month, day;
    if (sscanf(date.c_str(), "%4d-%2d-%2d", &year, &month, &day) != 3 || month < 1 || month > 12 || day < 1 || day > 31)
        return "";
    char text[32];
    snprintf(text, sizeof(text), "%04d-%02d-%02dT00:00:00.000Z", year, month, day);
    return text;
}

String describe(JsonObjectConst task, const String& listId)
{
    String line = "- " + String(task["title"] | "(no title)");
    const String due = task["due"] | "";
    if (due.length() >= 10) line += " | due " + due.substring(8, 10) + "." + due.substring(5, 7) + "." + due.substring(0, 4);
    if (String(task["status"] | "") == "completed") line += " | done";
    String notes = task["notes"] | "";
    if (notes.length()) {
        notes.replace("\n", " ");
        line += " | note: " + notes.substring(0, 150);
    }
    return line + " [id " + listId + "/" + String(task["id"] | "") + "]\n";
}

// "<list>/<task>" -> the two ids. False if malformed.
bool splitId(const String& id, String& list, String& task)
{
    const int slash = id.indexOf('/');
    if (slash <= 0 || slash == int(id.length()) - 1) return false;
    list = id.substring(0, slash);
    task = id.substring(slash + 1);
    return true;
}

// --- Tools -------------------------------------------------------------------------------

String listTodos(JsonObjectConst arguments, const tools::Context&)
{
    const String name = arguments["list"] | "";
    const bool done = arguments["include_done"] | false;
    std::vector<List> selected;
    String error;
    if (name.length()) {
        List list;
        if (!findList(name, false, list, error)) return error;
        selected.push_back(list);
    } else if (!lists(selected, error)) {
        return error;
    }
    String text;
    for (const List& list : selected) {
        JsonDocument answer(PsramJson::instance());
        const String path = "/lists/" + urlEncode(list.id) + "/tasks?maxResults=" + String(MAX_TASKS) +
                            "&showCompleted=" + (done ? "true&showHidden=true" : "false");
        if (request("GET", path, "", answer, error) != 200) return error;
        JsonArrayConst tasks = answer["items"];
        text += "List \"" + list.title + "\"" + (tasks.size() ? ":\n" : ": empty\n");
        for (JsonObjectConst task : tasks) text += describe(task, list.id);
    }
    return text.length() ? text : "No to-do lists.";
}

String addTodo(JsonObjectConst arguments, const tools::Context&)
{
    agenda::refreshSoon();  // the agenda of the next conversation shows the change
    String title = arguments["title"] | "";
    title.trim();
    if (title.isEmpty()) return "Error: title missing";
    List list;
    String error;
    if (!findList(arguments["list"] | "", true, list, error)) return error;
    JsonDocument task;
    task["title"] = title;
    const String notes = arguments["notes"] | "";
    if (notes.length()) task["notes"] = notes;
    const String due = arguments["due"] | "";
    if (due.length()) {
        const String value = dueValue(due);
        if (value.isEmpty()) return "Error: due as YYYY-MM-DD";
        task["due"] = value;
    }
    String body;
    serializeJson(task, body);
    JsonDocument answer;
    if (request("POST", "/lists/" + urlEncode(list.id) + "/tasks", body, answer, error) != 200) return error;
    return "Added to \"" + list.title + "\":\n" + describe(answer.as<JsonObjectConst>(), list.id);
}

String changeTodo(JsonObjectConst arguments, const tools::Context&)
{
    agenda::refreshSoon();  // the agenda of the next conversation shows the change
    String listId, taskId;
    if (!splitId(arguments["id"] | "", listId, taskId)) return "Error: id as in list_todos (list/task)";
    const String action = arguments["action"] | "";
    const String path = "/lists/" + urlEncode(listId) + "/tasks/" + urlEncode(taskId);
    JsonDocument answer;
    String error;
    if (action == "delete") {
        const int status = request("DELETE", path, "", answer, error);
        if (status == 404) return "Error: no such task (call list_todos first)";
        return status == 204 || status == 200 ? String("Deleted.") : error;
    }
    JsonDocument change;
    if (action == "complete") {
        change["status"] = "completed";
    } else if (action == "reopen") {
        change["status"] = "needsAction";
        change["completed"] = nullptr;
    } else if (action == "edit") {
        for (const char* key : {"title", "notes"})
            if (arguments[key].is<const char*>()) change[key] = arguments[key];
        if (arguments["due"].is<const char*>()) {
            const String due = arguments["due"].as<const char*>(), value = dueValue(due);
            if (due.length() && value.isEmpty()) return "Error: due as YYYY-MM-DD, '' removes it";
            if (value.length()) change["due"] = value;
            else change["due"] = nullptr;
        }
        if (change.isNull()) return "Error: nothing to change given";
    } else {
        return "Error: action is complete, reopen, edit or delete";
    }
    String body;
    serializeJson(change, body);
    const int status = request("PATCH", path, body, answer, error);
    if (status == 404) return "Error: no such task (call list_todos first)";
    if (status != 200) return error;
    // Google keeps deleted tasks a while and lets them be changed: they are gone for the user.
    if (answer["deleted"] | false) return "Error: this task was deleted";
    return (action == "complete" ? "Done:\n" : action == "reopen" ? "Open again:\n" : "Changed:\n") +
           describe(answer.as<JsonObjectConst>(), listId);
}

}  // namespace

bool due(const String& lastDay, std::vector<Todo>& out, String& error)
{
    out.clear();
    const String dueMax = dueValue(lastDay);
    if (dueMax.isEmpty()) {
        error = "Error: lastDay as YYYY-MM-DD";
        return false;
    }
    std::vector<List> all;
    if (!lists(all, error)) return false;
    for (const List& list : all) {
        JsonDocument answer(PsramJson::instance());
        // dueMax is exclusive in Google's API: the day after lastDay would need date arithmetic,
        // so the end of lastDay ("T23:59:59").
        String max = dueMax;
        max.replace("T00:00:00", "T23:59:59");
        const String path = "/lists/" + urlEncode(list.id) + "/tasks?maxResults=" + String(MAX_TASKS) +
                            "&showCompleted=false&dueMax=" + urlEncode(max);
        if (request("GET", path, "", answer, error) != 200) return false;
        for (JsonObjectConst task : answer["items"].as<JsonArrayConst>()) {
            const String date = String(task["due"] | "").substring(0, 10);
            if (date.length() < 10) continue;
            out.push_back({list.id + "/" + String(task["id"] | ""), task["title"] | "(no title)", date,
                           all.size() > 1 ? list.title : String()});
        }
    }
    return true;
}

void addTools()
{
    if (!google::configured()) return;
    using tools::Tool;
    tools::add(Tool{
        "list_todos",
        "Reads the user's to-do lists in Google Tasks (the Tasks app on their phone): open tasks with due date "
        "and id, per list. For 'was steht auf meiner Liste', 'was muss ich noch erledigen'.",
        R"json({"type": "object", "additionalProperties": false,
            "properties": {"list": {"type": "string", "description": "list name, '' = all lists"},
                           "include_done": {"type": "boolean", "description": "also completed tasks"}},
            "required": ["list"]})json",
        listTodos, tools::Slow | tools::Private, tools::Everywhere});
    tools::add(Tool{
        "add_todo",
        "Adds a task to one of the user's Google Tasks lists (a list that does not exist yet is made, e.g. "
        "'Baumarkt'). Not for groceries: those go on the shopping list in Home Assistant. For a spoken reminder at "
        "a time use create_task.",
        R"json({"type": "object", "additionalProperties": false,
            "properties": {"title": {"type": "string", "description": "short, German"},
                           "list": {"type": "string", "description": "list name, '' = the default list"},
                           "notes": {"type": "string"},
                           "due": {"type": "string", "description": "YYYY-MM-DD, '' = none"}},
            "required": ["title", "list"]})json",
        addTodo, tools::Slow | tools::Private, tools::Everywhere});
    tools::add(Tool{
        "change_todo",
        "Changes a task of the user's Google Tasks lists (id from list_todos): complete ('ist erledigt'), reopen, "
        "edit title, notes or due date, or delete it.",
        R"json({"type": "object", "additionalProperties": false,
            "properties": {"id": {"type": "string"},
                           "action": {"type": "string", "enum": ["complete", "reopen", "edit", "delete"]},
                           "title": {"type": "string"},
                           "notes": {"type": "string"},
                           "due": {"type": "string", "description": "YYYY-MM-DD, '' removes it"}},
            "required": ["id", "action"]})json",
        changeTodo, tools::Slow | tools::Private, tools::Everywhere});
}

}  // namespace todos
