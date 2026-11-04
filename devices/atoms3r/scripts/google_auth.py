"""One-time Google OAuth consent for the assistant (calendar, contacts, Drive, Tasks, Gmail sorting).

Usage:
    python scripts/google_auth.py path/to/client_secret_XXXX.json
    python scripts/google_auth.py --gmail-only path/to/client_secret_XXXX.json
    python scripts/google_auth.py --agent OUTFILE [path/to/client_secret_XXXX.json]

Opens the browser for consent (loopback redirect on 127.0.0.1), exchanges the code
for a refresh token, checks it against the Calendar, People, Drive, Tasks and Gmail APIs and writes
GOOGLE_CLIENT_ID, GOOGLE_CLIENT_SECRET and GOOGLE_REFRESH_TOKEN into include/secrets.h.

--gmail-only: for another person's Gmail that the assistant watches (WATCHED_MAILBOXES). Asks only for
their mail (gmail.modify: read, labels, trash; it cannot delete for good), writes nothing and
prints the refresh token for their mailbox entry. Log in with their Google account (add it as a
test user of the consent screen first).
--agent: a separate token for the task agent on the server (docs/architecture.md, Task agent):
Gmail to read and sort (gmail.modify), calendar and contacts read only, plus drive.file, so it
sees exactly the files of the assistant's app.
Uses the assistant's client from secrets.h unless a client JSON is given; writes client id, secret and
refresh token as JSON to OUTFILE (nothing printed, secrets.h unchanged).
Standard library only.
"""

import base64
import hashlib
import http.server
import json
import os
import re
import secrets
import sys
import threading
import urllib.parse
import urllib.request
import webbrowser

SCOPES = [
    "https://www.googleapis.com/auth/calendar.events",
    "https://www.googleapis.com/auth/calendar.readonly",  # calendar list, free/busy
    "https://www.googleapis.com/auth/contacts",           # read + write contacts (21.7)
    "https://www.googleapis.com/auth/drive.file",         # own files only: backups, archive, sheets (14, 21.6)
    "https://www.googleapis.com/auth/tasks",              # to-do lists (21.3)
    "https://www.googleapis.com/auth/gmail.modify",       # watched mailbox: read, labels, trash; never delete (#37)
]
GMAIL_SCOPES = ["https://www.googleapis.com/auth/gmail.modify"]
AGENT_SCOPES = [
    "https://www.googleapis.com/auth/drive.file",         # the files of the assistant's app: exchange folder
    "https://www.googleapis.com/auth/gmail.modify",       # research and sorting (labels, archive); cannot send or delete for good
    "https://www.googleapis.com/auth/calendar.readonly",
    "https://www.googleapis.com/auth/contacts.readonly",
]
SECRETS_H = os.path.join(os.path.dirname(__file__), "..", "include", "secrets.h")


def load_client(path):
    with open(path, encoding="utf-8") as f:
        data = json.load(f)
    client = data.get("installed")
    if not client:
        sys.exit("Das ist kein Client vom Typ 'Desktop-App' (Schluessel 'installed' fehlt).")
    return client["client_id"], client["client_secret"]


def client_from_secrets():
    with open(os.path.normpath(SECRETS_H), encoding="utf-8") as f:
        text = f.read()
    values = {}
    for name in ("GOOGLE_CLIENT_ID", "GOOGLE_CLIENT_SECRET"):
        match = re.search(rf'^#define {name} "([^"]+)"', text, re.M)
        if not match:
            sys.exit(f"{name} fehlt in secrets.h: Client-JSON angeben.")
        values[name] = match.group(1)
    return values["GOOGLE_CLIENT_ID"], values["GOOGLE_CLIENT_SECRET"]


def wait_for_code(port_holder, state):
    result = {}

    class Handler(http.server.BaseHTTPRequestHandler):
        def do_GET(self):
            query = urllib.parse.parse_qs(urllib.parse.urlparse(self.path).query)
            if query.get("state", [""])[0] != state:
                self.send_response(400)
                self.end_headers()
                return
            result["code"] = query.get("code", [""])[0]
            result["error"] = query.get("error", [""])[0]
            self.send_response(200)
            self.send_header("Content-Type", "text/html; charset=utf-8")
            self.end_headers()
            text = "Fertig, du kannst das Fenster schliessen." if result["code"] else "Fehler: " + result["error"]
            self.wfile.write(f"<html><body><h2>{text}</h2></body></html>".encode())
            threading.Thread(target=self.server.shutdown).start()

        def log_message(self, *args):
            pass

    server = http.server.HTTPServer(("127.0.0.1", 0), Handler)
    port_holder.append(server.server_address[1])
    return server, result


def post_form(url, fields):
    body = urllib.parse.urlencode(fields).encode()
    try:
        with urllib.request.urlopen(urllib.request.Request(url, data=body)) as r:
            return json.load(r)
    except urllib.error.HTTPError as e:
        sys.exit(f"{url}: {e.code} {e.read().decode()}")


def get_json(url, token):
    req = urllib.request.Request(url, headers={"Authorization": "Bearer " + token})
    try:
        with urllib.request.urlopen(req) as r:
            return json.load(r)
    except urllib.error.HTTPError as e:
        return {"_error": f"{e.code} {e.read().decode()[:300]}"}


def write_secrets(values):
    path = os.path.normpath(SECRETS_H)
    with open(path, encoding="utf-8") as f:
        text = f.read()
    for name, value in values.items():
        line = f'#define {name} "{value}"'
        pattern = re.compile(rf"^#define {name} .*$", re.M)
        if pattern.search(text):
            text = pattern.sub(lambda _, line=line: line, text)
        else:
            text = text.rstrip("\n") + "\n" + line + "\n"
    with open(path, "w", encoding="utf-8") as f:
        f.write(text)
    print(f"In {path} eingetragen: {', '.join(values)}")


def main():
    args = sys.argv[1:]
    gmail_only = "--gmail-only" in args
    agent_out = None
    if "--agent" in args:
        i = args.index("--agent")
        if i + 1 >= len(args):
            sys.exit(__doc__)
        agent_out = args[i + 1]
        del args[i:i + 2]
    args = [a for a in args if a != "--gmail-only"]
    if len(args) != 1 and not (agent_out and not args):
        sys.exit(__doc__)
    scopes = AGENT_SCOPES if agent_out else GMAIL_SCOPES if gmail_only else SCOPES
    client_id, client_secret = load_client(args[0]) if args else client_from_secrets()

    state = secrets.token_urlsafe(16)
    verifier = secrets.token_urlsafe(64)
    challenge = base64.urlsafe_b64encode(hashlib.sha256(verifier.encode()).digest()).rstrip(b"=").decode()

    port = []
    server, result = wait_for_code(port, state)
    redirect = f"http://127.0.0.1:{port[0]}"
    url = "https://accounts.google.com/o/oauth2/v2/auth?" + urllib.parse.urlencode({
        "client_id": client_id,
        "redirect_uri": redirect,
        "response_type": "code",
        "scope": " ".join(scopes),
        "access_type": "offline",
        "prompt": "consent",
        "state": state,
        "code_challenge": challenge,
        "code_challenge_method": "S256",
    })
    print("Browser oeffnet sich. Falls nicht, diese Adresse oeffnen:\n" + url + "\n")
    webbrowser.open(url)
    server.serve_forever()
    if not result.get("code"):
        sys.exit("Keine Zustimmung: " + result.get("error", "?"))

    tokens = post_form("https://oauth2.googleapis.com/token", {
        "code": result["code"],
        "client_id": client_id,
        "client_secret": client_secret,
        "redirect_uri": redirect,
        "grant_type": "authorization_code",
        "code_verifier": verifier,
    })
    refresh = tokens.get("refresh_token")
    if not refresh:
        sys.exit("Kein Refresh-Token erhalten: " + json.dumps(tokens))
    granted = tokens.get("scope", "").split()
    missing = [s for s in scopes if s not in granted]
    if missing:
        print("WARNUNG: nicht freigegeben:", ", ".join(missing), "(Haekchen bei der Zustimmung gesetzt?)")

    access = tokens["access_token"]
    gmail = get_json("https://gmail.googleapis.com/gmail/v1/users/me/profile", access)
    print("Gmail:", gmail.get("_error") or f"ok ({gmail.get('messagesTotal', 0)} Mails)")
    if agent_out:
        files = get_json("https://www.googleapis.com/drive/v3/files?pageSize=10&fields=files(name)", access)
        print("Drive:", files.get("_error") or f"ok ({len(files.get('files', []))} Dateien von Jarvis' App)")
        fd = os.open(agent_out, os.O_WRONLY | os.O_CREAT | os.O_TRUNC, 0o600)
        with os.fdopen(fd, "w", encoding="utf-8") as f:
            json.dump({"client_id": client_id, "client_secret": client_secret, "refresh_token": refresh}, f)
        print(f"Token fuer den Agenten in {agent_out} (nur dort, nicht ins Repo).")
        return
    if gmail_only:
        print("\nRefresh-Token fuer den Eintrag in WATCHED_MAILBOXES (secret), nirgends sonst ablegen:\n" + refresh)
        return
    events = get_json("https://www.googleapis.com/calendar/v3/calendars/primary/events?maxResults=1", access)
    people = get_json("https://people.googleapis.com/v1/people/me/connections?personFields=names&pageSize=1", access)
    print("Kalender:", events.get("_error") or f"ok ({events.get('summary', 'primary')})")
    print("Kontakte:", people.get("_error") or f"ok ({people.get('totalPeople', 0)} Kontakte)")
    calendars = get_json("https://www.googleapis.com/calendar/v3/users/me/calendarList", access)
    print("Kalenderliste:", calendars.get("_error") or f"ok ({len(calendars.get('items', []))} Kalender)")
    files = get_json("https://www.googleapis.com/drive/v3/files?pageSize=10&fields=files(name)", access)
    print("Drive:", files.get("_error") or f"ok ({len(files.get('files', []))} eigene Dateien)")
    lists = get_json("https://tasks.googleapis.com/tasks/v1/users/@me/lists", access)
    print("Tasks:", lists.get("_error") or f"ok ({len(lists.get('items', []))} Listen)")

    write_secrets({
        "GOOGLE_CLIENT_ID": client_id,
        "GOOGLE_CLIENT_SECRET": client_secret,
        "GOOGLE_REFRESH_TOKEN": refresh,
    })


if __name__ == "__main__":
    main()
