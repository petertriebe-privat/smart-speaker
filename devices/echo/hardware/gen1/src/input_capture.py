# Log gpio-keys and rotary-encoder events for a given time (Python 2.7).
# Usage: python input_capture.py SECONDS [OUTFILE]
import os, select, struct, sys, time

FMT = 'llHHi'  # struct input_event on 32-bit ARM
SIZE = struct.calcsize(FMT)
NAMES = {0: 'SYN', 1: 'KEY', 2: 'REL', 4: 'MSC'}

def main():
    duration = float(sys.argv[1]) if len(sys.argv) > 1 else 60
    out = open(sys.argv[2], 'a') if len(sys.argv) > 2 else sys.stdout
    fds = {}
    for dev in ('/dev/input/event0', '/dev/input/event1'):
        fds[os.open(dev, os.O_RDONLY)] = dev
    end = time.time() + duration
    while time.time() < end:
        r, _, _ = select.select(list(fds), [], [], 0.5)
        for fd in r:
            data = os.read(fd, SIZE * 16)
            for i in range(0, len(data) - SIZE + 1, SIZE):
                sec, usec, typ, code, value = struct.unpack(FMT, data[i:i + SIZE])
                if typ == 0:
                    continue
                out.write('%.3f %s %s code=%d value=%d\n' % (
                    time.time(), fds[fd], NAMES.get(typ, typ), code, value))
                out.flush()

main()
