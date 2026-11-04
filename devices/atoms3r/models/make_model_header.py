"""Converts a microWakeWord .tflite model into a C header (16-byte aligned array).

    python models/make_model_header.py models/hey_jarvis.tflite include/wake_model.h
    python models/make_model_header.py jarvis.tflite include/wake_model_own.h 0.8 30000

The second form is the household's own model (#61, trained on their voices: never in the
repository, include/wake_model_own.h is git-ignored); the firmware prefers it, with its cutoff and
tensor arena (the manifest's tensor_arena_size).
"""
import sys

src, dst = sys.argv[1], sys.argv[2]
own = len(sys.argv) > 3
data = open(src, "rb").read()
lines = [", ".join(f"0x{b:02x}" for b in data[i:i + 16]) for i in range(0, len(data), 16)]
with open(dst, "w", newline="\n") as out:
    out.write(f"#pragma once\n\n// Generated from {src} by models/make_model_header.py - do not edit.\n")
    out.write("#include <stddef.h>\n#include <stdint.h>\n\n")
    out.write("alignas(16) static const uint8_t WAKE_MODEL[] = {\n    ")
    out.write(",\n    ".join(lines))
    out.write("\n};\n")
    out.write(f"static const size_t WAKE_MODEL_SIZE = {len(data)};\n")
    if own:
        out.write("#define WAKE_MODEL_OWN 1\n")
        out.write(f"constexpr float WAKE_MODEL_CUTOFF = {float(sys.argv[3])}f;\n")
        out.write(f"constexpr size_t WAKE_MODEL_ARENA = {int(sys.argv[4])};\n")
