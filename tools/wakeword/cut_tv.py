"""Long recordings without the wake word (TV, talk) as negatives for training and test (#61).

    python cut_tv.py <work_dir> <recording.wav> [test_every]     (default 4)

Record in the room with `jarvis rawcapture <s> <dir>` while the TV runs and nobody says "Jarvis";
use micsout.wav (what the wake word hears). The recording is cut into 30 s blocks: every
<test_every>-th block goes whole to hard/tv_test (for wakeeval: false detections per hour), the rest
in 3 s pieces to hard/tv (prepare.py: tv_negative). Blocks interleave, so both halves hold the same
programmes.
"""
import os
import sys

import soundfile as sf

BLOCK_S, PIECE_S = 30, 3


def main():
    work, path = sys.argv[1], sys.argv[2]
    test_every = int(sys.argv[3]) if len(sys.argv) > 3 else 4
    audio, rate = sf.read(path, dtype="int16")
    stem = os.path.splitext(os.path.basename(path))[0]
    train_dir, test_dir = os.path.join(work, "hard", "tv"), os.path.join(work, "hard", "tv_test")
    os.makedirs(train_dir, exist_ok=True)
    os.makedirs(test_dir, exist_ok=True)
    block, piece = BLOCK_S * rate, PIECE_S * rate
    pieces = tests = 0
    for b, start in enumerate(range(0, len(audio), block)):
        chunk = audio[start:start + block]
        if (b + 1) % test_every == 0:
            sf.write(os.path.join(test_dir, f"{stem}_{b:03d}.wav"), chunk, rate, subtype="PCM_16")
            tests += 1
            continue
        for i in range(0, len(chunk) - piece // 2, piece):
            sf.write(os.path.join(train_dir, f"{stem}_{b:03d}_{i // piece:02d}.wav"), chunk[i:i + piece], rate,
                     subtype="PCM_16")
            pieces += 1
    print(f"{pieces} pieces of {PIECE_S} s in {train_dir}, {tests} blocks of {BLOCK_S} s in {test_dir}")


if __name__ == "__main__":
    main()
