#pragma once

#include <Arduino.h>
#include <IPAddress.h>

// The audio of a phone call (RTP over UDP, G.711 at 8 kHz), converted to and from the 24 kHz
// PCM of GPT Live. A task of its own sends a 20 ms packet every 20 ms (silence when nothing is
// queued) and takes in the caller's packets and key presses (RFC 4733 telephone-event).
// The conversation side mirrors audio/audio.h: readMic / queueSpeaker / speaking.
namespace rtp {

bool begin();  // buffers (PSRAM); the task starts with a call

// Starts the stream of a call to the address and payload types negotiated in SIP (payload 8 =
// A-law, 0 = µ-law; dtmfType -1 = none). stop() ends it. The media task runs only during a
// call; false if it could not start (internal RAM).
bool start(IPAddress remote, uint16_t remotePort, uint8_t payloadType, int dtmfType);
void stop();
bool active();

void setStreaming(bool on);  // the caller's audio into the microphone queue (off: dropped)
size_t micAvailable();       // samples at 24 kHz
size_t readMic(int16_t* dst, size_t samples);
size_t queueSpeaker(const int16_t* src, size_t samples);  // 24 kHz; returns samples accepted
size_t speakerQueued();                                    // samples at 24 kHz waiting to be sent
void flushSpeaker();
bool speaking();  // audible output within the last ECHO_TAIL_MS (by level: GPT Live sends silence too)

bool takeDigit(char& digit);  // a key the caller pressed: '0'..'9', '*', '#'
// Presses keys ('0'..'9', '*', '#') on our side, one after another (~0.26 s each): RFC 4733
// events if the call negotiated telephone-event, else tones in the audio. False if no call.
bool pressKeys(const String& keys);

}  // namespace rtp
