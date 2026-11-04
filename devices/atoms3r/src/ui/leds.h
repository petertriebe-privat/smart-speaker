#pragma once

// The two LED strips of the Pyramid (14 LEDs each, both show the same):
//   dark blue = ready, yellow = connecting, cyan = listening (level), green = speaking (level),
//   violet = thinking, white bar = volume, red = error.
namespace leds {

void update();

}  // namespace leds
