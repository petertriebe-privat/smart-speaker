#pragma once

#include <Arduino.h>

namespace textutil {

// "Mosel-Radtour 2027" -> "mosel-radtour-2027", "Jörg" -> "joerg": ASCII lower case with dashes,
// at most `maxLength` characters (IMAP searches subjects for it, ids of people and topics).
inline String slug(const String& text, size_t maxLength)
{
    String key;
    for (size_t i = 0; i < text.length() && key.length() < maxLength; ++i) {
        const uint8_t c = text[i];
        if (isalnum(c)) key += char(tolower(c));
        else if (c == 0xC3 && i + 1 < text.length()) {
            const uint8_t next = text[++i];
            if (next == 0xA4 || next == 0x84) key += "ae";
            else if (next == 0xB6 || next == 0x96) key += "oe";
            else if (next == 0xBC || next == 0x9C) key += "ue";
            else if (next == 0x9F) key += "ss";
        } else if (key.length() && !key.endsWith("-")) key += '-';
    }
    while (key.endsWith("-")) key.remove(key.length() - 1);
    return key;
}

}  // namespace textutil
