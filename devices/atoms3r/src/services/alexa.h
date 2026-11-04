#pragma once

#include <Arduino.h>

// The Amazon Echo speakers through Home Assistant's official Alexa Devices integration
// (alexa_devices). Volume and mute come from HA's MCP tools (HassSetVolume, ...); this adds what
// MCP does not offer: a text command to Alexa (music, pause, next track, timers on the Echo)
// and making an Echo speak or announce. Tool: alexa (slow: HA passes it on to Amazon's cloud).
namespace alexa {

void addTools();

}  // namespace alexa
