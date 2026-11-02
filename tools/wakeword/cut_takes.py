"""Cuts the spoken "Jarvis" takes out of the guided recordings (#61, jarvis ctl enroll).

    python cut_takes.py <recordings_dir> <out_dir>

<recordings_dir>/<person>/<session>-<time>/ holds mics.wav (7 raw microphones) and micsout.wav
(BMicsOut, what the wake word hears). The beeps are found in the raw microphones (the speaker is
loud there; Goertzel at 880 Hz), so the takes the device did not count are found too. After each
beep the strongest voiced stretch in micsout.wav is the take. Written:

    <out_dir>/positive/<person>/<session>/<nnn>.wav     the take, 0.25 s before and after (micsout)
    <out_dir>/positive_raw/<person>/<session>/<nnn>.wav the same stretch, raw microphones averaged
    <out_dir>/negative/<person>/<session>_<nnn>.wav     the stretches between takes (>= 2 s, away from
                                                        beeps and takes), and the free-talk sessions
    <out_dir>/takes.csv                                 person, session, n, beep_s, start_s, end_s, snr_db

Household voices: out_dir stays outside the repository.
"""
import csv
import os
import sys
import wave

import numpy as np

RATE = 16000
CHUNK = RATE // 50  # 20 ms
BEEP_HZ = 880.0


def read_wav(path):
    with wave.open(path) as w:
        n, frames = w.getnchannels(), w.readframes(w.getnframes())
    return np.frombuffer(frames, dtype="<i2").reshape(-1, n).astype(np.float32)


def write_wav(path, x):
    os.makedirs(os.path.dirname(path), exist_ok=True)
    with wave.open(path, "wb") as w:
        w.setnchannels(1)
        w.setsampwidth(2)
        w.setframerate(RATE)
        w.writeframes(np.clip(x, -32768, 32767).astype("<i2").tobytes())


def chunks(x):
    n = len(x) // CHUNK
    return x[: n * CHUNK].reshape(n, CHUNK)


def tone_share(c, hz):
    """Per chunk: the share of energy at hz (Goertzel)."""
    t = np.arange(CHUNK) / RATE
    ref = np.exp(-2j * np.pi * hz * t)
    power = np.abs(c @ ref) ** 2 * 2 / CHUNK
    energy = (c ** 2).sum(axis=1) + 1e-9
    return power / energy


def find_beeps(raw):
    """Start times (s) of the beeps in the raw microphone signal."""
    c = chunks(raw)
    share = tone_share(c, BEEP_HZ)
    rms = np.sqrt((c ** 2).mean(axis=1))
    # In quiet the beep is most of the chunk; under music it is not, but its power at 880 Hz still
    # stands far above what the music has there.
    p880 = share * (c ** 2).sum(axis=1)
    hit = ((share > 0.5) & (rms > 4 * np.median(rms) + 1)) | ((share > 0.08) & (p880 > 30 * np.median(p880) + 1))
    # The beep lasts 150 ms: a run of at least 4 chunks (a voice's harmonic at 880 Hz comes and goes).
    beeps, run, last = [], 0, -10
    for i, h in enumerate(hit):
        run = run + 1 if h else 0
        if run == 4:
            start = i - 3
            if start - last > 25:  # 0.5 s apart: a new beep
                beeps.append(start * CHUNK / RATE)
            last = start
    return beeps


def voiced(x):
    """Per chunk RMS, with the beep's own chunks and the start/end tones silenced."""
    c = chunks(x)
    rms = np.sqrt((c ** 2).mean(axis=1))
    for hz in (BEEP_HZ, 523.0, 784.0):
        rms[tone_share(c, hz) > 0.3] = 0
    return rms


MAX_WORD = 65  # chunks: 1.3 s


def strongest_stretch(rms, lo, hi, floor):
    """The word: around the loudest chunk in [lo, hi), the chunks at most 10 dB below it (gaps up to
    140 ms bridged), at most 1.3 s. Relative to the peak, so TV speech or music next to the word does
    not join it (a session-wide threshold made stretches of up to 3.5 s with the TV on)."""
    if hi - lo < 5:
        return None
    seg = rms[lo:hi]
    p = int(np.argmax(seg))
    if seg[p] < max(3 * floor, 20):
        return None
    thr = max(seg[p] / 3.16, 2 * floor, 2.5 * np.median(seg))  # the median: music around the word
    a = e = p
    gap = 0
    while a > 0 and gap <= 7 and e - a < MAX_WORD:
        a -= 1
        gap = 0 if seg[a] > thr else gap + 1
    a += gap
    gap = 0
    while e < len(seg) - 1 and gap <= 7 and e - a < MAX_WORD:
        e += 1
        gap = 0 if seg[e] > thr else gap + 1
    e -= gap
    if e - a < 12:  # shorter than a word (240 ms): under loud music, ±0.35 s around the peak
        a, e = max(0, p - 17), min(len(seg) - 1, p + 17)
    return lo + a, lo + e + 1


def cut_session(sdir, person, session, out, rows):
    out_m = read_wav(os.path.join(sdir, "micsout.wav"))[:, 0]
    raw = read_wav(os.path.join(sdir, "mics.wav"))
    raw_mix = raw.mean(axis=1)
    n = min(len(out_m), len(raw_mix))
    out_m, raw_mix = out_m[:n], raw_mix[:n]
    if session.startswith("negative"):
        write_wav(os.path.join(out, "negative", person, f"{session}_000.wav"), out_m)
        return 0, 0
    beeps = find_beeps(raw[:n, 0])
    rms = voiced(out_m)
    floor = np.percentile(rms[rms > 0], 20) if (rms > 0).any() else 1
    pad = int(0.25 * RATE)
    taken = []
    for k, b in enumerate(beeps):
        lo = int((b + 0.15) * RATE) // CHUNK
        nxt = beeps[k + 1] if k + 1 < len(beeps) else b + 4.5
        hi = min(len(rms), int(min(nxt - 0.1, b + 4.5) * RATE) // CHUNK)
        s = strongest_stretch(rms, lo, hi, floor)
        if s is None:
            continue
        a, e = s[0] * CHUNK, s[1] * CHUNK
        snr = 20 * np.log10(rms[s[0]:s[1]].max() / max(floor, 1))
        i = len(taken)
        write_wav(os.path.join(out, "positive", person, session, f"{i:03d}.wav"), out_m[max(0, a - pad): e + pad])
        write_wav(os.path.join(out, "positive_raw", person, session, f"{i:03d}.wav"), raw_mix[max(0, a - pad): e + pad])
        rows.append([person, session, i, round(b, 2), round(a / RATE, 2), round(e / RATE, 2), round(snr, 1)])
        taken.append((a, e))
    # negatives: what lies between, away from beeps and takes
    busy = [(int(b * RATE) - RATE // 2, int(b * RATE) + RATE) for b in beeps] + [(a - RATE // 2, e + RATE // 2) for a, e in taken]
    busy.sort()
    pos, j = int(1.5 * RATE), 0
    for a, e in busy + [(n, n)]:
        if a - pos >= 2 * RATE:
            write_wav(os.path.join(out, "negative", person, f"{session}_{j:03d}.wav"), out_m[pos:a])
            j += 1
        pos = max(pos, e)
    return len(beeps), len(taken)


def main():
    rec, out = sys.argv[1], sys.argv[2]
    rows = []
    for person in sorted(os.listdir(rec)):
        for sd in sorted(os.listdir(os.path.join(rec, person))):
            sdir = os.path.join(rec, person, sd)
            if not os.path.isfile(os.path.join(sdir, "micsout.wav")):
                continue
            session = sd.rsplit("-", 2)[0]
            if os.path.exists(os.path.join(out, "positive", person, sd)):
                continue
            beeps, takes = cut_session(sdir, person, sd, out, rows)
            print(f"{person}/{sd}: {beeps} beeps, {takes} takes", flush=True)
    os.makedirs(out, exist_ok=True)
    with open(os.path.join(out, "takes.csv"), "w", newline="") as f:
        w = csv.writer(f)
        w.writerow(["person", "session", "n", "beep_s", "start_s", "end_s", "snr_db"])
        w.writerows(rows)


if __name__ == "__main__":
    main()
