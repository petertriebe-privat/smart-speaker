"""Extract the assistant's prompt texts and settings from the M5 project's C++ headers.

The C++ headers stay the single source of the persona and instructions: this script reads
shared/prompts/{persona,prompts}.h and devices/atoms3r/include/config.h and writes them as JSON
for the Echo build (internal/prompts/prompts.json, embedded into the binary).

Usage: py -3 tools/extract_prompts.py  (from core)
       py -3 tools/extract_prompts.py --persona [persona_local.h [persona.json]]
"""
import json
import os
import re
import sys

HERE = os.path.dirname(os.path.abspath(__file__))
CORE = os.path.dirname(HERE)
REPO = os.path.normpath(os.path.join(CORE, '..'))  # core/ is at the repository's top
M5 = os.path.join(REPO, 'devices', 'atoms3r', 'include')
SHARED = os.path.join(REPO, 'shared', 'prompts')
LOCAL = os.path.join(REPO, 'local')  # the household's own values, git-ignored (local/README.md)
OUT = os.path.join(CORE, 'internal', 'prompts', 'prompts.json')

ESCAPES = {'n': '\n', 't': '\t', '"': '"', '\\': '\\', "'": "'", '0': '\0', 'r': '\r'}


def strip_comments(src):
    """Removes // and /* */ comments outside string literals."""
    out, i, n = [], 0, len(src)
    while i < n:
        c = src[i]
        if c == '"':
            j = i + 1
            while j < n and src[j] != '"':
                j += 2 if src[j] == '\\' else 1
            out.append(src[i:j + 1])
            i = j + 1
        elif src.startswith('R"', i):
            m = re.match(r'R"([^(]*)\(', src[i:])
            end = src.index(')' + m.group(1) + '"', i)
            out.append(src[i:end + len(m.group(1)) + 2])
            i = end + len(m.group(1)) + 2
        elif src.startswith('//', i):
            while i < n and src[i] != '\n':
                i += 1
        elif src.startswith('/*', i):
            i = src.index('*/', i) + 2
        else:
            out.append(c)
            i += 1
    return ''.join(out)


def literal(body):
    """Decodes the body of a C string literal."""
    out, i = [], 0
    while i < len(body):
        if body[i] == '\\':
            out.append(ESCAPES.get(body[i + 1], body[i + 1]))
            i += 2
        else:
            out.append(body[i])
            i += 1
    return ''.join(out)


TOKEN = re.compile(r'\s*(?:"((?:[^"\\]|\\.)*)"|([A-Za-z_][A-Za-z0-9_]*)|(.))', re.S)


def concat(expr, known):
    """Joins adjacent string literals and known macros/constants; None if anything else occurs."""
    parts = []
    for m in TOKEN.finditer(expr):
        if m.group(1) is not None:
            parts.append(literal(m.group(1)))
        elif m.group(2):
            if m.group(2) not in known or not isinstance(known[m.group(2)], str):
                return None
            parts.append(known[m.group(2)])
        elif m.group(3).strip():
            return None
    return ''.join(parts)


def split_top(text, sep):
    """Splits at sep outside string literals."""
    items, depth, cur, i = [], 0, [], 0
    while i < len(text):
        c = text[i]
        if c == '"':
            j = i + 1
            while text[j] != '"':
                j += 2 if text[j] == '\\' else 1
            cur.append(text[i:j + 1])
            i = j + 1
            continue
        if c == sep and depth == 0:
            items.append(''.join(cur))
            cur = []
        else:
            cur.append(c)
        i += 1
    items.append(''.join(cur))
    return items


def number(expr):
    expr = expr.strip().rstrip('fFuUlL')
    expr = re.sub(r'(\d)[uUlLfF]+\b', r'\1', expr)
    if expr in ('true', 'false'):
        return expr == 'true'
    if re.fullmatch(r'[\d\s\.\*\+\-/\(\)]+', expr):
        value = eval(expr, {'__builtins__': {}})  # digits and operators only
        return int(value) if float(value).is_integer() and '.' not in expr else value
    return None


def parse(path, known):
    src = strip_comments(open(path, encoding='utf-8').read())
    # #define NAME "..." (with line continuations)
    for m in re.finditer(r'#define\s+([A-Z_][A-Z0-9_]*)[ \t]+((?:[^\n]*\\\n)*[^\n]*)', src):
        value = concat(m.group(2).replace('\\\n', ' '), known)
        if value is None and m.group(2).strip():
            value = number(m.group(2))
        if value is not None:
            known[m.group(1)] = value
    decl = re.compile(r'constexpr\s+(?:const\s+)?[A-Za-z_0-9:]+\s*(?:const\s*)?\*?\s*([A-Z_][A-Z0-9_]*)\s*(\[\])?\s*=\s*', re.S)
    pos = 0
    while True:
        m = decl.search(src, pos)
        if not m:
            break
        name, is_array = m.group(1), bool(m.group(2))
        rest = src[m.end():]
        if is_array:
            end = rest.index('};')
            items = [concat(x, known) for x in split_top(rest[rest.index('{') + 1:end], ',') if x.strip()]
            if all(x is not None for x in items):
                known[name] = items
            pos = m.end() + end
            continue
        end = split_top(rest, ';')[0]
        value = concat(end, known)
        if value is None:
            value = number(end)
        if value is not None:
            known[name] = value
        pos = m.end() + len(end)
    return known


# The assistant's name stays a placeholder in the JSON; the Echo fills in its configured name.
NAME, PLACEHOLDER = 'ASSISTANT_NAME', '{{ASSISTANT_NAME}}'
PERSONA_KEYS = ('PERSONA_CORE', 'PERSONA_LEVELS', 'PERSONA_MAX_LEVEL', 'PERSONA_LIMITS', 'PERSONA_CHILD')


def write_persona(src, out):
    """The household's own persona (persona_local.h) as JSON for the Echo's state directory."""
    known = parse(src, {NAME: PLACEHOLDER})
    persona = {k: known[k] for k in PERSONA_KEYS if k in known}
    missing = [k for k in PERSONA_KEYS if k not in persona]
    if missing:
        sys.exit('%s: missing %s' % (src, ', '.join(missing)))
    os.makedirs(os.path.dirname(out), exist_ok=True)
    with open(out, 'w', encoding='utf-8', newline='\n') as f:
        json.dump(persona, f, ensure_ascii=False, indent=1, sort_keys=True)
        f.write('\n')
    print('persona -> %s' % out)


def main():
    if len(sys.argv) > 1 and sys.argv[1] == '--persona':
        # --persona [persona_local.h [persona.json]]
        src = sys.argv[2] if len(sys.argv) > 2 else os.path.join(LOCAL, 'persona_local.h')
        out = sys.argv[3] if len(sys.argv) > 3 else os.path.join(LOCAL, 'persona.json')
        write_persona(src, out)
        return
    known = {NAME: PLACEHOLDER}
    for path in (os.path.join(M5, 'config.h'), os.path.join(SHARED, 'persona.h'),
                 os.path.join(SHARED, 'prompts.h')):
        parse(path, known)
    known.pop(NAME, None)
    # Household values never go into this tracked file: drop every name secrets.h may define
    # (config.h has fallbacks for some of them). They come from the private config on the device.
    example = open(os.path.join(LOCAL, 'secrets.h.example'), encoding='utf-8').read()
    for secret in re.findall(r'#define\s+([A-Z_][A-Z0-9_]*)', example):
        known.pop(secret, None)
    for name in [k for k in known if re.search(r'(SECRET|TOKEN|PASSWORD|_KEY|EMAIL|OWNER|PHONE_NUMBER|HOST|REGISTRAR)', k)]:
        known.pop(name)
    os.makedirs(os.path.dirname(OUT), exist_ok=True)
    with open(OUT, 'w', encoding='utf-8', newline='\n') as f:
        json.dump(known, f, ensure_ascii=False, indent=1, sort_keys=True)
        f.write('\n')
    print('%d constants -> %s' % (len(known), OUT))
    for required in ('LIVE_INSTRUCTIONS', 'PERSONA_CORE', 'PERSONA_LEVELS', 'LIVE_MODEL', 'DELEGATION_INSTRUCTIONS'):
        if required not in known:
            sys.exit('missing ' + required)


if __name__ == "__main__":
    main()
