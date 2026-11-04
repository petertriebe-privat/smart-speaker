#pragma once

#include <Arduino.h>
#include <atomic>

// One background TLS job at a time. Each connection needs ~40 KB of internal RAM for its
// handshake; after a conversation the memory's consolidation, the mail check and the task
// scheduler's backup connected at once and got "SSL - Memory allocation failed". The background
// tasks (memory, mail, tasks, Telegram) hold the gate for a whole job. Only at that top level: tools inside
// an agent run never take it (the run already holds it), so it cannot deadlock. Conversations
// and their tools do not use it.
namespace tls_gate {

inline SemaphoreHandle_t mutex()
{
    static SemaphoreHandle_t gate = xSemaphoreCreateMutex();
    return gate;
}

// Set when someone found the gate taken. Telegram keeps its polling connection (and the gate)
// between polls and hands both over when this is set.
inline std::atomic<bool>& wanted()
{
    static std::atomic<bool> flag{false};
    return flag;
}

// Waits for the gate.
class Hold {
public:
    Hold()
    {
        if (xSemaphoreTake(mutex(), 0) == pdTRUE) return;
        wanted() = true;
        xSemaphoreTake(mutex(), portMAX_DELAY);
    }
    ~Hold() { xSemaphoreGive(mutex()); }
};

// Takes the gate only if it is free: for work that can simply come again later (the mail
// check), so the task stays free for more urgent calls meanwhile.
class TryHold {
public:
    TryHold() : held(xSemaphoreTake(mutex(), 0) == pdTRUE)
    {
        if (!held) wanted() = true;
    }
    ~TryHold()
    {
        if (held) xSemaphoreGive(mutex());
    }
    explicit operator bool() const { return held; }

private:
    bool held;
};

}  // namespace tls_gate
