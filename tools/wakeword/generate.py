"""Synthetic samples for the wake word "Jarvis" (#61): positives and sound-alike negatives.

    python generate.py <work_dir> [scale]     (scale 1.0 = the full set; 0.02 for a quick test)

Writes <work_dir>/synthetic/{positive,negative}/<voice>/<n>.wav (the voice's own rate; microWakeWord
resamples to 16 kHz when it loads them). One job per voice and kind, the texts cycled from a file
(loading a voice takes seconds: one process per text was ten times slower). Jobs already complete
are skipped, so it can be stopped and run again.

The spellings come from tools/wakeword/spellings.py (20.10.2026): German Piper voices say "Jarvis"
only spelled "Dschaarvis"/"Dschahrwis"/"Dscharwis" (scored 0.8-1.0 by the stock English model as
"Hey ..."); the English LibriTTS-R generator (904 speakers) says "Jarvis" itself (1.00). The MLS
voices babble (3.7-4.8 s for two words) and are left out, kerstin and thorsten_emotional too.
"""
import os
import subprocess
import sys

EN = "en_US-libritts_r-medium.pt"
DE = ["de_DE-thorsten-medium.onnx", "de_DE-thorsten-high.onnx", "de_DE-karlsson-low.onnx",
      "de_DE-pavoque-low.onnx", "de_DE-eva_k-x_low.onnx", "de_DE-ramona-low.onnx"]
LENGTHS = ["0.8", "0.9", "1.0", "1.1", "1.25"]

POS_EN = ["Jarvis", "Jarvis!", "Jarvis?", "Jarvis.", "Jarvis,"]
POS_DE = ["Dschaarvis", "Dschahrwis", "Dscharwis", "Dschaarvis!", "Dschahrwis!", "Dscharwis?"]

# Sound-alikes and parts of the word: the model must not fire on them (German household, some
# English). Said alone and in short phrases.
NEG_DE = ["Paris", "Davis", "Garten", "Jahres", "darf ich", "Service", "Charlie", "Marvin", "Harvey",
          "Gras", "Dschungel", "Jazz", "Java", "Arvid", "Doris", "Tschüss", "Chaos", "Kasse", "Dach",
          "Schaf", "Schal", "Jas", "Jahr", "Dschar", "Arvis", "Wiese", "Ja, wieso", "Mars", "Kurwis",
          "das war es", "schade", "ach so", "na klar", "Tschau", "Der Garten ist schön", "Ja, bitte"]
NEG_EN = ["Paris", "Davis", "Jarred", "Marvin", "Harvey", "Charles", "jar", "jars", "Jervis", "Travis",
          "Mavis", "service", "nervous", "carvings", "car wash", "jarvy", "Javis", "Elvis", "artist",
          "hey there", "okay", "Garbage", "starving", "Jordan", "Jasmine", "Gervais"]

GEN = ["--batch-size", "10", "--max-speakers", "800"]  # LibriTTS-R: later speakers have artefacts


def jobs(scale):
    n = lambda k: max(5, int(k * scale))
    yield "positive", "en_libritts", EN, POS_EN, n(6000), GEN
    yield "negative", "en_libritts", EN, NEG_EN, n(2000), GEN
    for v in DE:
        name = "de_" + "_".join(v.split(".")[0].split("-")[1:])
        yield "positive", name, v, POS_DE, n(500), []
        yield "negative", name, v, NEG_DE, n(250), []


def main():
    work = sys.argv[1]
    scale = float(sys.argv[2]) if len(sys.argv) > 2 else 1.0
    env = dict(os.environ, PYTHONPATH=os.path.join(work, "piper-sample-generator"))
    texts = os.path.join(work, "synthetic", "texts")
    os.makedirs(texts, exist_ok=True)
    all_jobs = list(jobs(scale))
    for i, (kind, name, model, lines, count, extra) in enumerate(all_jobs):
        out = os.path.join(work, "synthetic", kind, name)
        if os.path.isdir(out) and len(os.listdir(out)) >= count:
            continue
        tf = os.path.join(texts, f"{kind}_{name}.txt")
        with open(tf, "w", encoding="utf-8") as f:
            f.write("\n".join(lines) + "\n")
        cmd = [sys.executable, "-m", "piper_sample_generator", tf, "--model", os.path.join(work, "voices", model),
               "--max-samples", str(count), "--output-dir", out, "--length-scales", *LENGTHS] + extra
        r = subprocess.run(cmd, env=env, capture_output=True, text=True)
        if r.returncode != 0:
            sys.exit(f"{name}: {r.stderr[-2000:]}")
        print(f"[{i + 1}/{len(all_jobs)}] {kind} {name}: {count}", flush=True)


if __name__ == "__main__":
    main()
