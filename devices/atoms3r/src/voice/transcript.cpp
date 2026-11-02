#include "voice/transcript.h"

#include "config.h"
#include "system/utf8.h"

namespace transcript {
namespace {

constexpr size_t DISPLAY_CHARS = 160;
constexpr uint32_t LINE_PAUSE_MS = 1500;

String shown;  // for the display
String line;   // for the log, not yet printed
String all;    // for the memory
Speaker last = Speaker::None;
String userName = "Du";  // the other side of the conversation
uint32_t lastAppend = 0;

// The display font is ASCII only: fold German umlauts, drop other UTF-8.
void appendAscii(String& out, const char* text)
{
    for (const char* p = text; *p; ++p) {
        const uint8_t c = *p;
        if (c < 0x80) {
            out += char(c);
            continue;
        }
        if (c != 0xC3 || !p[1]) continue;
        switch (uint8_t(*++p)) {
            case 0xA4: out += "ae"; break;
            case 0xB6: out += "oe"; break;
            case 0xBC: out += "ue"; break;
            case 0x84: out += "Ae"; break;
            case 0x96: out += "Oe"; break;
            case 0x9C: out += "Ue"; break;
            case 0x9F: out += "ss"; break;
        }
    }
}

}  // namespace

void clear()
{
    flush();
    shown = "";
    all = "";
    last = Speaker::None;
}

void append(Speaker speaker, const char* text)
{
    if (speaker != last) {
        flush();
        shown += speaker == Speaker::User ? "\n> " : "\n";
        last = speaker;
    }
    line += text;
    lastAppend = millis();
    appendAscii(shown, text);
    if (shown.length() > DISPLAY_CHARS) shown.remove(0, shown.length() - DISPLAY_CHARS);
}

void flush()
{
    if (line.isEmpty()) return;
    line.trim();
    Serial.printf("[%s] %s\n", last == Speaker::User ? userName.c_str() : ASSISTANT_NAME "", line.c_str());
    if (all.length() < MEMORY_TRANSCRIPT_CHARS)
        all += (last == Speaker::User ? userName + ": " : String(ASSISTANT_NAME ": ")) + utf8::prefix(line, MEMORY_TRANSCRIPT_CHARS - all.length()) + "\n";
    line = "";
}

void update()
{
    if (!line.isEmpty() && millis() - lastAppend > LINE_PAUSE_MS) flush();
}

Speaker lastSpeaker() { return last; }
const String& text() { return shown; }
const String& full() { return all; }
void setUserName(const char* name) { userName = name; }

}  // namespace transcript
