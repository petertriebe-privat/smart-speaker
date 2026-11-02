#include "audio/wakeword.h"

#include <esp_heap_caps.h>
#include <frontend.h>
#include <frontend_util.h>

#include "audio/audio.h"
#include "config.h"
#include "tensorflow/lite/micro/micro_interpreter.h"
#include "tensorflow/lite/micro/micro_mutable_op_resolver.h"
#include "tensorflow/lite/micro/micro_resource_variable.h"
#include "tensorflow/lite/schema/schema_generated.h"
// The household's own model when it is there (#61, git-ignored: models/make_model_header.py),
// else the stock "Hey Jarvis".
#if __has_include("wake_model_own.h")
#include "wake_model_own.h"
#else
#include "wake_model.h"
#endif

namespace wakeword {
namespace {

// Feature settings every microWakeWord model is trained with.
constexpr int FEATURES = 40;
constexpr int WINDOW_MS = 30;
constexpr int STEP_MS = 10;
constexpr int SAMPLE_RATE = 16000;
#ifdef WAKE_MODEL_OWN
constexpr size_t TENSOR_ARENA = WAKE_MODEL_ARENA;  // the own model's manifest
constexpr float CUTOFF = WAKE_MODEL_CUTOFF;
constexpr const char* MODEL_NAME = "own";
#else
constexpr const char* MODEL_NAME = "stock";
constexpr size_t TENSOR_ARENA = 20 * 1024;   // measured 29.09.: 15516 bytes used (the manifest says 22860)
constexpr float CUTOFF = WAKE_PROBABILITY_CUTOFF;
#endif
constexpr size_t VARIABLE_ARENA = 1024;      // streaming state of the model
constexpr int WARMUP_INFERENCES = 34;        // ~1 s: the streaming state needs history

FrontendState frontend;
tflite::MicroMutableOpResolver<20> resolver;
tflite::MicroInterpreter* interpreter = nullptr;
volatile bool listening = false, detection = false;
bool started = false;
volatile int averageProbability = 0, peakProbability = 0;
uint8_t history[WAKE_SLIDING_WINDOW] = {};
int historyPos = 0, inferences = 0, stridePos = 0;

bool fail(const char* step)
{
    Serial.printf("Wake word: error at %s", step);
    Serial.println();
    return false;
}

bool setupFrontend()
{
    FrontendConfig config;
    config.window.size_ms = WINDOW_MS;
    config.window.step_size_ms = STEP_MS;
    config.filterbank.num_channels = FEATURES;
    config.filterbank.lower_band_limit = 125.0f;
    config.filterbank.upper_band_limit = 7500.0f;
    config.filterbank.output_scale_shift = 0;
    config.noise_reduction.smoothing_bits = 10;
    config.noise_reduction.even_smoothing = 0.025f;
    config.noise_reduction.odd_smoothing = 0.06f;
    config.noise_reduction.min_signal_remaining = 0.05f;
    config.pcan_gain_control.enable_pcan = 1;
    config.pcan_gain_control.strength = 0.95f;
    config.pcan_gain_control.offset = 80.0f;
    config.pcan_gain_control.gain_bits = 21;
    config.log_scale.enable_log = 1;
    config.log_scale.scale_shift = 6;
    return FrontendPopulateState(&config, &frontend, SAMPLE_RATE) || fail("frontend");
}

bool setupModel()
{
    const bool ok = resolver.AddCallOnce() == kTfLiteOk && resolver.AddVarHandle() == kTfLiteOk &&
        resolver.AddReshape() == kTfLiteOk && resolver.AddReadVariable() == kTfLiteOk &&
        resolver.AddStridedSlice() == kTfLiteOk && resolver.AddConcatenation() == kTfLiteOk &&
        resolver.AddAssignVariable() == kTfLiteOk && resolver.AddConv2D() == kTfLiteOk &&
        resolver.AddMul() == kTfLiteOk && resolver.AddAdd() == kTfLiteOk &&
        resolver.AddMean() == kTfLiteOk && resolver.AddFullyConnected() == kTfLiteOk &&
        resolver.AddLogistic() == kTfLiteOk && resolver.AddQuantize() == kTfLiteOk &&
        resolver.AddDepthwiseConv2D() == kTfLiteOk && resolver.AddAveragePool2D() == kTfLiteOk &&
        resolver.AddMaxPool2D() == kTfLiteOk && resolver.AddPad() == kTfLiteOk &&
        resolver.AddPack() == kTfLiteOk && resolver.AddSplitV() == kTfLiteOk;
    if (!ok) return fail("op resolver");
    const tflite::Model* model = tflite::GetModel(WAKE_MODEL);
    if (model->version() != TFLITE_SCHEMA_VERSION) return fail("model schema");
    auto* tensorArena = static_cast<uint8_t*>(heap_caps_aligned_alloc(16, TENSOR_ARENA, MALLOC_CAP_INTERNAL));
    auto* variableArena = static_cast<uint8_t*>(heap_caps_aligned_alloc(16, VARIABLE_ARENA, MALLOC_CAP_INTERNAL));
    if (!tensorArena || !variableArena) return fail("arena memory");
    auto* variables = tflite::MicroResourceVariables::Create(
        tflite::MicroAllocator::Create(variableArena, VARIABLE_ARENA), 20);
    interpreter = new tflite::MicroInterpreter(model, resolver, tensorArena, TENSOR_ARENA, variables);
    if (interpreter->AllocateTensors() != kTfLiteOk) return fail("AllocateTensors");
    const TfLiteTensor* input = interpreter->input(0);
    const TfLiteTensor* output = interpreter->output(0);
    if (input->type != kTfLiteInt8 || input->dims->size != 3 || input->dims->data[2] != FEATURES ||
        output->type != kTfLiteUInt8)
        return fail("model input/output format");
    Serial.printf("Wake word: %s model loaded, arena %u of %u bytes, stride %d, cutoff %.2f",
                  MODEL_NAME,
                  unsigned(interpreter->arena_used_bytes()), unsigned(TENSOR_ARENA), input->dims->data[1], CUTOFF);
    Serial.println();
    return true;
}

// Scale the frontend's 0..~670 values to the model's int8 input (as in TFLM micro_speech):
// input = feature * 256 / (25.6 * 26.0) - 128.
void toInput(const FrontendOutput& features, int8_t* input)
{
    for (size_t i = 0; i < features.size; ++i) {
        const int32_t value = (int32_t(features.values[i]) * 256 + 333) / 666 - 128;
        input[i] = int8_t(value < -128 ? -128 : value > 127 ? 127 : value);
    }
}

void infer(const FrontendOutput& features)
{
    TfLiteTensor* input = interpreter->input(0);
    const int stride = input->dims->data[1];
    toInput(features, input->data.int8 + stridePos * FEATURES);
    if (++stridePos < stride) return;
    stridePos = 0;
    if (interpreter->Invoke() != kTfLiteOk) return;
    history[historyPos] = interpreter->output(0)->data.uint8[0];
    historyPos = (historyPos + 1) % WAKE_SLIDING_WINDOW;
    int sum = 0;
    for (uint8_t p : history) sum += p;
    averageProbability = sum / WAKE_SLIDING_WINDOW;
    if (averageProbability > peakProbability) peakProbability = averageProbability;
    if (++inferences < WARMUP_INFERENCES) return;
    if (averageProbability > int(CUTOFF * 255)) {
        detection = true;
        // Start over: prevents one utterance from triggering twice.
        memset(history, 0, sizeof(history));
        inferences = 0;
    }
}

void wakeTask(void*)
{
    int16_t samples[SAMPLE_RATE * STEP_MS / 1000];
    for (;;) {
        const size_t count = audio::readWake(samples, sizeof(samples) / 2, pdMS_TO_TICKS(50));
        if (!listening) {
            inferences = 0;  // re-warm after each session
            continue;
        }
        const int16_t* p = samples;
        size_t left = count;
        while (left) {
            size_t used = 0;
            const FrontendOutput features = FrontendProcessSamples(&frontend, p, left, &used);
            p += used;
            left -= used;
            if (features.size == FEATURES) infer(features);
            if (!used) break;
        }
    }
}

}  // namespace

bool begin()
{
    if (!setupFrontend() || !setupModel()) return false;
    // Core 0 next to WiFi: the audio task owns core 1. ~1-2 ms per 30 ms inference.
    started = xTaskCreatePinnedToCore(wakeTask, "wakeword", 5120, nullptr, 3, nullptr, 0) == pdPASS;
    return started;
}

bool ready() { return started; }

void setListening(bool on)
{
    if (on && !listening) {
        detection = false;
        memset(history, 0, sizeof(history));
    }
    listening = on;
    audio::setWakeFeed(on);
}

bool takeDetection()
{
    if (!detection) return false;
    detection = false;
    return true;
}

int lastProbability() { return averageProbability; }

int takePeakProbability()
{
    const int peak = peakProbability;
    peakProbability = 0;
    return peak;
}

}  // namespace wakeword
