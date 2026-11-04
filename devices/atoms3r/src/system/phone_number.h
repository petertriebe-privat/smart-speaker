#pragma once

#include <Arduino.h>

namespace phonenumber {

// Numbers as the FritzBox writes them: +49171... / 0049171... / 0171... all as 0171...
// ("**610": the FritzBox's own phones).
inline String normalized(const String& number)
{
    String digits;
    for (size_t i = 0; i < number.length(); ++i)
        if (isdigit(uint8_t(number[i])) || number[i] == '*' || (number[i] == '+' && digits.isEmpty())) digits += number[i];
    if (digits.startsWith("+")) digits = "00" + digits.substring(1);
    if (digits.startsWith("0049")) digits = "0" + digits.substring(4);
    return digits;
}

}  // namespace phonenumber
