"""Features for training the wake word "Jarvis" with microWakeWord (#61).

    python prepare.py <work_dir> [set ...]     (default all: real_positive real_negative
                                                synthetic_negative synthetic_positive)

Reads (all under <work_dir>, made by fetch.py, cut_takes.py and generate.py):
    synthetic/positive, synthetic/negative   TTS "Jarvis" and sound-alikes
    real/positive, real/negative             the household's takes and TV/music/talk from the Echo
    hard/negative, hard/positive             the devices' false and real detections (harvest.py)
    hard/tv                                  long TV recordings in the room, cut into 3 s pieces
    background/fma, rirs                     music and room impulse responses
Writes features/<set>/{training,validation,testing}/<set>_mmap (microWakeWord's ragged mmaps) and
background/household (the real negatives cut into 3 s pieces: background noise for augmentation
and negatives themselves). A set already written is skipped; delete its folder to make it again.

The real takes are recorded through the Echo's DSP (BMicsOut), the signal the model will hear:
they get light augmentation only (no extra room, little noise) and are repeated in training. Their
validation and test splits are never trained on.
"""
import glob
import os
import sys

import numpy as np
import soundfile as sf
from mmap_ninja.ragged import RaggedMmap

from microwakeword.audio.augmentation import Augmentation
from microwakeword.audio.clips import Clips
from microwakeword.audio.spectrograms import SpectrogramGeneration

PIECE_S = 3.0


def cut_pieces(src_dir, out_dir):
    """Long negatives cut into 3 s pieces (Augmentation crops every clip to 3.2 s)."""
    if os.path.isdir(out_dir) and os.listdir(out_dir):
        return
    os.makedirs(out_dir, exist_ok=True)
    n = 0
    for path in sorted(glob.glob(os.path.join(src_dir, "**", "*.wav"), recursive=True)):
        audio, rate = sf.read(path, dtype="int16")
        step = int(PIECE_S * rate)
        for i in range(0, len(audio) - step // 2, step):
            sf.write(os.path.join(out_dir, f"{n:05d}.wav"), audio[i:i + step], rate, subtype="PCM_16")
            n += 1
    print(f"{n} pieces of {PIECE_S} s in {out_dir}", flush=True)


def write_set(work, name, clips, augmenter, repeat_train):
    base = os.path.join(work, "features", name)
    if os.path.isdir(base):
        print(f"{name}: exists, skipped", flush=True)
        return
    for split, split_name, repeat, slide in [("training", "train", repeat_train, 10),
                                             ("validation", "validation", 1, 10),
                                             ("testing", "test", 1, 1)]:
        out = os.path.join(base, split)
        os.makedirs(out, exist_ok=True)
        spectrograms = SpectrogramGeneration(clips=clips, augmenter=augmenter, slide_frames=slide, step_ms=10)
        RaggedMmap.from_generator(out_dir=os.path.join(out, f"{name}_mmap"),
                                  sample_generator=spectrograms.spectrogram_generator(split=split_name, repeat=repeat),
                                  batch_size=100, verbose=True)
        print(f"{name}/{split} done", flush=True)


def main():
    work = sys.argv[1]
    household = os.path.join(work, "background", "household")
    cut_pieces(os.path.join(work, "real", "negative"), household)
    backgrounds = [os.path.join(work, "background", "fma"), household]
    rirs = [os.path.join(work, "rirs")]

    full = Augmentation(augmentation_duration_s=3.2,
                        augmentation_probabilities={"SevenBandParametricEQ": 0.1, "TanhDistortion": 0.1,
                                                    "PitchShift": 0.1, "BandStopFilter": 0.1, "AddColorNoise": 0.1,
                                                    "AddBackgroundNoise": 0.75, "Gain": 1.0, "RIR": 0.5},
                        impulse_paths=rirs, background_paths=backgrounds,
                        background_min_snr_db=-5, background_max_snr_db=10,
                        min_jitter_s=0.195, max_jitter_s=0.205)
    # Real takes: the room and the DSP are in them already
    light = Augmentation(augmentation_duration_s=3.2,
                         augmentation_probabilities={"SevenBandParametricEQ": 0.05, "TanhDistortion": 0.0,
                                                     "PitchShift": 0.05, "BandStopFilter": 0.0, "AddColorNoise": 0.05,
                                                     "AddBackgroundNoise": 0.3, "Gain": 1.0, "RIR": 0.0},
                         impulse_paths=rirs, background_paths=backgrounds,
                         background_min_snr_db=5, background_max_snr_db=20,
                         min_jitter_s=0.195, max_jitter_s=0.205)
    plain = Augmentation(augmentation_duration_s=3.2,
                         augmentation_probabilities={"SevenBandParametricEQ": 0.0, "TanhDistortion": 0.0,
                                                     "PitchShift": 0.0, "BandStopFilter": 0.0, "AddColorNoise": 0.0,
                                                     "AddBackgroundNoise": 0.0, "Gain": 1.0, "RIR": 0.0},
                         impulse_paths=rirs, background_paths=backgrounds,
                         min_jitter_s=0.0, max_jitter_s=0.0)

    def clips(sub, split=0.1):
        return Clips(input_directory=os.path.join(work, sub), file_pattern="**/*.wav",
                     random_split_seed=10, split_count=split)

    sets = {
        "real_positive": lambda: (clips("real/positive", 0.15), light, 10),
        "real_negative": lambda: (clips("background/household", 0.15), plain, 2),
        "synthetic_negative": lambda: (clips("synthetic/negative"), full, 1),
        "synthetic_positive": lambda: (clips("synthetic/positive"), full, 2),
        # what fired the model in real use: the room and the DSP are in them, a little gain and
        # noise make each repetition differ
        "hard_negative": lambda: (clips("hard/negative", 0.1), light, 5),
        "tv_negative": lambda: (clips("hard/tv", 0.1), light, 2),
        "field_positive": lambda: (clips("hard/positive", 0.1), light, 10),  # the word ends the clip
    }
    for name in sys.argv[2:] or sets:
        c, aug, repeat = sets[name]()
        write_set(work, name, c, aug, repeat)


if __name__ == "__main__":
    np.random.seed(10)
    main()
