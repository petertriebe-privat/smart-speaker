#include "voice/quick_home.h"

#include <vector>

#include "config.h"
#include "system/name.h"
#include "services/home_assistant.h"

namespace quick_home {
namespace {

// Filler words that do not change the command (as the Echo's quickFiller).
const char* const FILLERS[] = {"hey", "bitte", "mal", "doch", "jetzt", "etwas", "noch", "ein",
                               "bisschen", "bißchen", "danke", "die", "der", "das", "dem", "den", "mach",
                               "machen", "okay", "ok", "ja", "sir", "mir", "uns", "einfach"};

// Word beginnings for what HA switches. Not the TV ("Fernseher aus" is fire_tv's) nor
// "hell"/"dunkel" ("Wann ist es dunkel heute?").
const char* const CUES[] = {"licht", "lamp", "leucht", "beleucht", "rollo", "rolllad", "rollad", "jalousi",
                            "markis", "heizung", "thermostat", "steckdos", "ventilat", "lüfter", "szene",
                            "dimm", "hellig"};

// Lower-case words; bytes of UTF-8 sequences count as letters (umlauts stay in the word).
std::vector<String> split(const String& text)
{
    std::vector<String> out;
    String word;
    for (size_t i = 0; i <= text.length(); ++i) {
        const char c = i < text.length() ? text[i] : ' ';
        if (isalnum(uint8_t(c)) || uint8_t(c) >= 0x80) {
            word += char(tolower(uint8_t(c)));
        } else if (word.length()) {
            out.push_back(word);
            word = "";
        }
    }
    return out;
}

bool filler(const String& word)
{
    for (const char* f : FILLERS)
        if (word == f) return true;
    return name::isName(word);
}

bool cue(const String& word)
{
    for (const char* c : CUES)
        if (word.startsWith(c)) return true;
    return false;
}

bool inList(const String& word, const char* const* list, size_t n)
{
    for (size_t i = 0; i < n; ++i)
        if (word == list[i]) return true;
    return false;
}

// The separable part of German switching verbs: HA wants a room before it ("Schalte das Licht im
// Wohnzimmer aus"); "… aus im Wohnzimmer" was no_valid_targets on the Echo (22.10.2026).
const char* const PARTICLES[] = {"an", "aus", "ein", "ab", "auf", "zu", "hoch", "runter"};

// What "<word> an/aus" means that is not a room's light (the TV is fire_tv's, music the music
// tools').
const char* const NOT_ROOMS[] = {"fernseher", "fernsehen", "tv", "musik", "radio", "video", "youtube", "netflix",
                                 "alexa", "alles"};

// This speaker's room into the words, before a closing particle.
String withRoom(const String& said, const char* room)
{
    const int space = said.lastIndexOf(' ');
    if (space > 0) {
        String last = said.substring(space + 1);
        last.toLowerCase();
        if (inList(last, PARTICLES, sizeof(PARTICLES) / sizeof(*PARTICLES)))
            return said.substring(0, space) + " im " + room + said.substring(space);
    }
    return said + " im " + room;
}

}  // namespace

bool bareRoom(const String& said)
{
    const std::vector<String> w = split(said);
    if (w.size() != 2 || filler(w[0]) || cue(w[0]) || inList(w[0], NOT_ROOMS, sizeof(NOT_ROOMS) / sizeof(*NOT_ROOMS)))
        return false;
    return w[1] == "an" || w[1] == "aus" || w[1] == "ein";
}

bool candidate(const String& text)
{
    String t = text;
    t.trim();
    if (t.isEmpty() || t.endsWith("?")) return false;
    if (bareRoom(words(t))) return true;  // "Küche aus": HA has no sentence for it, "Licht Küche aus" works
    int kept = 0;
    bool home = false;
    for (const String& word : split(t)) {
        if (filler(word)) continue;
        kept++;
        home = home || cue(word);
    }
    return home && kept <= 10;
}

bool namesPlace(const String& text)
{
    for (const String& word : split(text))
        if (word == "im" || word == "in" || word == "ins") return true;
    return false;
}

String words(const String& text)
{
    String t = text;
    t.trim();
    // the leading wake word and the assistant's "Ja, Sir" (HA's sentences do not allow them)
    for (;;) {
        const int space = t.indexOf(' ');
        String first = space < 0 ? t : t.substring(0, space);
        while (first.length() && strchr(".,!?;:", first[first.length() - 1])) first.remove(first.length() - 1);
        first.toLowerCase();
        if (space < 0 || (first != "hey" && !name::isName(first) && first != "ja" && first != "sir" && first != "okay" &&
                          first != "ok"))
            break;
        t = t.substring(space + 1);
        t.trim();
    }
    while (t.length() && (t.endsWith(".") || t.endsWith("!"))) t.remove(t.length() - 1);
    // "dir"/"mir" in the middle: "Schalt dir das Licht in der Küche aus" came from the Echo's
    // transcription and HA failed on it (22.10.2026)
    for (const char* drop : {" dir ", " mir ", " Dir ", " Mir "}) {
        int at;
        while ((at = t.indexOf(drop)) >= 0) t.remove(at, strlen(drop) - 1);
    }
    return t;
}

// HA takes the asking satellite's room when none is named; over its API there is none, and
// "Licht aus" came back as no_valid_targets on the Echo (21.10.2026). So this speaker's room goes
// first (withRoom), then the words alone (a device named in another room: "Schreibtischlampe
// an"). A bare room ("Küche aus") goes as that room's light.
Result run(const String& text, String& detail)
{
    if (!ha::configured() || !ha::reachable()) {
        detail = "Home Assistant not reachable";
        return Result::Unknown;
    }
    const String said = words(text);
    String tries[2] = {said, ""};
    if (bareRoom(said)) {
        tries[0] = "Licht " + said;
    } else if (*DEVICE_ROOM && !namesPlace(said)) {
        tries[0] = withRoom(said, DEVICE_ROOM);
        tries[1] = said;
    }
    ha::AssistResult r;
    for (const String& sentence : tries) {
        if (sentence.isEmpty()) break;
        r = ha::AssistResult{};
        const uint32_t started = millis();
        const bool answered = ha::assist(sentence, r, QUICK_HOME_TIMEOUT_MS);
        Serial.printf("\n[Quick] home \"%s\": %s %s \"%s\", %d done, %d failed (%lu ms)\n", sentence.c_str(),
                      answered ? r.type.c_str() : "no answer", r.code.c_str(), r.speech.c_str(), r.done, r.failed,
                      millis() - started);
        if (!answered) {
            detail = r.code;
            return Result::Unknown;
        }
        if (r.type != "error") break;
    }
    detail = r.speech;
    if (r.type == "action_done" && r.failed > 0) {
        detail = r.speech + " (" + String(r.failed) + " of " + String(r.failed + r.done) + " devices failed)";
        return Result::Failed;
    }
    // an error (no match, no such device) or a question's answer: GPT Live takes it
    return r.type == "action_done" ? Result::Done : Result::Unknown;
}

void selfTest()
{
    struct Case {
        const char* text;
        bool candidate, place;
    };
    static const Case CASES[] = {
        {"Licht in der Küche aus.", true, true},
        {"Hey Jarvis, mach bitte das Licht an", true, false},
        {"Rollos im Wohnzimmer runter", true, true},
        {"Stell die Heizung auf 21 Grad", true, false},
        {"Ist das Licht in der Küche an?", false, true},  // a question: GPT Live
        {"Fernseher aus", false, false},                 // fire_tv, not HA's sentences
        {"Wann ist es dunkel heute", false, false},
        {"Pause", false, false},
        {ASSISTANT_NAME ", Küche aus", true, false},  // a bare room: its light
        {"Musik aus", false, false},
        {"Alles aus", false, false},
        {"Erzähl mir, wie man das Licht in einem Bild so setzt, dass es warm und weich wirkt", false, true},
    };
    int misses = 0;
    for (const Case& c : CASES) {
        if (candidate(c.text) != c.candidate || namesPlace(c.text) != c.place) {
            misses++;
            Serial.printf("[Quick test] miss: \"%s\" candidate %d place %d\n", c.text, candidate(c.text),
                          namesPlace(c.text));
        }
    }
    if (words("Hey Jarvis, Licht in der Küche aus.") != "Licht in der Küche aus") {
        misses++;
        Serial.printf("[Quick test] miss: words \"%s\"\n", words("Hey Jarvis, Licht in der Küche aus.").c_str());
    }
    if (words("Ja, Sir. Rollos runter!") != "Rollos runter") {
        misses++;
        Serial.printf("[Quick test] miss: words \"%s\"\n", words("Ja, Sir. Rollos runter!").c_str());
    }
    const struct {
        String got;
        const char* want;
    } EXPECT[] = {
        {words("Schalt dir das Licht in der Küche aus."), "Schalt das Licht in der Küche aus"},
        {withRoom("Schalte das Licht ein", "Wohnzimmer"), "Schalte das Licht im Wohnzimmer ein"},
        {withRoom("Dimm die Lampe", "Wohnzimmer"), "Dimm die Lampe im Wohnzimmer"},
    };
    for (const auto& e : EXPECT) {
        if (e.got != e.want) {
            misses++;
            Serial.printf("[Quick test] miss: \"%s\", want \"%s\"\n", e.got.c_str(), e.want);
        }
    }
    if (misses) Serial.printf("[Quick test] %d misses\n", misses);
    else Serial.println("[Quick test] ok");
}

}  // namespace quick_home
