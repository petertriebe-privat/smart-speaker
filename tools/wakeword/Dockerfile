# The wake word training in a container (#61): it cannot freeze the PC (memory and CPU limits),
# and its progress is visible with `docker logs -f` and `docker stats`. The PC froze on 20.10.2026
# while training natively on Windows. See README.md.
#
#   docker build -t jarvis-wakeword tools/wakeword
#   docker run -d --name wakeword-v1 --memory 16g --memory-swap 16g --cpus 6 \
#       -v E:/wakeword-work:/work -v "$PWD/tools/wakeword:/tools" jarvis-wakeword \
#       python /tools/train.py /work v1 10000
FROM python:3.11-slim

RUN apt-get update && apt-get install -y --no-install-recommends git libsndfile1 \
    && rm -rf /var/lib/apt/lists/*

RUN pip install --no-cache-dir "tensorflow>=2.18" audiomentations audio_metadata "datasets<4" \
    mmap_ninja "numpy>=2" pymicro-features pyyaml webrtcvad-wheels ai-edge-litert tensorboard \
    soundfile scipy

# microWakeWord at the commit the training was tested with
RUN git clone https://github.com/kahrendt/microWakeWord /opt/microWakeWord \
    && git -C /opt/microWakeWord checkout 4665173 \
    && pip install --no-cache-dir --no-deps -e /opt/microWakeWord

ENV PYTHONUNBUFFERED=1
WORKDIR /work
