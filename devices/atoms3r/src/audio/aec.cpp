// Arduino builds with -Os; the per-sample loops here are the hot path of the audio task.
#pragma GCC optimize("O3")

#include "audio/aec.h"

#include <dsps_dotprod.h>
#include <math.h>
#include <string.h>

namespace {
constexpr float MU = 0.3f;
constexpr float REGULARIZATION = EchoCanceller::TAPS * 1e-6f;  // ~ -60 dBFS reference floor

// w += g * x. Unrolled with independent accumulators so loads and multiply-adds overlap.
void adaptWeights(float* __restrict w, const float* __restrict x, float g)
{
    for (int k = 0; k < EchoCanceller::TAPS; k += 4) {
        const float a = w[k] + g * x[k], b = w[k + 1] + g * x[k + 1];
        const float c = w[k + 2] + g * x[k + 2], d = w[k + 3] + g * x[k + 3];
        w[k] = a;
        w[k + 1] = b;
        w[k + 2] = c;
        w[k + 3] = d;
    }
}
}

void EchoCanceller::process(const int16_t* mic, const int16_t* ref, int16_t* out, int n,
                            bool adapt)
{
    float power = 0;
    for (int k = 0; k < TAPS; ++k) power += x_[pos_ + k] * x_[pos_ + k];
    Stats s;
    for (int i = 0; i < n; ++i) {
        pos_ = pos_ ? pos_ - 1 : TAPS - 1;
        const float old = x_[pos_];
        const float xi = ref[i] / 32768.0f;
        x_[pos_] = x_[pos_ + TAPS] = xi;
        power = fmaxf(0.0f, power + xi * xi - old * old);
        const float* window = x_ + pos_;
        float y = 0;
        dsps_dotprod_f32(w_, window, &y, TAPS);
        const float d = mic[i] / 32768.0f;
        const float e = d - y;
        if (adapt) {
            const float g = MU * e / (power + REGULARIZATION);
            adaptWeights(w_, window, g);
        }
        const float scaled = e * 32768.0f;
        out[i] = scaled > 32767.0f ? 32767 : scaled < -32768.0f ? -32768 : int16_t(scaled);
        s.mic += d * d;
        s.ref += xi * xi;
        s.echo += y * y;
        s.residual += e * e;
    }
    s.mic /= n;
    s.ref /= n;
    s.echo /= n;
    s.residual /= n;
    stats_ = s;
    // A diverged filter would only add noise; start over.
    if (!isfinite(s.residual)) reset();
}

void EchoCanceller::reset()
{
    memset(w_, 0, sizeof(w_));
    memset(x_, 0, sizeof(x_));
    pos_ = 0;
}

int EchoCanceller::peakTap() const
{
    int best = 0;
    for (int k = 1; k < TAPS; ++k) if (fabsf(w_[k]) > fabsf(w_[best])) best = k;
    return best;
}
