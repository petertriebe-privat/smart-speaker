#include "vault.h"

#include <string.h>

namespace vault {
namespace {

constexpr size_t NAME_MAX_BYTES = 120;  // a file name without extension

String timeName(const String& at)
{
    String name = at;
    name.replace(":", ".");
    return name;
}

String quote(const String& text)
{
    String out = "\"";
    for (size_t i = 0; i < text.length(); ++i) {
        const char c = text[i];
        if (c == '\\' || c == '"') out += '\\';
        out += c == '\n' ? ' ' : c;
    }
    return out + "\"";
}

void property(String& out, const char* key, const String& value, bool plain = false)
{
    if (value.length()) out += String(key) + ": " + (plain ? value : quote(value)) + "\n";
}

// A list property from "a, b"; `links`: each item as a wikilink.
void listProperty(String& out, const char* key, const String& items, bool links = false)
{
    String lines;
    int start = 0;
    while (start <= int(items.length())) {
        int comma = items.indexOf(',', start);
        if (comma < 0) comma = items.length();
        String item = items.substring(start, comma);
        item.trim();
        if (item.length()) lines += "  - " + quote(links ? link(item, item) : item) + "\n";
        start = comma + 1;
    }
    if (lines.length()) out += String(key) + ":\n" + lines;
}

String isoTime(const String& at)
{
    String out = at;
    const int space = out.indexOf(' ');
    if (space >= 0) out.setCharAt(space, 'T');
    return out;
}

// "--- 2026-11-14 18:38 (heading) ---" of the old format as an entry heading; "" if it is none.
String legacyEntry(const String& line)
{
    if (!line.startsWith("--- ") || !line.endsWith(" ---") || line.length() < 24) return "";
    const String at = line.substring(4, 20);
    if (at[4] != '-' || at[10] != ' ' || at[13] != ':') return "";
    String heading = line.substring(20, line.length() - 4);
    heading.trim();
    if (heading.length() && !(heading.startsWith("(") && heading.endsWith(")"))) return "";
    if (heading.length()) heading = heading.substring(1, heading.length() - 1);
    const String from = heading.isEmpty() || heading == "Nächtliche Durchsicht" ? String() : conversationName(at, heading);
    String converted = entry(at, heading, from, "");
    while (converted.endsWith("\n")) converted.remove(converted.length() - 1);
    return converted;
}

}  // namespace

String fileName(const String& text)
{
    String in = text;
    in.replace(": ", " – ");
    String out;
    out.reserve(in.length());
    bool blank = true;  // collapses runs of blanks, drops leading ones
    for (size_t i = 0; i < in.length(); ++i) {
        const char c = in[i];
        if (uint8_t(c) <= ' ' || strchr(":[]#^|\\/*?\"<>", c)) {
            if (!blank) out += ' ';
            blank = true;
        } else {
            out += c;
            blank = false;
        }
    }
    out.trim();
    if (out.length() > NAME_MAX_BYTES) {
        size_t cut = NAME_MAX_BYTES;
        while (cut > 0 && (uint8_t(out[cut]) & 0xC0) == 0x80) --cut;
        out = out.substring(0, cut);
        out.trim();
    }
    // Obsidian hides a leading dot, Windows drops a trailing one.
    while (out.length() && (out[0] == '.' || out[0] == ' ')) out.remove(0, 1);
    while (out.length() && (out[out.length() - 1] == '.' || out[out.length() - 1] == ' ')) out.remove(out.length() - 1);
    return out.length() ? out : String("Notiz");
}

String link(const String& name, const String& label)
{
    const String target = fileName(name);
    String text = label;
    text.replace("]]", "");
    text.replace("|", " ");
    text.replace("\n", " ");
    if (text.isEmpty() || text == target) return "[[" + target + "]]";
    return "[[" + target + "|" + text + "]]";
}

String conversationName(const String& at, const String& title)
{
    return fileName(title.isEmpty() ? timeName(at) : timeName(at) + " – " + title);
}

String dreamName(const String& day) { return fileName("Traum " + day); }

String nameFromSubject(const String& subject)
{
    if (subject.startsWith("Thema [")) {
        const int end = subject.indexOf("]: ");
        if (end > 0) return fileName(subject.substring(end + 3));
    }
    if (subject.startsWith("Gespräch ")) {
        const String rest = subject.substring(strlen("Gespräch "));
        if (rest.length() >= 16) {
            String title = rest.substring(16);
            title.trim();
            if (title.startsWith("–")) title = title.substring(strlen("–"));
            title.trim();
            return conversationName(rest.substring(0, 16), title);
        }
    }
    return fileName(subject);
}

String conversationNote(const String& at, const String& title, const String& with, const String& channel,
                        const String& summary, const String& keywords, const String& topics, const String& transcript)
{
    String text = "---\n";
    property(text, "typ", "Gespräch");
    property(text, "datum", isoTime(at), true);
    property(text, "mit", with.length() ? link(with, with) : String());
    property(text, "kanal", channel);
    listProperty(text, "themen", topics, true);
    listProperty(text, "stichwörter", keywords);
    listProperty(text, "tags", "gespräch");
    text += "---\n# " + (title.length() ? title : "Gespräch vom " + at) + "\n\n";
    text += summary.length() ? "**Kurzfassung:** " + summary + "\n\n" : "Gespräch vom " + at + ", nicht zusammengefasst.\n\n";
    return text + "## Transkript\n\n" + transcript;
}

String dreamNote(const String& day, const String& diary, const String& report)
{
    String text = "---\n";
    property(text, "typ", "Traum");
    property(text, "datum", day, true);
    listProperty(text, "tags", "traum");
    String d = diary, r = report;
    d.trim();
    r.trim();
    text += "---\n# Traum vom " + day + "\n\nNächtliche Durchsicht des Gedächtnisses.\n\n" + d + "\n";
    if (r.length()) text += "\n" + r + "\n";
    return text;
}

String topicHeader(const String& key, const String& title, const String& status, const String& keywords,
                   const String& updated, const String& created, bool done)
{
    String text = "---\n";
    property(text, "typ", "Thema");
    property(text, "schlüssel", key);
    property(text, "status", done ? "erledigt" : "offen");
    property(text, "stand", updated, true);
    property(text, "angelegt", created, true);
    listProperty(text, "stichwörter", keywords);
    listProperty(text, "tags", "thema");
    return text + "---\n# " + title + "\n\n**Stand (" + updated + "):** " + status + "\n";
}

String entry(const String& at, const String& heading, const String& from, const String& text)
{
    String out = String(ENTRY_PREFIX + 1) + at + (heading.length() ? " – " + heading : String()) + "\n";
    if (from.length()) out += "Aus: " + link(from, heading) + "\n";
    return out + text + "\n";
}

String history(const String& dossier)
{
    int at = dossier.indexOf(HISTORY_MARKER);
    if (at >= 0) return dossier.substring(at + strlen(HISTORY_MARKER));
    at = dossier.indexOf(LEGACY_HISTORY_MARKER);
    if (at < 0) return "";
    const String old = dossier.substring(at + strlen(LEGACY_HISTORY_MARKER));
    String out;
    out.reserve(old.length() + 256);
    for (int start = 0; start < int(old.length());) {
        int end = old.indexOf('\n', start);
        if (end < 0) end = old.length();
        const String line = old.substring(start, end);
        const String converted = legacyEntry(line);
        out += converted.length() ? converted : line;
        if (end < int(old.length())) out += '\n';
        start = end + 1;
    }
    return out;
}

int headerEnd(const String& dossier)
{
    const int at = dossier.indexOf(HISTORY_MARKER);
    return at >= 0 ? at : dossier.indexOf(LEGACY_HISTORY_MARKER);
}

String body(const String& text)
{
    if (!text.startsWith("---\n")) return text;
    const int end = text.indexOf("\n---\n", 4);
    if (end < 0) return text;
    int start = end + 5;
    while (start < int(text.length()) && text[start] == '\n') ++start;
    return text.substring(start);
}

String head(const String& text)
{
    const String rest = body(text);
    String out;
    int lines = 0;
    for (int start = 0; start < int(rest.length()) && lines < 2;) {
        int end = rest.indexOf('\n', start);
        if (end < 0) end = rest.length();
        String line = rest.substring(start, end);
        String trimmed = line;
        trimmed.trim();
        if (trimmed.length()) {
            out += (lines ? "\n" : "") + line;
            ++lines;
        }
        start = end + 1;
    }
    return out;
}

}  // namespace vault
