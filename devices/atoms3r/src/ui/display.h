#pragma once

// The AtomS3R display (128 x 128): state, WiFi, volume and cost, the end of the conversation.
namespace display {

void begin();
void update();  // redraws every 200 ms

}  // namespace display
