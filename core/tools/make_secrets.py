"""Convert the M5 project's include/secrets.h into the Echo's private JSON config.

Writes devices/echo/local/assistant/secrets.json (git-ignored). Never prints values.
Usage: py -3 tools/make_secrets.py
"""
import json
import os
import re

from extract_prompts import CORE, M5, REPO, parse, strip_comments

OUT = os.path.join(REPO, 'devices', 'echo', 'local', 'assistant', 'secrets.json')


def main():
    path = os.path.join(M5, 'secrets.h')
    known = parse(path, {})
    # Macros that are no single string or number (brace lists such as SPOTIFY_ACCOUNTS,
    # PHONE_CALLERS, WATCHED_MAILBOXES): their raw text, comments removed; the Go side parses it.
    src = strip_comments(open(path, encoding='utf-8').read())
    for m in re.finditer(r'#define\s+([A-Z_][A-Z0-9_]*)[ \t]*((?:[^\n]*\\\n)*[^\n]*)', src):
        name, raw = m.group(1), m.group(2).replace('\\\n', ' ').strip()
        if name not in known and raw:
            known[name] = raw
    os.makedirs(os.path.dirname(OUT), exist_ok=True)
    with open(OUT, 'w', encoding='utf-8', newline='\n') as f:
        json.dump(known, f, ensure_ascii=False, indent=1, sort_keys=True)
        f.write('\n')
    print('%d values -> %s' % (len(known), OUT))
    print('names: ' + ', '.join(sorted(known)))


if __name__ == '__main__':
    main()
