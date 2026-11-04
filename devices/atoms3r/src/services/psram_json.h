#pragma once

#include <ArduinoJson.h>
#include <esp_heap_caps.h>

// ArduinoJson allocator in PSRAM. A document with all tool schemas and the memory notes is
// tens of KB in many small blocks; from the default heap they took the internal RAM that TLS
// needs ("SSL - Memory allocation failed" mailing a scheduled task's result).
//   JsonDocument doc(PsramJson::instance());
class PsramJson : public ArduinoJson::Allocator {
public:
    void* allocate(size_t size) override { return heap_caps_malloc(size, MALLOC_CAP_SPIRAM | MALLOC_CAP_8BIT); }
    void deallocate(void* pointer) override { heap_caps_free(pointer); }
    void* reallocate(void* pointer, size_t size) override
    {
        return heap_caps_realloc(pointer, size, MALLOC_CAP_SPIRAM | MALLOC_CAP_8BIT);
    }
    static Allocator* instance()
    {
        static PsramJson allocator;
        return &allocator;
    }
};

// A document serialized into PSRAM, for requests and events of any size: Arduino's String
// corrupts content beyond 64 KB (measured 28.09.: the length was right, but the bytes past
// 65536 overwrote the start; GPT Live rejected session.start with "invalid character ','
// after object key"). Empty (data nullptr) if out of memory.
class PsramText {
public:
    explicit PsramText(const JsonDocument& doc) : size(measureJson(doc))
    {
        data = static_cast<char*>(heap_caps_malloc(size + 1, MALLOC_CAP_SPIRAM | MALLOC_CAP_8BIT));
        if (!data) size = 0;
        else serializeJson(doc, data, size + 1);
    }
    ~PsramText() { heap_caps_free(data); }
    PsramText(const PsramText&) = delete;
    PsramText& operator=(const PsramText&) = delete;
    char* data = nullptr;
    size_t size;
};
