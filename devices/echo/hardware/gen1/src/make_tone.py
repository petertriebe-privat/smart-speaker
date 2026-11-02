#!/usr/bin/env python
"""Generate a quiet 0.4-second PCM test tone. Compatible with Python 2.7."""
from __future__ import print_function
import hashlib
import math
import struct
import sys

path = sys.argv[1] if len(sys.argv) > 1 else '/mnt/echo-tone.wav'
rate = 48000
count = int(rate * 0.4)
frames = []
for n in range(count):
    fade = min(1.0, n / (rate * 0.02), (count - 1 - n) / (rate * 0.02))
    sample = int(32767 * 0.01 * fade * math.sin(2 * math.pi * 660 * n / rate))
    frames.append(struct.pack('<hh', sample, sample))
pcm = b''.join(frames)
header = struct.pack('<4sI4s4sIHHIIHH4sI', b'RIFF', 36 + len(pcm), b'WAVE',
                     b'fmt ', 16, 1, 2, rate, rate * 4, 4, 16, b'data', len(pcm))
with open(path, 'wb') as out:
    out.write(header)
    out.write(pcm)
with open(path, 'rb') as handle:
    data = handle.read()
print('TONE_READY bytes=%d sha256=%s' % (len(data), hashlib.sha256(data).hexdigest()))
