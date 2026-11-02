#pragma once

#include <Arduino.h>

// OpenAI rejects a request with invalid UTF-8 ("unicode decode error") or stray control
// characters ("failed to parse JSON value"). A byte-wise substring() can cut a character in
// half (ä is 2 bytes), and mails may come in other charsets or with ReadyMail's garbage.
namespace utf8 {

// At most `maxBytes` of `text`, never ending inside a character.
inline String prefix(const String& text, size_t maxBytes)
{
    if (text.length() <= maxBytes) return text;
    size_t end = maxBytes;
    while (end > 0 && (uint8_t(text[end]) & 0xC0) == 0x80) --end;  // continuation byte: back off
    return text.substring(0, end);
}

// False for bytes that never occur in text: F5..FF (not UTF-8 at all), control characters
// other than tab and line breaks, DEL. Chunks of a longer text may split a character, so
// continuation bytes and cut sequences at the edges are fine.
inline bool looksLikeText(const uint8_t* data, size_t length)
{
    for (size_t i = 0; i < length; ++i) {
        const uint8_t c = data[i];
        if (c >= 0xF5 || c == 0x7F || (c < 0x20 && c != '\t' && c != '\n' && c != '\r')) return false;
    }
    return true;
}

// Replaces invalid byte sequences and control characters (other than tab and line breaks) with
// '?'. Returns how many bytes were replaced.
inline size_t repair(char* text, size_t size)
{
    size_t replaced = 0;
    for (size_t i = 0; i < size;) {
        const uint8_t c = text[i];
        if (c == 0x7F || (c < 0x20 && c != '\t' && c != '\n' && c != '\r')) {
            text[i++] = '?';
            ++replaced;
            continue;
        }
        size_t length = c < 0x80 ? 1 : (c & 0xE0) == 0xC0 ? 2 : (c & 0xF0) == 0xE0 ? 3 : (c & 0xF8) == 0xF0 ? 4 : 0;
        bool valid = length > 0 && (length == 1 || c >= 0xC2) && i + length <= size;
        for (size_t k = 1; valid && k < length; ++k) valid = (uint8_t(text[i + k]) & 0xC0) == 0x80;
        if (valid) {
            i += length;
            continue;
        }
        text[i++] = '?';
        ++replaced;
    }
    return replaced;
}

inline size_t repair(String& text) { return repair(text.begin(), text.length()); }

}  // namespace utf8
