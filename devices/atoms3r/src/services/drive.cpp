#include "services/drive.h"

#include <LittleFS.h>

#include "config.h"
#include "services/google.h"
#include "system/file_util.h"
#include "system/scoped_lock.h"

namespace drive {
namespace {

constexpr const char* HOST = "www.googleapis.com";
constexpr const char* FOLDER = ASSISTANT_NAME "";
constexpr const char* FOLDER_MIME = "application/vnd.google-apps.folder";
constexpr const char* IDS_FILE = "/drive.json";  // {"folder": id, "folders": {name: id}, "files": {name: id}}
constexpr const char* BOUNDARY = "assistant-part-boundary";

SemaphoreHandle_t lock = xSemaphoreCreateMutex();
struct Locked : ScopedLock {
    Locked() : ScopedLock(lock) {}
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

// --- Cached ids: "folder" (The assistant), "folders" (subfolders), "files" (backups) ------------

JsonDocument readIds()
{
    JsonDocument ids;
    File file = LittleFS.exists(IDS_FILE) ? LittleFS.open(IDS_FILE, "r") : File();
    if (!file || deserializeJson(ids, file)) ids.clear();
    return ids;
}

String cachedId(const char* group, const char* name)  // group nullptr: the folder the assistant
{
    Locked locked;
    const JsonDocument ids = readIds();
    return group ? ids[group][name] | "" : ids["folder"] | "";
}

void cacheId(const char* group, const char* name, const String& id)  // id "" forgets it
{
    Locked locked;
    JsonDocument ids = readIds();
    if (!group) ids["folder"] = id;
    else if (id.length()) ids[group][name] = id;
    else ids[group].remove(name);
    if (!fileutil::saveJson(IDS_FILE, ids)) Serial.println("[Drive] id file not written");
}

void forgetAll()  // something was deleted in Drive: look everything up again
{
    Locked locked;
    LittleFS.remove(IDS_FILE);
}

// --- Requests --------------------------------------------------------------------------

int jsonRequest(const char* method, const String& path, const String& body, JsonDocument& answer, String& error)
{
    PsramSink reply(256 * 1024);
    const int status = google::request(method, HOST, path, "application/json",
                                       reinterpret_cast<const uint8_t*>(body.c_str()), body.length(), reply, error);
    answer.clear();
    if (status >= 200 && status < 300 && reply.data) deserializeJson(answer, reply.data, reply.size);
    return status;
}

// The id of the first file matching `query`, "" if none; false on an error.
bool searchOne(const String& query, String& id, String& error)
{
    JsonDocument answer;
    if (jsonRequest("GET", "/drive/v3/files?spaces=drive&fields=files(id)&pageSize=1&q=" + urlEncode(query), "", answer,
                    error) != 200)
        return false;
    id = answer["files"][0]["id"] | "";
    return true;
}

bool createMetadata(const JsonDocument& metadata, String& id, String& error)
{
    String body;
    serializeJson(metadata, body);
    JsonDocument answer;
    if (jsonRequest("POST", "/drive/v3/files?fields=id", body, answer, error) != 200) return false;
    id = answer["id"] | "";
    if (id.isEmpty()) error = "Error: Drive gave no id";
    return id.length();
}

// A folder named `name` in `parent` ("" = anywhere), made when missing.
bool findFolder(const String& name, const String& parent, String& id, String& error)
{
    String query = "name=" + quoted(name) + " and mimeType='" + FOLDER_MIME + "' and trashed=false";
    if (parent.length()) query += " and '" + parent + "' in parents";
    if (!searchOne(query, id, error)) return false;
    if (id.length()) return true;
    JsonDocument metadata;
    metadata["name"] = name;
    metadata["mimeType"] = FOLDER_MIME;
    if (parent.length()) metadata["parents"].add(parent);
    if (!createMetadata(metadata, id, error)) return false;
    Serial.printf("[Drive] folder %s made\n", name.c_str());
    return true;
}

bool rootId(String& id, String& error)
{
    id = cachedId(nullptr, nullptr);
    if (id.length()) return true;
    if (!findFolder(FOLDER, "", id, error)) return false;
    cacheId(nullptr, nullptr, id);
    return true;
}

bool subfolderId(const char* folder, String& id, String& error)
{
    id = cachedId("folders", folder);
    if (id.length()) return true;
    String root;
    if (!rootId(root, error) || !findFolder(folder, root, id, error)) return false;
    cacheId("folders", folder, id);
    return true;
}

// A backup's id; "" (and true) if there is none and `make` is false.
bool fileId(const char* name, const char* mime, bool make, String& id, String& error)
{
    id = cachedId("files", name);
    if (id.length()) return true;
    String root;
    if (!rootId(root, error)) return false;
    if (!searchOne("name=" + quoted(name) + " and '" + root + "' in parents and trashed=false", id, error)) return false;
    if (id.isEmpty() && make) {
        JsonDocument metadata;
        metadata["name"] = name;
        metadata["mimeType"] = mime;
        metadata["parents"].add(root);
        if (!createMetadata(metadata, id, error)) return false;
    }
    if (id.length()) cacheId("files", name, id);
    return true;
}

}  // namespace

bool configured() { return google::configured(); }

String quoted(const String& text)
{
    String out = "'";
    for (size_t i = 0; i < text.length(); ++i) {
        if (text[i] == '\\' || text[i] == '\'') out += '\\';
        out += text[i];
    }
    return out + "'";
}

// --- Backups -----------------------------------------------------------------------------

bool known(const char* name) { return cachedId("files", name).length(); }

String store(const char* name, const char* mime, const uint8_t* data, size_t length)
{
    String error;
    for (int attempt = 0; attempt < 2; ++attempt) {
        String id;
        if (!fileId(name, mime, true, id, error)) return error;
        const String result = replace(id, mime, data, length);
        if (result == "stored" || !result.startsWith("Error: HTTP 404")) return result;
        error = result;
        forgetAll();
    }
    return error;
}

int load(const char* name, PsramSink& out, String& error)
{
    for (int attempt = 0; attempt < 2; ++attempt) {
        String id;
        if (!fileId(name, "", false, id, error)) return -1;
        if (id.isEmpty()) return 0;
        if (download(id, out, error)) return 1;
        if (!error.startsWith("Error: HTTP 404")) return -1;
        forgetAll();
    }
    return 0;
}

// --- Files in subfolders -----------------------------------------------------------------

bool list(const char* folder, const String& query, size_t limit, std::vector<Entry>& found, String& error)
{
    found.clear();
    String q = "trashed=false";
    if (folder) {
        String parent;
        if (!subfolderId(folder, parent, error)) return false;
        q += " and '" + parent + "' in parents";
    }
    if (query.length()) q += " and (" + query + ")";
    JsonDocument answer;
    const int status = jsonRequest("GET",
                                   "/drive/v3/files?spaces=drive&orderBy=modifiedTime%20desc"
                                   "&fields=files(id,name,mimeType,size,modifiedTime,appProperties)&pageSize=" +
                                       String(limit) + "&q=" + urlEncode(q),
                                   "", answer, error);
    if (status == 404) forgetAll();
    if (status != 200) return false;
    for (JsonObjectConst file : answer["files"].as<JsonArrayConst>()) {
        Entry entry{file["id"] | "", file["name"] | ""};
        entry.type = file["mimeType"] | "";
        entry.modified = file["modifiedTime"] | "";
        entry.source = file["appProperties"]["source"] | "";
        entry.size = String(file["size"] | "0").toInt();  // Drive sends the size as a string
        found.push_back(entry);
    }
    return true;
}

bool download(const String& id, PsramSink& out, String& error)
{
    out.size = 0;
    const int status = google::request("GET", HOST, "/drive/v3/files/" + id + "?alt=media", "", nullptr, 0, out, error);
    if (status != 200) return false;
    error = "";
    return true;
}

String create(const char* folder, const String& name, const char* mime, const uint8_t* data, size_t length,
              JsonObjectConst properties)
{
    String parent, error;
    if (!subfolderId(folder, parent, error)) return error;
    JsonDocument metadata;
    metadata["name"] = name;
    metadata["mimeType"] = mime;
    metadata["parents"].add(parent);
    if (!properties.isNull()) metadata["appProperties"] = properties;
    // multipart/related: the metadata, then the content, in one request.
    PsramSink body(length + 4096);
    body.print(String("--") + BOUNDARY + "\r\nContent-Type: application/json; charset=UTF-8\r\n\r\n");
    serializeJson(metadata, body);
    body.print(String("\r\n--") + BOUNDARY + "\r\nContent-Type: " + mime + "\r\n\r\n");
    body.write(data, length);
    body.print(String("\r\n--") + BOUNDARY + "--\r\n");
    PsramSink reply(16 * 1024);
    const int status = google::request("POST", HOST, "/upload/drive/v3/files?uploadType=multipart&fields=id",
                                       (String("multipart/related; boundary=") + BOUNDARY).c_str(), body.data,
                                       body.size, reply, error);
    if (status == 404) forgetAll();  // the subfolder was deleted
    return status == 200 ? String("stored") : error;
}

String createEmpty(const char* folder, const String& name, const char* mime, String& id)
{
    String parent, error;
    if (!subfolderId(folder, parent, error)) return error;
    JsonDocument metadata;
    metadata["name"] = name;
    metadata["mimeType"] = mime;
    metadata["parents"].add(parent);
    return createMetadata(metadata, id, error) ? String("stored") : error;
}

String replace(const String& id, const char* mime, const uint8_t* data, size_t length)
{
    String error;
    PsramSink reply(16 * 1024);
    const int status = google::request("PATCH", HOST, "/upload/drive/v3/files/" + id + "?uploadType=media&fields=id",
                                       mime, data, length, reply, error);
    return status == 200 ? String("stored") : error;
}

String trashFile(const String& id)
{
    JsonDocument answer;
    String error;
    return jsonRequest("PATCH", "/drive/v3/files/" + id + "?fields=id", "{\"trashed\": true}", answer, error) == 200
               ? String("trashed")
               : error;
}

String trash(const char* folder)
{
    String root, id, error;
    if (!rootId(root, error)) return error;
    if (!searchOne("name=" + quoted(folder) + " and mimeType='" + FOLDER_MIME + "' and '" + root +
                       "' in parents and trashed=false",
                   id, error))
        return error;
    cacheId("folders", folder, "");
    if (id.isEmpty()) return "trashed";
    JsonDocument answer;
    return jsonRequest("PATCH", "/drive/v3/files/" + id + "?fields=id", "{\"trashed\": true}", answer, error) == 200
               ? String("trashed")
               : error;
}

}  // namespace drive
