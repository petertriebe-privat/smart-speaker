#pragma once

#include <stdint.h>

// Time-domain NLMS echo canceller. The reference is the speaker loopback (MIC3),
// sampled by the same ADC as the microphone, so no delay alignment is needed.
class EchoCanceller {
public:
    static constexpr int TAPS = 256;  // 10.7 ms at 24 kHz: direct path plus early reflections

    // Block energies (mean square, full scale = 1) of the last process() call.
    struct Stats {
        float mic = 0, ref = 0, echo = 0, residual = 0;
    };

    void process(const int16_t* mic, const int16_t* ref, int16_t* out, int n, bool adapt);
    void reset();
    const Stats& stats() const { return stats_; }
    int peakTap() const;  // echo delay estimate in samples

private:
    alignas(16) float w_[TAPS] = {};
    alignas(16) float x_[2 * TAPS] = {};  // mirrored ring: x_ + pos_ is a contiguous window, newest first
    int pos_ = 0;
    Stats stats_;
};
