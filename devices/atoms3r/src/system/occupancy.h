#pragma once

#include <Arduino.h>

// The presence sensor on the pyramid's Grove port: an STHS34PF80 (M5 Unit TMOS, I2C 0x5A on
// the pyramid's bus). Infrared, so it also notices someone sitting still, not only movement.
// The room counts as occupied from the first motion or (trusted) presence flag; empty after
// ROOM_EMPTY_AFTER_MS without one once someone walked out, else ROOM_STILL_EMPTY_MS (config.h).
// Home Assistant gets it as binary_sensor.jarvis_presence (state set over REST),
// so automations there can react (light when someone comes in, a greeting via jarvis_announce).
namespace occupancy {

// The presence sensor in Home Assistant, after the assistant's name (binary_sensor.jarvis_presence by default).
String entity();

void begin();               // after board::begin() (Wire1)
void update(bool reportAllowed);  // every loop; reports to HA only when allowed (REST blocks)
bool available();
bool someoneThere();        // the room counts as occupied (false without the sensor); any task
void toggleDebug();         // serial R: the raw presence and motion values every second
void addTools();            // room_sensor

}  // namespace occupancy
