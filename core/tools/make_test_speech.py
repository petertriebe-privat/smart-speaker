"""Make a spoken test utterance as raw s16le 16 kHz mono (for `jarvis ctl inject`).

Uses OpenAI's speech API with the key from local/secrets.json.
Usage: py -3 tools/make_test_speech.py "Wie spät ist es?" out.raw
"""
import array
import json
import sys
import urllib.request

from make_secrets import OUT as SECRETS


def main():
    text, out = sys.argv[1], sys.argv[2]
    key = json.load(open(SECRETS, encoding='utf-8'))['OPENAI_API_KEY']
    body = json.dumps({'model': 'gpt-4o-mini-tts', 'voice': 'ash', 'input': text,
                       'response_format': 'pcm', 'instructions': 'Speak German, natural pace.'}).encode()
    req = urllib.request.Request('https://api.openai.com/v1/audio/speech', body,
                                 {'Authorization': 'Bearer ' + key, 'Content-Type': 'application/json'})
    pcm24 = array.array('h', urllib.request.urlopen(req, timeout=60).read())
    # 24 kHz -> 16 kHz: two output samples per three input samples (averaged neighbours).
    out16 = array.array('h')
    for i in range(0, len(pcm24) - 2, 3):
        a, b, c = pcm24[i], pcm24[i + 1], pcm24[i + 2]
        out16.append((2 * a + b) // 3)
        out16.append((b + 2 * c) // 3)
    silence = array.array('h', [0] * 16000)  # 1 s before and after: like a real pause
    with open(out, 'wb') as f:
        (silence + out16 + silence).tofile(f)
    print('%s: %.1f s' % (out, len(out16) / 16000.0))


if __name__ == '__main__':
    main()
