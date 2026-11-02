"""Reference data for the Echo's Go wake word port (core/internal/wakeword, TestReference):
TFLite Micro's frontend (pymicro_features) and the full TFLite runtime (LiteRT) on a WAV file.

    python reference.py <model.tflite> <file.wav> <out_dir>
    WAKEWORD_REF=<out_dir> WAKEWORD_MODEL=<model.tflite> go test ./internal/wakeword -run Reference -v

Writes stream.raw (s16le), features.txt (the frontend's raw values, one frame per line) and
probs.txt (the model's uint8 output per inference). Used to check a newly trained model and the
operators it needs (SPLIT_V since microWakeWord 2025, #61).
"""
import os
import sys

import numpy as np
import pymicro_features
import soundfile as sf
from ai_edge_litert.interpreter import Interpreter


def main():
    model, wav, out = sys.argv[1], sys.argv[2], sys.argv[3]
    os.makedirs(out, exist_ok=True)
    audio, rate = sf.read(wav, dtype="int16")
    if audio.ndim > 1:
        audio = audio[:, 0]
    assert rate == 16000, "16 kHz only"
    audio.astype("<i2").tofile(os.path.join(out, "stream.raw"))

    frontend = pymicro_features.MicroFrontend()
    frames = []
    for i in range(0, len(audio) - 159, 160):
        r = frontend.process_samples(audio[i:i + 160].tobytes())
        if r.features:
            frames.append([int(round(f * 25.6)) for f in r.features])  # pymicro_features scales by 1/25.6
    with open(os.path.join(out, "features.txt"), "w") as f:
        f.write("\n".join(" ".join(map(str, fr)) for fr in frames) + "\n")

    it = Interpreter(model_path=model)
    it.allocate_tensors()
    inp, outp = it.get_input_details()[0], it.get_output_details()[0]
    stride = inp["shape"][1]
    probs = []
    for i in range(0, len(frames) - stride + 1, stride):
        x = np.array(frames[i:i + stride], dtype=np.int32)
        q = np.clip((x * 256 + 333) // 666 - 128, -128, 127).astype(np.int8)  # as TFLM's micro_speech
        it.set_tensor(inp["index"], q.reshape(inp["shape"]))
        it.invoke()
        probs.append(int(it.get_tensor(outp["index"]).reshape(-1)[0]))
    with open(os.path.join(out, "probs.txt"), "w") as f:
        f.write(" ".join(map(str, probs)) + "\n")
    print(f"{len(frames)} frames, {len(probs)} inferences (stride {stride}), max probability {max(probs)}")


if __name__ == "__main__":
    main()
