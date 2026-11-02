#pragma once

#include <stdint.h>

// G.711 A-law (RTP payload type 8, "PCMA", Europe) and µ-law (0, "PCMU"): 8 kHz, one byte per
// sample. The classic ITU-T reference algorithms.
namespace g711 {

inline uint8_t encodeAlaw(int16_t pcm)
{
    int sample = pcm >> 3;  // 13 bits
    uint8_t mask = 0xD5;
    if (sample < 0) {
        mask = 0x55;
        sample = -sample - 1;
    }
    static const int16_t segments[8] = {0x1F, 0x3F, 0x7F, 0xFF, 0x1FF, 0x3FF, 0x7FF, 0xFFF};
    int segment = 0;
    while (segment < 8 && sample > segments[segment]) ++segment;
    if (segment >= 8) return uint8_t(0x7F ^ mask);
    uint8_t value = uint8_t(segment << 4);
    value |= segment < 2 ? (sample >> 1) & 0x0F : (sample >> segment) & 0x0F;
    return uint8_t(value ^ mask);
}

inline int16_t decodeAlaw(uint8_t alaw)
{
    alaw ^= 0x55;
    int value = (alaw & 0x0F) << 4;
    const int segment = (alaw & 0x70) >> 4;
    if (segment == 0) value += 8;
    else if (segment == 1) value += 0x108;
    else value = (value + 0x108) << (segment - 1);
    return int16_t((alaw & 0x80) ? value : -value);
}

inline uint8_t encodeUlaw(int16_t pcm)
{
    constexpr int BIAS = 0x84, CLIP = 32635;
    int sample = pcm;
    const uint8_t sign = sample < 0 ? 0x80 : 0;
    if (sign) sample = -sample;
    if (sample > CLIP) sample = CLIP;
    sample += BIAS;
    int exponent = 7;
    for (int mask = 0x4000; exponent > 0 && !(sample & mask); mask >>= 1) --exponent;
    const int mantissa = (sample >> (exponent + 3)) & 0x0F;
    return uint8_t(~(sign | (exponent << 4) | mantissa));
}

inline int16_t decodeUlaw(uint8_t ulaw)
{
    ulaw = ~ulaw;
    const int exponent = (ulaw >> 4) & 0x07;
    const int value = (((ulaw & 0x0F) << 3) + 0x84) << exponent;
    return int16_t((ulaw & 0x80) ? 0x84 - value : value - 0x84);
}

}  // namespace g711
