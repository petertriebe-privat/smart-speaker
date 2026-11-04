"""One-time Spotify OAuth consent for the assistant (player, devices, playlists, library; issue #39).

Usage:
    python tools/spotify_auth.py            the admin's account: writes SPOTIFY_REFRESH_TOKEN
    python tools/spotify_auth.py --other    another person's account: prints the refresh token
                                              for their SPOTIFY_ACCOUNTS entry, writes nothing

Reads SPOTIFY_CLIENT_ID and SPOTIFY_CLIENT_SECRET from local/secrets.h, opens the browser for
consent (redirect http://127.0.0.1:8888/callback, registered in the app's settings), exchanges
the code for a refresh token and checks it against the devices and playlists endpoints.
For --other, log in with that person's Spotify account (a user of the app under User
Management; a private window helps). Standard library only.
"""

import base64
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
    "user-read-playback-state",     # devices, what is playing
    "user-modify-playback-state",   # play, pause, volume, transfer, queue
    "user-read-currently-playing",
    "playlist-read-private",        # own playlists by name
    "playlist-read-collaborative",
    "playlist-modify-private",      # create playlists, add and remove tracks
    "playlist-modify-public",
    "user-library-read",            # liked songs
    "user-library-modify",
    "user-read-recently-played",
    "user-top-read",                # "play something I like"
    "user-follow-read",             # followed artists
    "user-follow-modify",
    "ugc-image-upload",             # playlist covers
]
PORT = 8888
REDIRECT = f"http://127.0.0.1:{PORT}/callback"
SECRETS_H = os.path.join(os.path.dirname(__file__), "..", "local", "secrets.h")


def read_secret(text, name):
    match = re.search(rf'^#define {name} "([^"]*)"', text, re.M)
    return match.group(1) if match else ""


def wait_for_code(state):
    result = {}

    class Handler(http.server.BaseHTTPRequestHandler):
        def do_GET(self):
            url = urllib.parse.urlparse(self.path)
            query = urllib.parse.parse_qs(url.query)
            if url.path != "/callback" or query.get("state", [""])[0] != state:
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

    return http.server.HTTPServer(("127.0.0.1", PORT), Handler), result


def get_json(url, token):
    req = urllib.request.Request(url, headers={"Authorization": "Bearer " + token})
    try:
        with urllib.request.urlopen(req) as r:
            return json.load(r)
    except urllib.error.HTTPError as e:
        return {"_error": f"{e.code} {e.read().decode()[:300]}"}


def write_secret(path, text, name, value):
    line = f'#define {name} "{value}"'
    pattern = re.compile(rf"^#define {name} .*$", re.M)
    if pattern.search(text):
        text = pattern.sub(lambda _: line, text)
    else:
        text = text.rstrip("\n") + "\n" + line + "\n"
    with open(path, "w", encoding="utf-8") as f:
        f.write(text)
    print(f"In {path} eingetragen: {name}")


def main():
    args = sys.argv[1:]
    other = "--other" in args
    if [a for a in args if a != "--other"]:
        sys.exit(__doc__)
    path = os.path.normpath(SECRETS_H)
    with open(path, encoding="utf-8") as f:
        text = f.read()
    client_id, client_secret = read_secret(text, "SPOTIFY_CLIENT_ID"), read_secret(text, "SPOTIFY_CLIENT_SECRET")
    if not client_id or not client_secret:
        sys.exit("SPOTIFY_CLIENT_ID und SPOTIFY_CLIENT_SECRET zuerst in local/secrets.h eintragen.")

    state = secrets.token_urlsafe(16)
    server, result = wait_for_code(state)
    url = "https://accounts.spotify.com/authorize?" + urllib.parse.urlencode({
        "client_id": client_id,
        "response_type": "code",
        "redirect_uri": REDIRECT,
        "scope": " ".join(SCOPES),
        "state": state,
        "show_dialog": "true",  # always ask, so the right account can be chosen
    })
    print("Browser oeffnet sich. Falls nicht, diese Adresse oeffnen:\n" + url + "\n", flush=True)
    webbrowser.open(url)
    server.serve_forever()
    if not result.get("code"):
        sys.exit("Keine Zustimmung: " + result.get("error", "?"))

    basic = base64.b64encode(f"{client_id}:{client_secret}".encode()).decode()
    body = urllib.parse.urlencode({
        "grant_type": "authorization_code",
        "code": result["code"],
        "redirect_uri": REDIRECT,
    }).encode()
    req = urllib.request.Request("https://accounts.spotify.com/api/token", data=body,
                                 headers={"Authorization": "Basic " + basic})
    try:
        with urllib.request.urlopen(req) as r:
            tokens = json.load(r)
    except urllib.error.HTTPError as e:
        sys.exit(f"Token: {e.code} {e.read().decode()}")
    refresh = tokens.get("refresh_token")
    if not refresh:
        sys.exit("Kein Refresh-Token erhalten: " + json.dumps(tokens))
    granted = tokens.get("scope", "").split()
    missing = [s for s in SCOPES if s not in granted]
    if missing:
        print("WARNUNG: nicht freigegeben:", ", ".join(missing))

    access = tokens["access_token"]
    devices = get_json("https://api.spotify.com/v1/me/player/devices", access)
    if "_error" in devices:
        print("Geraete:", devices["_error"])
    else:
        names = [f"{d.get('name')} ({d.get('type')})" for d in devices.get("devices", [])]
        print("Geraete:", ", ".join(names) if names else "keine gerade sichtbar (normal, wenn nichts laeuft)")
    playlists = get_json("https://api.spotify.com/v1/me/playlists?limit=1", access)
    print("Playlists:", playlists.get("_error") or f"ok ({playlists.get('total', 0)})")

    if other:
        print("\nRefresh-Token fuer den Eintrag in SPOTIFY_ACCOUNTS, nirgends sonst ablegen:\n" + refresh)
        return
    write_secret(path, text, "SPOTIFY_REFRESH_TOKEN", refresh)


if __name__ == "__main__":
    main()
