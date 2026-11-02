"""Which spelling makes a voice say "Jarvis" (#61)? Generates "Hey <spelling>" per voice and lets the
stock English "Hey Jarvis" model score it (cmd/wakeeval): the higher, the nearer to "Jarvis".

    python spellings.py <work_dir> <wakeeval_dir>

<wakeeval_dir>: core (go run ./cmd/wakeeval there).
"""
import os
import subprocess
import sys
import wave

import numpy as np
from scipy.signal import resample_poly

SPELLINGS = ["Jarvis", "Dschahrwis", "Dscharwis", "Dschahwis", "Tschahrwis", "Dschaarvis"]
VOICES = ["de_DE-thorsten-medium.onnx", "de_DE-mls-medium.onnx", "de_DE-kerstin-low.onnx", "de_DE-ramona-low.onnx",
          "de_DE-karlsson-low.onnx", "de_DE-pavoque-low.onnx", "de_DE-eva_k-x_low.onnx",
          "de_DE-thorsten_emotional-medium.onnx",
          "en_US-libritts_r-medium.pt", "de_DE-mls-medium.pt"]  # .pt: generators mixing speakers
SAMPLES = 6


def to16k(path):
    with wave.open(path) as w:
        rate, x = w.getframerate(), np.frombuffer(w.readframes(w.getnframes()), "<i2").astype(np.float32)
    if rate != 16000:
        g = np.gcd(rate, 16000)
        x = resample_poly(x, 16000 // g, rate // g)
    # 1 s of quiet before and after: the detector warms up and the word ends inside the file
    x = np.concatenate([np.zeros(16000), x, np.zeros(8000)])
    with wave.open(path, "wb") as w:
        w.setnchannels(1)
        w.setsampwidth(2)
        w.setframerate(16000)
        w.writeframes(np.clip(x, -32768, 32767).astype("<i2").tobytes())


def main():
    work, evaldir = sys.argv[1], sys.argv[2]
    out = os.path.join(work, "spellings")
    for voice in VOICES:
        for sp in SPELLINGS:
            d = os.path.join(out, voice, sp)
            if not os.path.isdir(d):
                # piper_train (the generator's model code) lies next to the package in its repository
                env = dict(os.environ, PYTHONPATH=os.path.join(work, "piper-sample-generator"))
                r = subprocess.run([sys.executable, "-m", "piper_sample_generator", f"Hey {sp}!",
                                    "--model", os.path.join(work, "voices", voice), "--max-samples", str(SAMPLES),
                                    "--output-dir", d], env=env, capture_output=True, text=True)
                if r.returncode != 0:
                    sys.exit(r.stderr[-2000:])
                for f in os.listdir(d):
                    to16k(os.path.join(d, f))
            files = sorted(os.path.join(d, f) for f in os.listdir(d))
            r = subprocess.run(["go", "run", "./cmd/wakeeval", "-cutoff", "1"] + files, cwd=evaldir,
                               capture_output=True, text=True)
            peaks = [float(l.split("peak ")[1].split()[0]) for l in r.stdout.splitlines() if "mono" in l]
            print(f"{voice:34} {sp:12} median {np.median(peaks):.2f}  {' '.join(f'{p:.2f}' for p in peaks)}", flush=True)


if __name__ == "__main__":
    main()
