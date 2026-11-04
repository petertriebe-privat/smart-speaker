#pragma once

#include <Arduino.h>

// Who is at home, from Home Assistant's person entities (the HA app on the phones). Fetched
// about once a minute by the tool registry's worker (LAN, no TLS) and matched to the people
// register (assistant/people) by id or name: person.alex -> alex.
namespace presence {

// Registers the background fetch (assistant/tools). Nothing without HA_URL / HA_TOKEN.
void begin();

enum class Where { Unknown, Home, Away };
// Where a person of the register is; Unknown without an HA person or with stale data.
Where of(const String& person);
// For the prompts: "Anwesenheit laut Home Assistant (…): zu Hause: Tom; unterwegs: Anna (Arbeit).",
// "" without data.
String text();

}  // namespace presence
