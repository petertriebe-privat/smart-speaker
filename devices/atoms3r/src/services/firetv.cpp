#include "services/firetv.h"

#include <ArduinoJson.h>
#include <vector>

#include "assistant/tools.h"
#include "services/home_assistant.h"
#include "services/psram_json.h"

namespace firetv {
namespace {

// The sticks as HA knows them: "alexa" are Alexa's devices whose model names a Fire TV and that
// are reachable (an entity not unavailable), "adb" the media players of the Android Debug Bridge.
constexpr const char* TVS_TEMPLATE =
    "{% set ns = namespace(alexa=[], adb=[], seen=[]) %}"
    "{% for e in integration_entities('alexa_devices') %}{% set d = device_id(e) %}"
    "{% if d and d not in ns.seen and 'fire tv' in (device_attr(d, 'model') or '') | lower %}"
    "{% set ns.seen = ns.seen + [d] %}"
    "{% if device_entities(d) | reject('is_state', 'unavailable') | list | count > 0 %}"
    "{% set ns.alexa = ns.alexa + [{'name': device_attr(d, 'name_by_user') or device_attr(d, 'name'),"
    " 'area': area_name(d) or '', 'device': d}] %}"
    "{% endif %}{% endif %}{% endfor %}"
    "{% for e in integration_entities('androidtv') | select('match', 'media_player[.]') %}"
    "{% set ns.adb = ns.adb + [{'name': state_attr(e, 'friendly_name'), 'area': area_name(e) or '', 'player': e}] %}"
    "{% endfor %}{{ {'alexa': ns.alexa, 'adb': ns.adb} | tojson }}";

// Shell commands through androidtv.adb_command. The integration's own app detection, app start
// (select_source) and key names fail on Fire OS 8 ("dumpsys window windows" lost mCurrentFocus),
// its shell works. Every command stays short: one that ran ~10 s lost its answer.
constexpr const char* STATUS_COMMAND =
    "dumpsys power | grep mWakefulness=; dumpsys window | grep mCurrentFocus; "
    "dumpsys media_session | grep -E 'package=|state=PlaybackState'; "
    "dumpsys activity activities | grep 'cmp=android/com.android.internal.app.ResolverActivity' | head -1";
constexpr const char* WAKE_COMMAND =
    "if dumpsys power 2>/dev/null | grep -qE 'mWakefulness=(Asleep|Dozing)'; then "
    "input keyevent 224; input keyevent 3; echo woken; fi";
constexpr const char* APPS_COMMAND =
    "cmd package query-activities --brief -a android.intent.action.MAIN "
    "-c android.intent.category.LEANBACK_LAUNCHER | grep / | cut -d/ -f1 | sort -u";
// One line per labelled or focused element: "focused|text|content-desc" (uiautomator keeps this
// attribute order).
constexpr const char* SCREEN_COMMAND =
    "uiautomator dump /sdcard/assistant_ui.xml >/dev/null 2>&1; "
    "sed 's/<node/\\n<node/g' /sdcard/assistant_ui.xml | "
    "sed -n 's/.* text=\"\\([^\"]*\\)\".* content-desc=\"\\([^\"]*\\)\".* focused=\"\\([a-z]*\\)\".*/\\3|\\1|\\2/p' | "
    "grep -v '^false||$'";

constexpr uint8_t MAX_REPEAT = 10;
constexpr uint8_t ALEXA_POLLS = 8;            // after an Alexa command: look this often ...
constexpr uint16_t ALEXA_POLL_MS = 800;       // ... at this interval (plus ~1 s per look)
constexpr uint8_t MAX_PRESSES = 2;            // OK presses on an app that does not start playing
constexpr uint16_t SCREEN_MAX_CHARS = 1500;   // screen text for the model

// One stick: the Alexa device id and/or the ADB media player ("" = not in that integration).
struct Tv {
    String name, area, alexa, player;
};

// Remote keys of the tool and their Android key codes.
struct Key {
    const char* name;
    uint8_t code;
};
constexpr Key KEYS[] = {
    {"home", 3},  {"back", 4},   {"up", 19},     {"down", 20},       {"left", 21},          {"right", 22},
    {"select", 23}, {"menu", 82}, {"play_pause", 85}, {"rewind", 89}, {"fast_forward", 90},
};

// Names for packages that do not say what they are (generic Fire TV apps, not configuration).
struct AppName {
    const char* package;
    const char* name;
};
constexpr AppName APP_NAMES[] = {
    {"com.amazon.firebat", "Prime Video"},
    {"com.amazon.bueller.music", "Amazon Music"},
    {"com.amazon.firetv.youtube", "YouTube"},
    {"com.amazon.spiderpork", "Silk Browser"},
    {"com.amazon.tv.launcher", "home screen"},
    {"com.amazon.ftv.screensaver", "screensaver"},
    {"com.amazon.vizzini", "Alexa overlay"},
    {"com.netflix.ninja", "Netflix"},
    {"com.disney.disneyplus", "Disney+"},
    {"com.apple.atve.amazon.appletv", "Apple TV"},
    {"de.cbc.tvnow.firetv", "RTL+"},
    {"de.telekom.magentatv.firetv", "MagentaTV"},
};

// What STATUS_COMMAND found on a stick.
struct State {
    String power;      // on, screensaver, standby ("" = unknown)
    String front;      // package in front
    String playing;    // package playing (or paused, see `paused`)
    bool paused = false;
    long position = -1;  // ms into what plays
    String chooser;    // link of an open "Open with" dialog
};

String lower(String text)
{
    text.trim();
    text.toLowerCase();  // ASCII only; umlauts compare byte by byte
    return text;
}

std::vector<String> lines(const String& text)
{
    std::vector<String> result;
    int start = 0;
    while (start < (int)text.length()) {
        int end = text.indexOf('\n', start);
        if (end < 0) end = text.length();
        String line = text.substring(start, end);
        line.trim();
        if (line.length()) result.push_back(line);
        start = end + 1;
    }
    return result;
}

String appName(const String& package)
{
    for (const AppName& app : APP_NAMES)
        if (package == app.package) return app.name;
    return package;
}

String names(const std::vector<Tv>& tvs)
{
    String text;
    for (const Tv& tv : tvs)
        text += (text.length() ? ", " : "") + tv.name + (tv.area.length() ? " (" + tv.area + ")" : "");
    return text;
}

// The ADB player that is the same stick as an Alexa device: same room, else same name.
bool sameStick(const Tv& alexa, const Tv& adb)
{
    if (alexa.area.length() && adb.area.length()) return lower(alexa.area) == lower(adb.area);
    return lower(alexa.name) == lower(adb.name);
}

bool loadTvs(std::vector<Tv>& tvs, String& error)
{
    JsonDocument request, response;
    request["template"] = TVS_TEMPLATE;
    String payload;
    serializeJson(request, payload);
    const int status = ha::rest("POST", "/api/template", payload, response);
    if (status != 200) {
        error = ha::failure(status, response);
        return false;
    }
    for (JsonObjectConst item : response["alexa"].as<JsonArrayConst>())
        tvs.push_back(Tv{item["name"] | "", item["area"] | "", item["device"] | ""});
    for (JsonObjectConst item : response["adb"].as<JsonArrayConst>()) {
        const Tv adb{item["name"] | "", item["area"] | "", "", item["player"] | ""};
        Tv* stick = nullptr;
        for (Tv& tv : tvs)
            if (tv.player.isEmpty() && sameStick(tv, adb)) { stick = &tv; break; }
        if (!stick) { tvs.push_back(adb); continue; }
        stick->player = adb.player;
        if (stick->area.isEmpty()) stick->area = adb.area;
    }
    if (tvs.empty()) {
        error = "Error: Home Assistant reports no reachable Fire TV (integrations Alexa Devices, Android Debug Bridge).";
        return false;
    }
    return true;
}

// The stick meant by `wanted`: its exact name, else the only one in that room or with the name
// part. Nullptr with `error` otherwise.
const Tv* find(const std::vector<Tv>& tvs, const String& wanted, String& error)
{
    const String key = lower(wanted);
    for (const Tv& tv : tvs)
        if (lower(tv.name) == key) return &tv;
    std::vector<const Tv*> hits;
    for (const Tv& tv : tvs)
        if (key.length() && (lower(tv.area) == key || lower(tv.name).indexOf(key) >= 0)) hits.push_back(&tv);
    if (hits.size() == 1) return hits[0];
    if (key.isEmpty()) error = "Error: device missing. Fire TVs: " + names(tvs);
    else if (hits.empty()) error = "Error: no Fire TV \"" + wanted + "\". Fire TVs: " + names(tvs);
    else {
        std::vector<Tv> several;
        for (const Tv* tv : hits) several.push_back(*tv);
        error = "Error: several Fire TVs match \"" + wanted + "\": " + names(several) + ". Name one.";
    }
    return nullptr;
}

String call(const char* service, JsonDocument& data)
{
    String payload;
    serializeJson(data, payload);
    JsonDocument response;
    const int status = ha::rest("POST", String("/api/services/") + service, payload, response);
    return status == 200 ? "" : ha::failure(status, response);
}

// Runs a shell command on the stick; its output is the player's attribute adb_response afterwards
// (the action returns only when the command is done). HA keeps the old answer when a command
// prints nothing or times out, so every command ends with its own marker.
bool shell(const Tv& tv, const String& command, String& output, String& error)
{
    static uint32_t calls = 0;
    const String marker = "@end" + String(++calls) + "-" + String(millis());
    JsonDocument data;
    data["entity_id"] = tv.player;
    data["command"] = command + "; echo " + marker;
    error = call("androidtv/adb_command", data);
    if (error.length()) return false;
    JsonDocument filter, response(PsramJson::instance());
    filter["state"] = true;
    filter["attributes"]["adb_response"] = true;
    const int status = ha::rest("GET", "/api/states/" + tv.player, "", response, &filter);
    if (status != 200) {
        error = ha::failure(status, response);
        return false;
    }
    if (response["state"] == "unavailable") {
        error = "Error: " + tv.name + " does not answer over ADB (off the network, or ADB debugging off)";
        return false;
    }
    output = response["attributes"]["adb_response"] | "";
    output.trim();
    if (!output.endsWith(marker)) {
        error = "Error: no answer from " + tv.name + " (command took too long)";
        return false;
    }
    output.remove(output.length() - marker.length());
    return true;
}

String noAdb(const Tv& tv)
{
    return "Error: " + tv.name + " is not in the Android Debug Bridge integration of Home Assistant; "
           "use action alexa instead";
}

State parseState(const String& output)
{
    State state;
    String package;
    for (const String& line : lines(output)) {
        if (line.startsWith("mWakefulness=")) {
            const String value = line.substring(13);
            state.power = value == "Awake" ? "on" : value == "Dreaming" ? "screensaver" : "standby";
        } else if (line.startsWith("mCurrentFocus=")) {
            const int to = line.indexOf('/');
            const int from = line.indexOf(" u0 ");
            if (from >= 0) state.front = line.substring(from + 4, to > from ? to : line.lastIndexOf('}'));
        } else if (line.startsWith("package=")) {
            package = line.substring(8);
        } else if (line.startsWith("state=PlaybackState {state=")) {
            const int code = line.substring(27).toInt();  // 3 playing, 2 paused
            const bool better = code == 3 && (state.playing.isEmpty() || state.paused);
            if (better || (code == 2 && state.playing.isEmpty())) {
                state.playing = package;
                state.paused = code == 2;
                const int at = line.indexOf("position=");
                state.position = at >= 0 ? line.substring(at + 9).toInt() : -1;
            }
        } else if (line.indexOf("ResolverActivity") >= 0 && line.indexOf(" dat=") >= 0) {
            const int from = line.indexOf(" dat=") + 5;
            state.chooser = line.substring(from, line.indexOf(' ', from));
        }
    }
    return state;
}

bool readState(const Tv& tv, State& state, String& error)
{
    String output;
    if (!shell(tv, STATUS_COMMAND, output, error)) return false;
    state = parseState(output);
    return true;
}

String describe(const State& state)
{
    String text = state.power.length() ? state.power : "state unknown";
    if (state.front.length()) text += ", in front: " + appName(state.front);
    if (state.playing.length()) text += (state.paused ? ", paused: " : ", playing: ") + appName(state.playing);
    if (state.chooser.length()) text += ", an \"Open with\" dialog waits";
    return text;
}

String status(const std::vector<Tv>& tvs)
{
    String text;
    for (const Tv& tv : tvs) {
        text += tv.name + (tv.area.length() ? " (" + tv.area + ")" : "") + ": ";
        State state;
        String error;
        if (tv.player.isEmpty()) text += "state unknown (Alexa only)";
        else if (!readState(tv, state, error)) text += error;
        else text += describe(state);
        text += "\n";
    }
    return text;
}

String unescapeXml(String text)
{
    text.replace("&#10;", " ");
    text.replace("&quot;", "\"");
    text.replace("&apos;", "'");
    text.replace("&lt;", "<");
    text.replace("&gt;", ">");
    text.replace("&amp;", "&");
    return text;
}

// What the screen shows: the labelled elements in order, the focused one marked. A focused
// element without a label (a tile) marks the first label inside it.
String screen(const Tv& tv)
{
    if (tv.player.isEmpty()) return noAdb(tv);
    State state;
    String output, error;
    if (!readState(tv, state, error) || !shell(tv, SCREEN_COMMAND, output, error)) return error;
    String text = tv.name + ": " + describe(state) + "\nScreen (> = focused):\n", last;
    bool focusPending = false, focusShown = false;
    for (const String& line : lines(output)) {
        const int first = line.indexOf('|'), second = line.indexOf('|', first + 1);
        if (first < 0 || second < 0) continue;
        const bool focused = line.substring(0, first) == "true";
        const String label = unescapeXml(line.substring(first + 1, second)),
                     description = unescapeXml(line.substring(second + 1));
        String entry = label.length() ? label : description;
        if (label.length() && description.length() && lower(description) != lower(label))
            entry += " (" + description + ")";
        if (entry.isEmpty()) {
            if (focused) focusPending = true;
            continue;
        }
        const bool mark = !focusShown && (focused || focusPending);
        if (mark) focusShown = true, focusPending = false;
        if (entry == last && !mark) continue;
        last = entry;
        if (text.length() + entry.length() > SCREEN_MAX_CHARS) {
            text += "...\n";
            break;
        }
        text += (mark ? "> " : "- ") + entry + "\n";
    }
    if (!last.length()) text += "(nothing readable: video, Alexa overlay or an app drawing its own screen)\n";
    return text;
}

// The installed app meant by `wanted` (name or package): equal, else the only one containing it.
// "" with `error` otherwise, listing the apps.
String findApp(const String& output, const String& wanted, String& error)
{
    const String key = lower(wanted);
    String hit, list;
    bool several = false;
    for (const String& package : lines(output)) {
        const String name = appName(package);
        list += (list.length() ? ", " : "") + (name == package ? package : name + " (" + package + ")");
        if (lower(package) == key || lower(name) == key) return package;
        if (lower(package).indexOf(key) < 0 && lower(name).indexOf(key) < 0) continue;
        several = several || hit.length() > 0;
        hit = package;
    }
    if (hit.length() && !several) return hit;
    error = "Error: no single app \"" + wanted + "\". Apps: " + list;
    return "";
}

String openApp(const Tv& tv, const String& app);

// A link from the stick's own "Open with" dialog, opened in the installed app whose package
// contains the link's domain name (www.disneyplus.com -> com.disney.disneyplus). Only links of
// plain characters reach the shell. "" if it went, else why not.
String openLink(const Tv& tv, const String& link)
{
    for (const char c : link)
        if (!isalnum(c) && !strchr(":/._-?=&%#+~", c)) return "unusual link";
    const int hostStart = link.indexOf("://") + 3;
    String host = link.substring(hostStart, link.indexOf('/', hostStart));
    const int dot = host.lastIndexOf('.');
    if (hostStart < 3 || dot <= 0) return "no domain";
    host = host.substring(0, dot);
    const String domain = host.substring(host.lastIndexOf('.') + 1);
    String output, error;
    if (!shell(tv, APPS_COMMAND, output, error)) return error;
    String package;
    for (const String& line : lines(output))
        if (lower(line).indexOf(lower(domain)) >= 0) package = line;
    if (package.isEmpty()) return "no app for " + domain;
    if (!shell(tv, "am start -a android.intent.action.VIEW -d '" + link + "' -p " + package + " >/dev/null 2>&1",
               output, error))
        return error;
    return "";
}

// An app's own screen, not the home screen, Alexa's overlay, the screensaver or a system dialog.
bool inApp(const String& front)
{
    return front.length() && front != "com.amazon.tv.launcher" && front != "com.amazon.vizzini" &&
           front != "com.amazon.ftv.screensaver" && front != "android";
}

// Sends `text` to the stick's Alexa. With ADB it then watches the stick (each look is one short
// shell command): it opens a link Alexa left in an "Open with" dialog in the matching app, presses
// OK when an app stays in front without playing (profile selection: the preselected profile;
// details page: play or continue), and reports whether something new plays.
String alexaCommand(const Tv& tv, const String& text)
{
    if (tv.alexa.isEmpty()) return "Error: " + tv.name + " is not in Alexa Devices, Alexa commands are not possible";
    State before, now, last;
    String error, output;
    // A sleeping stick: Alexa would wake it, but not its TV; HOME after waking does (HDMI-CEC).
    // Woken in the same command as the first look.
    const bool watch = tv.player.length() && shell(tv, String(WAKE_COMMAND) + "; " + STATUS_COMMAND, output, error);
    const bool woken = watch && output.indexOf("woken") >= 0;
    if (watch) before = parseState(output);
    JsonDocument data;
    data["device_id"] = tv.alexa;
    data["text_command"] = text;
    error = call("alexa_devices/send_text_command", data);
    if (error.length()) return error;
    String result = (woken ? "Switched " + tv.name + " on. " : String()) + "Sent to Alexa on " + tv.name + ": " + text;
    if (!watch) return result;
    const uint32_t started = millis();

    uint8_t presses = 0;
    bool press = false, overlay = false;
    for (uint8_t poll = 0; poll < ALEXA_POLLS; poll++) {
        if (poll) Serial.printf("[Fire TV] %lu ms: %s\n", (unsigned long)(millis() - started), describe(now).c_str());
        if (press) {
            if (!shell(tv, String("input keyevent 23; sleep 2; ") + STATUS_COMMAND, output, error)) continue;
            presses++;
            press = false;
        } else {
            delay(ALEXA_POLL_MS);
            if (!shell(tv, STATUS_COMMAND, output, error)) continue;  // one slow answer is no reason to stop
        }
        last = now;
        now = parseState(output);
        if (now.front == "com.amazon.vizzini") overlay = true;
        if (now.chooser.length()) {
            const String why = openLink(tv, now.chooser);
            if (why.length()) return result + ". An \"Open with\" dialog waits for " + now.chooser + " (" + why + ")";
            result += " (opened Alexa's link in the app)";
            continue;
        }
        const bool fresh = now.playing.length() && !now.paused &&
                           (now.playing != before.playing || before.paused || now.position < before.position);
        if (fresh)
            return result + ". Now playing: " + appName(now.playing) +
                   (presses ? " (pressed OK " + String(presses) + "x: preselected profile or play button)" : "");
        // Alexa is done once her overlay is gone; the old video playing on means she started nothing.
        const bool settled = (overlay || poll >= 2) && now.front != "com.amazon.vizzini";
        const bool oldPlays = now.playing.length() && !now.paused;
        if (settled && now.front == before.front && now.playing == before.playing && (!inApp(now.front) || oldPlays))
            return result + ". Nothing new started: Alexa probably asked back on the TV (e.g. which app); "
                            "repeat the command with the app ('... auf Netflix'). Now: " + describe(now);
        press = settled && inApp(now.front) && !oldPlays && now.front == last.front && presses < MAX_PRESSES;
    }
    return result + ". Not playing yet (pressed OK " + String(presses) + "x). Now: " + describe(now) +
           ". Look with screen, then use key.";
}

String openApp(const Tv& tv, const String& app)
{
    // Without ADB Alexa opens the app; the command is German like Alexa's language.
    if (tv.player.isEmpty()) return alexaCommand(tv, "Öffne " + app);
    String output, error;
    if (!shell(tv, APPS_COMMAND, output, error)) return error;
    const String package = findApp(output, app, error);  // from the stick's own list: safe in the shell
    if (package.isEmpty()) return error;
    if (!shell(tv, "monkey -p " + package + " -c android.intent.category.LEANBACK_LAUNCHER 1 2>&1 | grep -c 'Events injected'",
               output, error))
        return error;
    output.trim();
    return output == "1" ? tv.name + " opens " + appName(package) : "Error: " + appName(package) + " did not start";
}

// On: WAKE_COMMAND. An awake stick is left alone, HOME would leave the app in front.
String power(const Tv& tv, bool on)
{
    if (tv.player.isEmpty()) return noAdb(tv);
    String output, error;
    if (!shell(tv, on ? WAKE_COMMAND : "input keyevent 223", output, error)) return error;
    if (!on) return tv.name + " switched off";
    return output.indexOf("woken") >= 0 ? tv.name + " switched on"
                                      : tv.name + " was already on (if its TV is dark: key home)";
}

String key(const Tv& tv, const String& name, int repeat)
{
    if (tv.player.isEmpty()) return noAdb(tv);
    const Key* key = nullptr;
    for (const Key& entry : KEYS)
        if (name == entry.name) key = &entry;
    if (!key) return "Error: unknown key \"" + name + "\"";
    repeat = constrain(repeat, 1, MAX_REPEAT);
    String command = "input keyevent", output, error;
    for (int i = 0; i < repeat; i++) command += " " + String(key->code);
    if (!shell(tv, command, output, error)) return error;
    return tv.name + ": " + name + (repeat > 1 ? " x" + String(repeat) : "");
}

String run(JsonObjectConst arguments, const tools::Context&)
{
    const String action = arguments["action"] | "", text = arguments["text"] | "";
    std::vector<Tv> tvs;
    String error;
    if (!loadTvs(tvs, error)) return error;
    if (action == "status") return status(tvs);
    if (action != "alexa" && action != "open_app" && action != "on" && action != "off" && action != "key" &&
        action != "screen")
        return "Error: action is status, alexa, open_app, on, off, key or screen";
    const Tv* tv = find(tvs, arguments["device"] | "", error);
    if (!tv) return error;
    if (action == "on" || action == "off") return power(*tv, action == "on");
    if (action == "screen") return screen(*tv);
    if (text.isEmpty()) return "Error: text missing";
    if (action == "alexa") return alexaCommand(*tv, text);
    if (action == "open_app") return openApp(*tv, text);
    return key(*tv, text, arguments["repeat"] | 1);
}

}  // namespace

void addTools()
{
    if (!ha::configured()) return;
    tools::add(tools::Tool{
        "fire_tv",
        "Controls the Amazon Fire TV sticks at home through Home Assistant. action status: every stick with "
        "power, the app in front and what plays (device not needed). alexa: sends German text to the stick's own "
        "Alexa as if the user had said it; the way to play a film or series by title. Always name the app ('Spiele "
        "Stranger Things auf Netflix'): without one Alexa asks back on the TV and nobody can answer. If the user "
        "names no app, ask, or take the one you are fairly sure has it. It presses OK by itself when the app "
        "stops at a profile selection (takes the preselected profile) or a details page (play/continue), and says "
        "whether it plays. It also switches a sleeping stick and its TV on: no status or on before it. 'Now "
        "playing' means done: do not check again. If not playing: look with screen and go on with key. Say "
        "'Moment' first, it takes up to 20 s. "
        "screen: the texts and buttons on the screen, the focused one marked. key: press a remote key (text: "
        "home, back, up, down, left, right, select, menu, play_pause, rewind, fast_forward; repeat for several "
        "presses); with screen you can operate any app, e.g. another profile when the user asks for it. "
        "open_app: start an app by name or package (text, e.g. "
        "'Netflix'); an unclear name returns the installed apps. on/off: wake the stick and its TV, or put both "
        "to sleep (HDMI-CEC). YouTube videos go through play_video instead. device: the stick's name or its "
        "room; an unclear device returns the list.",
        R"json({"type": "object", "additionalProperties": false,
            "properties": {"device": {"type": "string", "description": "e.g. 'Fire TV Wohnzimmer' or 'Wohnzimmer'; '' for status"},
                           "action": {"type": "string", "enum": ["status", "alexa", "screen", "key", "open_app", "on", "off"]},
                           "text": {"type": "string", "description": "alexa: the German command without 'Alexa'; open_app: the app; key: the key; else ''"},
                           "repeat": {"type": "integer", "description": "key: how often, 1-10"}},
            "required": ["device", "action", "text", "repeat"]})json",
        run, tools::Slow, tools::Everywhere});
}

}  // namespace firetv
