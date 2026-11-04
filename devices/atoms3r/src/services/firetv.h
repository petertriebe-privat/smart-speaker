#pragma once

#include <Arduino.h>

// The Fire TV sticks through Home Assistant, from two integrations that complement each other:
// - Android Debug Bridge (androidtv, local, needs ADB debugging on the stick): state and running
//   app, on/off, starting an app (media_player.select_source) and remote keys (remote.send_command).
// - Alexa Devices (cloud): a text command to the stick's own Alexa, the only way to play a film or
//   series by its title (Alexa resolves it through Amazon's catalog of the apps).
// The sticks come from HA at run time: Alexa's devices whose model names a Fire TV, ADB's media
// players; one of each with the same room (or the same name) is the same stick. Without ADB only
// the Alexa part works. Tool: fire_tv (slow: Alexa goes through Amazon's cloud).
namespace firetv {

void addTools();

}  // namespace firetv
