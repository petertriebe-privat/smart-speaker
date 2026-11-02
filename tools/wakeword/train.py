"""Trains the wake word "Jarvis" with microWakeWord and exports it for the devices (#61).

    python train.py <work_dir> <name> [steps]      (default 10000 steps)
    python train.py <work_dir> <name> [steps] export   only export trained_models/<name>'s best weights
                                                       (after a run that broke off)
    python train.py <work_dir> <name> <steps> resume <done>   go on with a run that broke off: from its
                                                       last checkpoint (the last new best weights, at step
                                                       <done>), only the steps still missing

Uses the features of prepare.py and microWakeWord's negative datasets. The model is microWakeWord's
mixednet as in its notebook: the same layers as the stock hey_jarvis, so the Echo's Go interpreter
and the M5 load it unchanged (check with wakeeval). Output:

    <work_dir>/models/<name>/jarvis.tflite     the streaming int8 model
    <work_dir>/models/<name>/jarvis.json       its manifest (sliding window, cutoff guess)

On the Echo: copy both to /mnt/data/jarvis/wakemodel/ and set "wake_model": "wakemodel/jarvis.tflite"
(not wake/: the wake word clips there are pruned to the newest 400).
Training runs on the CPU on Windows (no GPU in native TensorFlow); it resumes from its checkpoint.
"""
import json
import os
import re
import shutil
import subprocess
import sys
import time

import yaml

# The ambient validation set cut into overlapping windows is the training's memory peak: all 9.7 h of
# dinner_party_eval needed ~12 GB and hung the PC twice (22.10., 27.10.2026). Half of it is enough
# to pick the best weights; the field clips judge the model (README.md).
AMBIENT_EVAL = "negative_datasets/dinner_party_eval_half"
EVAL_INTERVAL = 1000


def make_ambient_eval(work):
    """dinner_party_eval with the recordings of validation_ambient that fit into half its length, the
    testing set unchanged; made once."""
    from mmap_ninja.ragged import RaggedMmap
    out = os.path.join(work, AMBIENT_EVAL)
    if os.path.isdir(out):
        return
    full = os.path.join(work, "negative_datasets", "dinner_party_eval")
    tmp = out + ".tmp"
    shutil.rmtree(tmp, ignore_errors=True)
    shutil.copytree(os.path.join(full, "testing_ambient"), os.path.join(tmp, "testing_ambient"))
    os.makedirs(os.path.join(tmp, "validation_ambient"))
    for name in os.listdir(os.path.join(full, "validation_ambient")):
        m = RaggedMmap(os.path.join(full, "validation_ambient", name))
        lengths = [m[i].shape[0] for i in range(len(m))]
        keep, total = [], 0
        for i, n in enumerate(lengths):
            if total + n <= sum(lengths) / 2:
                keep.append(i)
                total += n
        RaggedMmap.from_generator(out_dir=os.path.join(tmp, "validation_ambient", name),
                                  sample_generator=(m[i] for i in keep), batch_size=1, verbose=False)
        print(f"ambient validation {name}: {len(keep)} of {len(m)} recordings, {total / 360000:.1f} h", flush=True)
    os.rename(tmp, out)


def remaining_phases(steps, done):
    """The two phases (steps, then steps // 2) as (index, steps) without the first `done` steps: microWakeWord
    restores the checkpoint's weights and optimizer but counts its steps from 1 again."""
    phases = []
    for i, n in enumerate([steps, steps // 2]):
        left = n - min(n, done)
        done -= min(n, done)
        if left:
            phases.append((i, left))
    return phases


def config(work, name, steps, done=0):
    phases = remaining_phases(steps, done)
    per_phase = lambda values: [values[i] for i, _ in phases]
    f = lambda d: os.path.join(work, d)
    pos = lambda d, w: {"features_dir": f(d), "sampling_weight": w, "penalty_weight": 1.0, "truth": True,
                        "truncation_strategy": "truncate_start", "type": "mmap"}
    neg = lambda d, w, t="random": {"features_dir": f(d), "sampling_weight": w, "penalty_weight": 1.0,
                                    "truth": False, "truncation_strategy": t, "type": "mmap"}
    return {
        "window_step_ms": 10,
        "train_dir": f(f"trained_models/{name}"),
        "features": [
            pos("features/synthetic_positive", 2.0),
            # repeated 10x in the training split already; weight 1.0 in v1 heard only 69 % of the
            # (mostly trained) takes and 28 % with music: they carry the Echo's DSP, the TTS does not
            pos("features/real_positive", 3.0),
            pos("features/field_positive", 3.0),  # v3: real "Jarvis" that started a conversation
            neg("features/synthetic_negative", 3.0),
            neg("features/real_negative", 3.0),  # the household's TV, music and talk on the Echo
            # v3: v2 fired on TV and talk in the living room every few minutes (21.10.2026); the
            # detection is at the end of each clip, so keep the end
            neg("features/hard_negative", 6.0, "truncate_start"),
            neg("features/tv_negative", 4.0),
            neg("negative_datasets/speech", 10.0),
            neg("negative_datasets/dinner_party", 10.0),
            neg("negative_datasets/no_speech", 5.0),
            neg(AMBIENT_EVAL, 0.0, "split"),  # validation and test only
        ],
        # two phases: then half as many steps with a small learning rate and SpecAugment (masks in
        # time and frequency: robustness against music and TV)
        "training_steps": [n for _, n in phases],
        "positive_class_weight": per_phase([1, 1]),
        "negative_class_weight": per_phase([20, 20]),
        "learning_rates": per_phase([0.001, 0.0002]),
        "batch_size": 128,
        "time_mask_max_size": per_phase([0, 5]),
        "time_mask_count": per_phase([0, 2]),
        "freq_mask_max_size": per_phase([0, 5]),
        "freq_mask_count": per_phase([0, 2]),
        # every validation is the training's memory peak (the ambient set in windows): half as many
        "eval_step_interval": EVAL_INTERVAL,
        "clip_duration_ms": 1500,
        "target_minimization": 0.9,
        "minimization_metric": None,
        "maximization_metric": "average_viable_recall",
    }


BATCH = re.compile(r"Validation Batch #(\d+): Accuracy = ([\d.]+); Recall = ([\d.]+); "
                   r"Precision = ([\d.]+); Loss = ([\d.]+); Mini-Batch #(\d+)")


def run_with_progress(cmd, steps, log_path, every=15.0):
    """Runs the training; its full output goes to log_path, the console gets one line every `every`
    seconds (microWakeWord overwrites one line with \\r per mini-batch) and the validations."""
    env = dict(os.environ, PYTHONUNBUFFERED="1")
    p = subprocess.Popen(cmd, stdout=subprocess.PIPE, stderr=subprocess.STDOUT, env=env)
    started, last_print, line = time.time(), 0.0, b""
    first_step = None
    with open(log_path, "ab") as log:
        while True:
            ch = p.stdout.read(1)
            if not ch:
                break
            log.write(ch)
            if ch not in (b"\r", b"\n"):
                line += ch
                continue
            text, line = line.decode("utf-8", "replace"), b""
            m = BATCH.search(text)
            if m:
                step = (int(m.group(1)) - 1) * EVAL_INTERVAL + int(m.group(6))
                if first_step is None:
                    first_step, started = step, time.time()
                now = time.time()
                if now - last_print >= every:
                    last_print = now
                    rate = (step - first_step) / max(1e-6, now - started)
                    eta = (steps - step) / rate / 60 if rate > 0 else 0
                    print(f"{time.strftime('%H:%M:%S')}  step {step}/{steps} ({100 * step / steps:.0f} %)  "
                          f"loss {float(m.group(5)):.4f}  recall {float(m.group(3)):.2f}  "
                          f"precision {float(m.group(4)):.2f}  {rate:.1f} steps/s  ~{eta:.0f} min left", flush=True)
            elif "Step" in text and ("Validation" in text or "rate" in text) or "Error" in text or "Traceback" in text:
                print(time.strftime("%H:%M:%S"), text.strip()[:400], flush=True)
    return p.wait()


def main():
    work, name = sys.argv[1], sys.argv[2]
    steps = int(sys.argv[3]) if len(sys.argv) > 3 else 10000
    export_only = sys.argv[4:5] == ["export"]
    done = int(sys.argv[5]) if sys.argv[4:5] == ["resume"] else 0
    make_ambient_eval(work)
    cfg = config(work, name, steps, done)
    missing = [x["features_dir"] for x in cfg["features"] if not os.path.isdir(x["features_dir"])]
    if missing:  # a trial run before all features are made
        print("left out (not made yet):", ", ".join(missing))
        cfg["features"] = [x for x in cfg["features"] if x["features_dir"] not in missing]
    cfg_path = os.path.join(work, f"training_{name}.yaml")
    with open(cfg_path, "w") as f:
        yaml.dump(cfg, f)
    cmd = [sys.executable, "-m", "microwakeword.model_train_eval", f"--training_config={cfg_path}",
           "--train", "0" if export_only else "1", "--restore_checkpoint", "1",
           "--test_tf_nonstreaming", "0", "--test_tflite_nonstreaming", "0",
           "--test_tflite_nonstreaming_quantized", "0", "--test_tflite_streaming", "0",
           "--test_tflite_streaming_quantized", "1", "--use_weights", "best_weights",
           "mixednet", "--pointwise_filters", "64,64,64,64", "--repeat_in_block", "1, 1, 1, 1",
           "--mixconv_kernel_sizes", "[5], [7,11], [9,15], [23]", "--residual_connection", "0,0,0,0",
           "--first_conv_filters", "32", "--first_conv_kernel_size", "5", "--stride", "3"]
    if run_with_progress(cmd, sum(cfg["training_steps"]),os.path.join(work, f"train_{name}.log")) != 0:
        sys.exit("training failed: see the log")
    src = os.path.join(work, "trained_models", name, "tflite_stream_state_internal_quant",
                       "stream_state_internal_quant.tflite")
    out = os.path.join(work, "models", name)
    os.makedirs(out, exist_ok=True)
    shutil.copy(src, os.path.join(out, "jarvis.tflite"))
    with open(os.path.join(out, "jarvis.json"), "w") as f:
        json.dump({"type": "micro", "wake_word": "Jarvis", "author": "household training (tools/wakeword)",
                   "model": "jarvis.tflite", "trained_languages": ["de", "en"], "version": 2,
                   "micro": {"probability_cutoff": 0.9, "feature_step_size": 10, "sliding_window_size": 5,
                             "tensor_arena_size": 30000, "minimum_esphome_version": "2024.7.0"}}, f, indent=2)
    print("model:", os.path.join(out, "jarvis.tflite"))


if __name__ == "__main__":
    main()
