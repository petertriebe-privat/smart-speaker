#pragma once

#include <Arduino.h>
#include <esp_heap_caps.h>

// Collects an HTTP body of unknown size (chunked) in PSRAM, for HTTPClient::writeToStream.
// The data stays NUL-terminated so JSON bodies can be searched as C strings.
class PsramSink : public Stream {
public:
    explicit PsramSink(size_t limit = 8 * 1024 * 1024) : limit(limit) {}
    ~PsramSink() { free(data); }
    uint8_t* data = nullptr;
    size_t size = 0;

    // Hands the buffer to the caller (who frees it).
    uint8_t* release()
    {
        uint8_t* out = data;
        data = nullptr;
        size = capacity = 0;
        return out;
    }

    size_t write(uint8_t c) override { return write(&c, 1); }
    size_t write(const uint8_t* buffer, size_t length) override
    {
        if (size + length + 1 > limit) return 0;
        if (size + length + 1 > capacity) {
            size_t next = max(capacity * 2, size_t(64 * 1024));
            while (next < size + length + 1) next *= 2;
            next = min(next, limit);
            auto* grown = static_cast<uint8_t*>(
                heap_caps_realloc(data, next, MALLOC_CAP_SPIRAM | MALLOC_CAP_8BIT));
            if (!grown) return 0;
            data = grown;
            capacity = next;
        }
        memcpy(data + size, buffer, length);
        size += length;
        data[size] = 0;
        return length;
    }
    int available() override { return 0; }
    int read() override { return -1; }
    int peek() override { return -1; }
    void flush() override {}

private:
    size_t capacity = 0, limit;
};
