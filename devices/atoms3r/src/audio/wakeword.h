#pragma once

#include <Arduino.h>

// Wake word detection with a microWakeWord streaming model ("Hey Jarvis") on
// TensorFlow Lite Micro. Audio comes from audio::readWake() at 16 kHz; inference
// runs in its own task on core 0 while listening is enabled.
namespace wakeword {

bool begin();                // load the model and start the task
bool ready();                // begin() succeeded
void setListening(bool on);  // only listen while no session is open
bool takeDetection();        // true once per detection
int lastProbability();       // sliding-window average of the last inference, 0..255
int takePeakProbability();   // highest average since the previous call, 0..255

}  // namespace wakeword
