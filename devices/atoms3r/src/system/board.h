#pragma once

#include <Arduino.h>
#include <M5EchoPyramid.h>

// The hardware: AtomS3R (display, screen button) with the Echo Pyramid on Wire1 (codec,
// amplifier, LEDs, touch). The Pyramid needs its own USB-C power, otherwise the microphone
// delivers zeros and the amplifier is missing.
namespace board {

String begin();  // M5Unified, serial port and Pyramid; returns an error text, "" if fine
M5EchoPyramid& pyramid();

// Warns on the display while the microphone delivers only zeros (Pyramid without power).
void checkMicrophone();
// Every AMPLIFIER_CHECK_MS: warns while the amplifier does not answer, sets it up again when it
// lost its setup (main loop only: Wire1).
void checkAmplifier();

}  // namespace board
