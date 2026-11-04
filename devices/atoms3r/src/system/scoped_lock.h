#pragma once

#include <Arduino.h>

// Holds a FreeRTOS mutex for a scope. Modules define their own short form:
//   struct Locked : ScopedLock { Locked() : ScopedLock(lock) {} };
class ScopedLock {
public:
    explicit ScopedLock(SemaphoreHandle_t mutex) : mutex(mutex) { xSemaphoreTake(mutex, portMAX_DELAY); }
    ~ScopedLock() { xSemaphoreGive(mutex); }
    ScopedLock(const ScopedLock&) = delete;
    ScopedLock& operator=(const ScopedLock&) = delete;

private:
    SemaphoreHandle_t mutex;
};
