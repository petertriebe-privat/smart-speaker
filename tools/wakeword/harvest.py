"""Hard negatives from the devices' wake word clips (#61).

    python harvest.py <work_dir> <secrets.h> <test_every> <sessions.jsonl>[,...] <clips_dir> [...]

The Echos keep 3 s of microphone before every detection and every near miss
(state_dir/wake/<time>_<hit|near|second>_<peak>_<music|quiet>[_silent].wav). Most of them are false
detections by talk and TV in the room: what the model has to learn not to fire on. Some hold a real
"Jarvis": those become positives in the room's own sound. Each clip is transcribed once (OpenAI,
cached in <work_dir>/hard/labels.csv) and matched with the conversation it started
(state_dir/memory/sessions.jsonl of the devices).

A clip is a positive only when a real conversation with a request followed: people also say
"Jarvis" after a false start ("was will der jetzt?"). So every clip with a "Jarvis" in its
transcript or a conversation after it is listed in hard/review.csv and left out until
hard/verdicts.csv (clip,verdict: positive | negative | skip) decides; all others are negatives.

Since 22.10.2026 (#78) every hit has a record next to it (<clip>.json, cmd/assistant/wakeclips.go:
peer score, who answered, whether GPT Live heard the user, the quick command done, tools, seconds).
With it a hit is labelled without a transcription (auto_verdict):
- positive: a request was done (a quick command or a tool), here or on the Echo that answered;
- negative: nobody spoke, no other Echo heard it, and no request followed on any device within
  REPEAT_S (a lost command repeated means the first "Jarvis" was real too);
- everything else goes to review, transcribed as before.
A near miss up to NEAR_BEFORE_S before a positive hit of the same device goes to review: likely the
user's first, missed "Jarvis". hard/verdicts.csv still overrides every automatic label.

Every <test_every>-th negative and positive of a device (by time, so every day and every TV evening
is in both) goes to hard/test or hard/positive_test, never trained on: evaluate the old and the new
model on them with `wakeeval`. The rest go to hard/negative and hard/positive (prepare.py: the
hard_negative and field_positive features). The clips stay outside the repository.
"""
import concurrent.futures
import csv
import glob
import json
import os
import re
import shutil
import sys
import time

import requests

MODEL = "gpt-4o-mini-transcribe"
MATCH_BEFORE_S, MATCH_AFTER_S = 5, 15  # a conversation's start against the clip's time
REPEAT_S = 60  # a request this soon after a silent hit: the user repeated a lost command
SAME_WAKE_S = 3  # two Echos' hits this close are the same word (internal/peer: sameWake)
NEAR_BEFORE_S = 10  # a near miss this soon before a real hit: the user's first, missed "Jarvis"
# "Jarvis" as a German transcriber writes it, and what it hears instead
JARVIS = re.compile(r"j[ae]h?r?[vw]i[sß]|dsch[ae]h?r?[vw]i|tsch[ae]h?r?[vw]i|charv|jervis|travis|service", re.I)


def api_key(secrets):
    with open(secrets, encoding="utf-8") as f:
        m = re.search(r'#define\s+OPENAI_API_KEY\s+"([^"]+)"', f.read())
    if not m:
        sys.exit("no OPENAI_API_KEY in " + secrets)
    return m.group(1)


def transcribe(key, path):
    with open(path, "rb") as f:
        r = requests.post("https://api.openai.com/v1/audio/transcriptions",
                          headers={"Authorization": "Bearer " + key},
                          files={"file": (os.path.basename(path), f, "audio/wav")},
                          data={"model": MODEL, "language": "de"}, timeout=60)
    r.raise_for_status()
    return r.json().get("text", "").strip().replace("\n", " ")


def load_sessions(paths):
    """Start time (unix) -> "title: summary" of every conversation on the speaker. The devices share
    memory/sessions.jsonl, so it does not say which device answered: matched by time only (another
    device answering the same words still means they were spoken)."""
    sessions = {}
    for path in paths:
        with open(path, encoding="utf-8") as f:
            for line in f:
                try:
                    s = json.loads(line)
                except ValueError:
                    continue
                if s.get("channel") == "speaker":
                    sessions[int(s["t"])] = f'{s.get("title", "")}: {s.get("summary", "")}'
    return sessions


def session_after(sessions, name):
    """The conversation that the detection of clip <name> (YYYYMMDD-HHMMSS_..., local time) started."""
    t = time.mktime(time.strptime(name[:15], "%Y%m%d-%H%M%S"))
    near = [s for s in sessions if -MATCH_BEFORE_S <= s - t <= MATCH_AFTER_S]
    return sessions[min(near, key=lambda s: abs(s - t))] if near else ""


def clip_time(name):
    return time.mktime(time.strptime(name[:15], "%Y%m%d-%H%M%S"))


def load_records(clips):
    """<device>/<file> -> its record (the .json next to a hit), for the clips that have one."""
    records = {}
    for clip, path in clips.items():
        try:
            with open(path[:-len(".wav")] + ".json", encoding="utf-8") as f:
                records[clip] = json.load(f)
        except (OSError, ValueError):
            pass
    return records


def request_done(r):
    return bool(r.get("done")) or r.get("tools", 0) > 0


def auto_verdicts(clips, records):
    """<device>/<file> -> positive | negative | review, from the records (see the module comment).
    Clips without a record are not in it."""
    hits = sorted((clip_time(c.split("/")[1]), c) for c in records)
    done = [(t, c) for t, c in hits if request_done(records[c])]

    def answered_elsewhere(t, clip):
        # the Echo that answered the same word (its record says what followed)
        dev = clip.split("/")[0]
        return any(abs(t2 - t) <= SAME_WAKE_S and c2.split("/")[0] != dev for t2, c2 in done)

    verdicts = {}
    for t, clip in hits:
        r = records[clip]
        if request_done(r) or (r.get("answered") == "other" and answered_elsewhere(t, clip)):
            verdicts[clip] = "positive"
        elif (not r.get("heard_user") and r.get("answered") == "here" and not r.get("peer")
              and not any(0 < t2 - t <= REPEAT_S for t2, _ in done)):
            verdicts[clip] = "negative"
        else:
            verdicts[clip] = "review"
    for clip in clips:
        dev, name = clip.split("/")
        if "_near_" not in name:
            continue
        t = clip_time(name)
        if any(0 < t2 - t <= NEAR_BEFORE_S and c2.split("/")[0] == dev and verdicts.get(c2) == "positive"
               for t2, c2 in hits):
            verdicts[clip] = "review"
    return verdicts


def main():
    work, secrets, test_every = sys.argv[1], sys.argv[2], int(sys.argv[3])
    sessions, dirs = load_sessions(sys.argv[4].split(",")), sys.argv[5:]
    hard = os.path.join(work, "hard")
    os.makedirs(hard, exist_ok=True)
    labels_path = os.path.join(hard, "labels.csv")
    labels = {}
    if os.path.exists(labels_path):
        with open(labels_path, newline="", encoding="utf-8") as f:
            labels = {row["clip"]: row["text"] for row in csv.DictReader(f)}

    clips = {}  # "<device>/<file>" -> path
    for d in dirs:
        device = os.path.basename(os.path.normpath(d))
        for p in sorted(glob.glob(os.path.join(d, "*.wav"))):
            clips[f"{device}/{os.path.basename(p)}"] = p

    records = load_records(clips)
    auto = auto_verdicts(clips, records)
    # a clip labelled from its record needs no transcription
    todo = [c for c in clips if c not in labels and auto.get(c) in (None, "review")]
    print(f"{len(clips)} clips, {len(records)} with a record ({sum(v != 'review' for v in auto.values())} "
          f"labelled from it), {len(todo)} to transcribe", flush=True)
    key = api_key(secrets) if todo else ""
    with concurrent.futures.ThreadPoolExecutor(8) as pool:
        for clip, text in zip(todo, pool.map(lambda c: transcribe(key, clips[c]), todo), strict=True):
            labels[clip] = text
    with open(labels_path, "w", newline="", encoding="utf-8") as f:
        w = csv.DictWriter(f, ["clip", "text"])
        w.writeheader()
        for clip in sorted(labels):
            w.writerow({"clip": clip, "text": labels[clip]})
    for clip in clips:
        labels.setdefault(clip, "")

    verdicts = {}
    verdicts_path = os.path.join(hard, "verdicts.csv")
    if os.path.exists(verdicts_path):
        with open(verdicts_path, newline="", encoding="utf-8") as f:
            verdicts = {row["clip"]: row["verdict"] for row in csv.DictReader(f)}

    subs = ("negative", "test", "positive", "positive_test", "review")
    for sub in subs:
        shutil.rmtree(os.path.join(hard, sub), ignore_errors=True)
        os.makedirs(os.path.join(hard, sub))
    counts = dict.fromkeys(subs, 0)
    seen = {}  # (device, kind) -> clips so far, for the test split
    review = []
    for clip, path in clips.items():
        device, name = clip.split("/")
        talk = session_after(sessions, name)
        verdict = verdicts.get(clip, auto.get(clip))
        if verdict is None:
            # a "Jarvis" or a conversation after it: decided by hand in verdicts.csv (people say
            # "Jarvis" about a false start too; the transcriber misses a real one now and then)
            verdict = "review" if JARVIS.search(labels[clip]) or talk else "negative"
        if verdict in ("review", "skip"):
            sub = "review" if verdict == "review" else None
        else:
            seen[device, verdict] = seen.get((device, verdict), 0) + 1
            held = seen[device, verdict] % test_every == 0
            sub = {"negative": "test" if held else "negative",
                   "positive": "positive_test" if held else "positive"}[verdict]
        if verdict == "review":
            record = records.get(clip)
            review.append({"clip": clip, "text": labels[clip], "conversation": talk,
                           "record": json.dumps(record, ensure_ascii=False) if record else ""})
        if sub:
            counts[sub] += 1
            shutil.copy(path, os.path.join(hard, sub, f"{device}_{name}"))
    with open(os.path.join(hard, "review.csv"), "w", newline="", encoding="utf-8") as f:
        w = csv.DictWriter(f, ["clip", "text", "conversation", "record"])
        w.writeheader()
        w.writerows(review)
    print(", ".join(f"{k} {v}" for k, v in counts.items()))


if __name__ == "__main__":
    main()
