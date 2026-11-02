"""Downloads what the wake word training needs into the work directory (#61).

    python fetch.py [work_dir]      (default E:/wakeword-work)

- Piper voices for synthetic positives: the German ones (mls has many speakers) and the English
  LibriTTS-R generator (904 speakers, mixable) of piper-sample-generator.
- Augmentation: MIT room impulse responses, a part of AudioSet, FMA music (16 kHz WAV).
- microWakeWord's precomputed negative features (speech, dinner party, no speech).

Already downloaded files are skipped. The datasets' licences are mixed: models trained with them are
for personal, non-commercial use.
"""
import os
import sys
import urllib.request
import zipfile

HF = "https://huggingface.co"
VOICES = [
    "de/de_DE/thorsten/medium/de_DE-thorsten-medium",
    "de/de_DE/thorsten/high/de_DE-thorsten-high",
    "de/de_DE/thorsten_emotional/medium/de_DE-thorsten_emotional-medium",
    "de/de_DE/mls/medium/de_DE-mls-medium",
    "de/de_DE/kerstin/low/de_DE-kerstin-low",
    "de/de_DE/ramona/low/de_DE-ramona-low",
    "de/de_DE/eva_k/x_low/de_DE-eva_k-x_low",
    "de/de_DE/karlsson/low/de_DE-karlsson-low",
    "de/de_DE/pavoque/low/de_DE-pavoque-low",
]
# Generators that mix speaker embeddings (many voices): English LibriTTS-R, German MLS
GENERATORS = ["en_US-libritts_r-medium", "de_DE-mls-medium"]
RELEASE = "https://github.com/rhasspy/piper-sample-generator/releases/download/v2.0.0"
NEGATIVES = ["dinner_party", "dinner_party_eval", "no_speech", "speech"]


def fetch(url, path):
    if os.path.exists(path) and os.path.getsize(path) > 0:
        return
    os.makedirs(os.path.dirname(path), exist_ok=True)
    print("downloading", url, flush=True)
    tmp = path + ".part"
    urllib.request.urlretrieve(url, tmp)
    os.replace(tmp, path)


def main():
    work = sys.argv[1] if len(sys.argv) > 1 else "E:/wakeword-work"
    for v in VOICES:
        name = v.rsplit("/", 1)[1]
        for ext in (".onnx", ".onnx.json"):
            fetch(f"{HF}/rhasspy/piper-voices/resolve/main/{v}{ext}", os.path.join(work, "voices", name + ext))
    models = os.path.join(work, "piper-sample-generator", "models")
    for g in GENERATORS:
        fetch(f"{RELEASE}/{g}.pt", os.path.join(work, "voices", g + ".pt"))
        # the generator's config ships in the repository's models/
        cfg = os.path.join(work, "voices", g + ".pt.json")
        if not os.path.exists(cfg):
            with open(os.path.join(models, g + ".pt.json"), "rb") as src, open(cfg, "wb") as dst:
                dst.write(src.read())
    for n in NEGATIVES:
        z = os.path.join(work, "downloads", n + ".zip")
        fetch(f"{HF}/datasets/kahrendt/microwakeword/resolve/main/{n}.zip", z)
        target = os.path.join(work, "negative_datasets")
        if not os.path.isdir(os.path.join(target, n)):
            print("unpacking", n, flush=True)
            with zipfile.ZipFile(z) as f:
                f.extractall(target)
    # AudioSet (the notebook's bal_train09.tar) moved to parquet files of ~690 MB: left out for now;
    # the background noise is FMA music and the household's own TV and music recordings.
    fma = os.path.join(work, "downloads", "fma_xs.zip")
    fetch(f"{HF}/datasets/mchl914/fma_xsmall/resolve/main/fma_xs.zip", fma)
    to_16k_dir(fma, os.path.join(work, "background", "fma"))
    rirs(os.path.join(work, "rirs"))
    print("done")


def write16k(path, audio, rate):
    import numpy as np
    import soundfile as sf
    from scipy.signal import resample_poly

    if audio.ndim > 1:
        audio = audio.mean(axis=1)
    if rate != 16000:
        g = np.gcd(int(rate), 16000)
        audio = resample_poly(audio, 16000 // g, int(rate) // g)
    sf.write(path, audio, 16000, subtype="PCM_16")


def to_16k_dir(zip_path, out):
    """The archive's audio files as 16 kHz mono WAV."""
    import io

    import soundfile as sf

    if os.path.isdir(out) and os.listdir(out):
        return
    os.makedirs(out, exist_ok=True)
    with zipfile.ZipFile(zip_path) as z:
        names = [n for n in z.namelist() if n.lower().endswith((".mp3", ".wav", ".flac", ".ogg"))]
        print(f"converting {len(names)} files of {os.path.basename(zip_path)}", flush=True)
        for n in names:
            try:
                audio, rate = sf.read(io.BytesIO(z.read(n)), dtype="float32")
            except Exception as e:  # a damaged file is left out
                print("skipped", n, e)
                continue
            write16k(os.path.join(out, os.path.splitext(os.path.basename(n))[0] + ".wav"), audio, rate)


def rirs(out):
    """MIT environmental impulse responses (as the microWakeWord notebook), the repository's 16 kHz
    WAV files directly (the datasets library wants torchcodec for them on Windows)."""
    import json

    if os.path.isdir(out) and len(os.listdir(out)) > 200:
        return
    repo = "datasets/davidscripka/MIT_environmental_impulse_responses"
    with urllib.request.urlopen(f"{HF}/api/{repo}/tree/main/16khz") as r:
        files = [x["path"] for x in json.load(r)]
    print(f"downloading {len(files)} impulse responses", flush=True)
    for p in files:
        fetch(f"{HF}/{repo}/resolve/main/{p}", os.path.join(out, os.path.basename(p)))


if __name__ == "__main__":
    main()
