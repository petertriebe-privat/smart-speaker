# Speaker/microphone loopback check for the Echo (Python 2.7, no numpy).
# Plays a tone through PulseAudio while recording the 16 kHz mic source and
# prints the RMS level per 100 ms window, so speaker and mics can be verified
# without anyone listening.
import array, math, os, subprocess, sys, time

RUN = '/mnt/echo-audio'
ENV = dict(os.environ, PULSE_COOKIE=RUN + '/pulse-cookie',
           PULSE_SERVER='unix:' + RUN + '/pulse-native')

def tone(freq, seconds, amp, rate=48000):
    n = int(rate * seconds)
    a = array.array('h')
    for i in range(n):
        ramp = min(1.0, i / 480.0, (n - i) / 480.0)
        v = int(32767 * amp * ramp * math.sin(2 * math.pi * freq * i / rate))
        a.append(v); a.append(v)
    return a.tostring()

def rms_windows(raw, rate=16000, win=0.1):
    a = array.array('h'); a.fromstring(raw[:len(raw) // 2 * 2])
    step = int(rate * win)
    out = []
    for s in range(0, len(a) - step + 1, step):
        acc = 0
        for v in a[s:s + step]:
            acc += v * v
        out.append(math.sqrt(acc / float(step)))
    return out

def main():
    freq = float(sys.argv[1]) if len(sys.argv) > 1 else 1000
    amp = float(sys.argv[2]) if len(sys.argv) > 2 else 0.2
    source = sys.argv[3] if len(sys.argv) > 3 else '/BMicsOut.shm'
    rec_path = RUN + '/loopback.raw'
    if os.path.exists(rec_path):
        os.remove(rec_path)
    if source.startswith('/'):
        # DSP shared lockless buffer, 16 kHz mono s16le
        rec = subprocess.Popen(['/usr/local/bin/shmbuf_tool', '-m', '2', '-S', source,
                                '-o', rec_path, '-p', '0'],
                               stdout=open('/dev/null', 'w'), stderr=subprocess.STDOUT)
    else:
        rec = subprocess.Popen(['parec', '-d', source, '--raw', '--format=s16le',
                                '--rate=16000', '--channels=1'],
                               stdout=open(rec_path, 'wb'), env=ENV)
    time.sleep(1.5)
    play = subprocess.Popen(['pacat', '--raw', '--format=s16le', '--rate=48000',
                             '--channels=2'], stdin=subprocess.PIPE, env=ENV)
    play.communicate(tone(freq, 1.0, amp))
    time.sleep(1.5)
    rec.terminate(); rec.wait()
    raw = open(rec_path, 'rb').read()
    levels = rms_windows(raw)
    print 'samples=%d' % (len(raw) // 2)
    print 'rms_100ms=' + ' '.join('%d' % v for v in levels)
    if levels:
        quiet = sorted(levels)[len(levels) // 4]
        print 'noise_floor=%d peak=%d' % (quiet, max(levels))

main()
