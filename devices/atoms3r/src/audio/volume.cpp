#include "audio/volume.h"

#include <Arduino.h>
#include <atomic>

#include "config.h"
#include "system/board.h"
#include "ui/status.h"

namespace volume {
namespace {

std::atomic<int> current{DEFAULT_VOLUME};  // board::begin() sets the codec to it
std::atomic<int> pending{-1};              // requested by another task, -1 = none

}  // namespace

int level() { return current; }

void set(int percent)
{
    percent = constrain(percent, 0, 100);
    if (board::pyramid().codec().setVolume(percent)) current = percent;
    status::showVolume();
}

void change(int step) { set(current + step); }

int request(int level, int step)
{
    const int base = pending >= 0 ? pending.load() : current.load();
    if (!step && level < 0) return base;
    const int target = constrain(step ? base + step : level, 0, 100);
    pending = target;
    return target;
}

void update()
{
    const int target = pending.exchange(-1);
    if (target >= 0) set(target);
}

}  // namespace volume
