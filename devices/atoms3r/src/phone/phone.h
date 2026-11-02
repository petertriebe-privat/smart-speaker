#pragma once

#include <Arduino.h>

// The assistant as an IP phone of the FritzBox (SIP over UDP in the LAN, SIP_USER / SIP_PASSWORD in
// secrets.h): he registers at PHONE_REGISTRAR and answers every call, talking through a GPT Live
// session whose audio runs over the call (phone/rtp). Released numbers (PHONE_CALLERS; without a
// list only the FritzBox's internal phones "**6xx") get the assistant with memory and all tools,
// optionally only after a PIN typed on the keypad (PHONE_PIN, ends with #). Anyone else is a
// guest: no memory, no tools but hanging up; The assistant takes a message and the user gets a report.
// The caller hears it ring until the session is up. A hang-up on either side ends both.
// The assistant also calls (phone_call): a person of the household as itself (the admin's number from
// PHONE_OWNER), or anyone else for the user (a reservation: the far end gets no memory and no
// tools, the one who asked a report afterwards); and he hangs up himself (end_call).
namespace phone {

bool configured();
bool begin();   // the RTP task; at boot
// Registers phone_call and end_call (voice) with the tool registry (assistant/tools).
void addTools();
void update();  // every loop pass: SIP messages, registration, the call
bool inCall();  // ringing or talking
void print();   // serial: registration and call

}  // namespace phone
