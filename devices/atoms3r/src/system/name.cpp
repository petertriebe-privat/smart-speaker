#include "system/name.h"

#include "config.h"

namespace name {

namespace {

// German umlauts as two letters, everything else that is no letter or digit as one '_'.
String makeSlug(const char* in) {
    String out;
    for (const char* p = in; *p; ++p) {
        const unsigned char c = *p;
        if (c == 0xC3 && p[1]) {  // UTF-8 ä ö ü ß Ä Ö Ü
            const unsigned char d = *++p;
            const char* rep = d == 0xA4 || d == 0x84 ? "ae" : d == 0xB6 || d == 0x96 ? "oe"
                            : d == 0xBC || d == 0x9C ? "ue" : d == 0x9F ? "ss" : nullptr;
            if (rep) {
                out += rep;
                continue;
            }
        }
        if (isalnum(c) && c < 0x80) {
            out += char(tolower(c));
        } else if (out.length() && !out.endsWith("_")) {
            out += '_';
        }
    }
    while (out.endsWith("_")) out.remove(out.length() - 1);
    return out.length() ? out : String("assistant");
}

}  // namespace

const String& slug() {
    static const String value = makeSlug(ASSISTANT_NAME);
    return value;
}

bool isName(String word) {
    word.trim();
    while (word.length() && strchr(".,!?;:", word[word.length() - 1])) word.remove(word.length() - 1);
    word.toLowerCase();
    if (!word.length()) return false;
    if (word == "jarvis") return true;
    String name = ASSISTANT_NAME;
    name.toLowerCase();
    int start = 0;
    while (start <= (int)name.length()) {
        int end = name.indexOf(' ', start);
        if (end < 0) end = name.length();
        if (name.substring(start, end) == word) return true;
        start = end + 1;
    }
    return false;
}

}  // namespace name
