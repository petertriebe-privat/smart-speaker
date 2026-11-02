"""Evaluates a wake word model on whole guided recordings with the Echo's own detector (#61).

    python evaluate.py <model.tflite|builtin> <recordings_dir> <takes.csv> <wakeeval_dir>

Runs cmd/wakeeval (the Go detector of the Echo, bit for bit) over each session's micsout.wav without
triggering (-cutoff 1: every rise with its true peak) and matches the rises to the takes of
cut_takes.py's takes.csv: a rise from the take's start to 1 s after its end is a hit; any other is a
false detection. Prints per session and cutoff: hits of takes, false detections, and per hour of
the free-talk sessions (negative*). Most takes were trained on: a new recording is the fair test.
"""
import collections
import csv
import os
import re
import subprocess
import sys

CUTOFFS = [0.5, 0.6, 0.7, 0.8, 0.9, 0.95]


def rises(model, wav, evaldir):
    args = ["go", "run", "./cmd/wakeeval", "-cutoff", "1"]
    if model != "builtin":
        args += ["-model", model]
    out = subprocess.run(args + [wav], cwd=evaldir, capture_output=True, text=True, check=True).stdout
    line = next(l for l in out.splitlines() if l.strip().startswith("mono"))
    return [(float(t), float(p)) for t, p in re.findall(r"([\d.]+)s:([\d.]+)", line.split("rises", 1)[1])]


def main():
    model, rec, takes_csv, evaldir = sys.argv[1:5]
    takes = collections.defaultdict(list)
    for row in csv.DictReader(open(takes_csv)):
        takes[(row["person"], row["session"])].append((float(row["start_s"]), float(row["end_s"])))
    total = collections.Counter()
    print(f"model {model}")
    for person in sorted(os.listdir(rec)):
        for session in sorted(os.listdir(os.path.join(rec, person))):
            wav = os.path.join(rec, person, session, "micsout.wav")
            if not os.path.isfile(wav) or not os.path.isfile(os.path.join(rec, person, session, "takes.json")):
                continue
            rs = rises(model, wav, evaldir)
            tk = takes.get((person, session), [])
            parts = []
            for c in CUTOFFS:
                above = [(t, p) for t, p in rs if p >= c]
                hit = sum(1 for a, e in tk if any(a <= t <= e + 1.0 for t, _ in above))
                false = sum(1 for t, _ in above if not any(a <= t <= e + 1.0 for a, e in tk))
                total[("hit", c)] += hit
                total[("takes", c)] += len(tk)
                total[("false", c)] += false
                if session.startswith("negative"):
                    total[("neg_false", c)] += false
                parts.append(f"{c:.2f}: {hit}/{len(tk)} +{false}")
            if session.startswith("negative"):
                total["neg_seconds"] += 90
            print(f"{session:32} {'  '.join(parts)}", flush=True)
    print("total")
    for c in CUTOFFS:
        per_hour = total[("neg_false", c)] / max(1, total["neg_seconds"]) * 3600
        print(f"  cutoff {c:.2f}: {total[('hit', c)]}/{total[('takes', c)]} takes heard, "
              f"{total[('false', c)]} false in all, {per_hour:.0f}/h in free talk")


if __name__ == "__main__":
    main()
