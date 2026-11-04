#!/bin/sh
# The whole training after the recordings (#61), in Git Bash on the PC:
#   tools/wakeword/pipeline.sh <name> [steps] [export]   (export: a run that broke off, best weights)
#   tools/wakeword/pipeline.sh <name> <steps> resume <done>   (go on from its last checkpoint at step <done>)
# features of the samples (prepare.py, natively; sets already made are skipped), training in the
# Docker container (README.md: natively it froze the PC), export, evaluation on the guided
# recordings. Work directory E:/wakeword-work (fetch.py, generate.py, cut_takes.py first).
# Progress: docker logs -f wakeword-<name>; the rest in E:/wakeword-work/<name>.log
set -e
NAME=${1:?name}
STEPS=${2:-10000}
MODE=${3:-}
DONE=${4:-}
WORK=E:/wakeword-work
PY="$WORK/venv/Scripts/python.exe -I -W ignore"
HERE=$(cd "$(dirname "$0")" && pwd)
REPO=$(cd "$HERE/../.." && pwd)
LOG="$WORK/$NAME.log"
{
    echo "== $(date) prepare"
    $PY "$HERE/prepare.py" "$WORK" real_positive real_negative synthetic_negative synthetic_positive \
        hard_negative tv_negative field_positive
    echo "== $(date) train $NAME $STEPS (docker logs -f wakeword-$NAME)"
    docker rm -f "wakeword-$NAME" >/dev/null 2>&1 || :
    # 13 GB and 4 of 8 threads: with 20 GB and 6 the 32 GB PC hung (27.10.2026); over the limit
    # Docker kills the container, not the PC
    MSYS_NO_PATHCONV=1 docker run --name "wakeword-$NAME" --memory 13g --memory-swap 13g --cpus 4 \
        -v "$WORK:/work" -v "$(cygpath -m "$HERE"):/tools" jarvis-wakeword \
        python /tools/train.py /work "$NAME" "$STEPS" $MODE $DONE
    echo "== $(date) evaluate"
    $PY "$HERE/evaluate.py" "$WORK/models/$NAME/jarvis.tflite" "$REPO/local/wakeword/recordings" \
        "$WORK/real/takes.csv" "$REPO/core"
    echo "== $(date) done"
} > "$LOG" 2>&1
