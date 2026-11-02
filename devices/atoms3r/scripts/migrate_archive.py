"""Moves the assistant's memory archive from the mailbox to Google Drive.

Usage:
    python scripts/migrate_archive.py            # copy what is missing in Drive
    python scripts/migrate_archive.py --dry-run  # only show what it would copy

Reads every mail of the IMAP folders <name>-Themen (topic dossiers) and <name>-Gespraeche
(conversations) with MAIL_ADDRESS / MAIL_PASSWORD, and writes each as a text file into Drive:
<name>/Themen and <name>/Gespräche (<name>: ASSISTANT_NAME, "Jarvis" by default), named like the mail's subject, with the same appProperties
the device uses to find them again (topic = the key in "[...]", at = "YYYY-MM-DD HH:MM").
Files already in Drive are skipped, so it can run again. The mails stay in the mailbox.
Uses the device's Google login (GOOGLE_* in include/secrets.h, scope drive.file: the device sees
these files because they are made with the same OAuth client). Standard library only.
"""

import email
import email.header
import email.policy
import imaplib
import json
import os
import re
import sys
import urllib.error
import urllib.parse
import urllib.request

SECRETS_H = os.path.join(os.path.dirname(__file__), "..", "include", "secrets.h")
# mailbox folder after the name -> (Drive subfolder, appProperty)
FOLDERS = {"-Themen": ("Themen", "topic"), "-Gespraeche": ("Gespräche", "at")}
FOLDER_MIME = "application/vnd.google-apps.folder"
DRIVE = "https://www.googleapis.com/drive/v3/files"
UPLOAD = "https://www.googleapis.com/upload/drive/v3/files"
BOUNDARY = "assistant-part-boundary"


def secrets():
    text = open(SECRETS_H, encoding="utf-8").read()
    return lambda name: re.search(rf'^#define {name}\s+"([^"]*)"', text, re.M).group(1)


def assistant_name(define):
    """ASSISTANT_NAME from secrets.h, "Jarvis" when it is not set."""
    try:
        return define("ASSISTANT_NAME")
    except AttributeError:
        return "Jarvis"


def access_token(define):
    body = urllib.parse.urlencode({"client_id": define("GOOGLE_CLIENT_ID"),
                                   "client_secret": define("GOOGLE_CLIENT_SECRET"),
                                   "refresh_token": define("GOOGLE_REFRESH_TOKEN"),
                                   "grant_type": "refresh_token"}).encode()
    with urllib.request.urlopen(urllib.request.Request("https://oauth2.googleapis.com/token", data=body)) as r:
        return json.load(r)["access_token"]


class Drive:
    def __init__(self, token):
        self.token = token

    def call(self, url, data=None, content_type="application/json", method=None):
        headers = {"Authorization": "Bearer " + self.token}
        if data is not None:
            headers["Content-Type"] = content_type
        request = urllib.request.Request(url, data=data, headers=headers, method=method)
        try:
            with urllib.request.urlopen(request) as r:
                return json.load(r)
        except urllib.error.HTTPError as e:
            sys.exit(f"Drive: {e.code} {e.read().decode()[:300]}")

    def search(self, query):
        url = f"{DRIVE}?spaces=drive&fields=files(id)&pageSize=1&q=" + urllib.parse.quote(query)
        files = self.call(url)["files"]
        return files[0]["id"] if files else None

    def folder(self, name, parent=None):
        query = f"name={quoted(name)} and mimeType='{FOLDER_MIME}' and trashed=false"
        if parent:
            query += f" and '{parent}' in parents"
        found = self.search(query)
        if found:
            return found
        metadata = {"name": name, "mimeType": FOLDER_MIME}
        if parent:
            metadata["parents"] = [parent]
        print(f"folder {name} made")
        return self.call(f"{DRIVE}?fields=id", json.dumps(metadata).encode())["id"]

    def create(self, parent, name, text, properties):
        metadata = {"name": name, "mimeType": "text/plain", "parents": [parent], "appProperties": properties}
        body = (f"--{BOUNDARY}\r\nContent-Type: application/json; charset=UTF-8\r\n\r\n{json.dumps(metadata)}\r\n"
                f"--{BOUNDARY}\r\nContent-Type: text/plain\r\n\r\n").encode() + text.encode("utf-8") + \
               f"\r\n--{BOUNDARY}--\r\n".encode()
        self.call(f"{UPLOAD}?uploadType=multipart&fields=id", body, f"multipart/related; boundary={BOUNDARY}")


def quoted(text):
    return "'" + text.replace("\\", "\\\\").replace("'", "\\'") + "'"


def decoded(header):
    return str(email.header.make_header(email.header.decode_header(header or "")))


def body_text(message):
    part = message.get_body(("plain",)) if message.is_multipart() else message
    return part.get_content() if part else ""


def key_of(subject, property_name):
    """The same key the device takes from the subject (memory_archive.cpp noteKey)."""
    if property_name == "topic":
        match = re.search(r"\[([^\]]+)\]", subject)
        return match.group(1) if match else ""
    parts = subject.split(" ", 1)
    return parts[1][:16] if len(parts) > 1 else ""


def main():
    sys.stdout.reconfigure(encoding="utf-8")
    dry_run = "--dry-run" in sys.argv
    define = secrets()
    drive = Drive(access_token(define))
    name = assistant_name(define)
    root = drive.folder(name)
    imap = imaplib.IMAP4_SSL(define("MAIL_IMAP_HOST"))
    imap.login(define("MAIL_ADDRESS"), define("MAIL_PASSWORD"))
    totals = {}
    for suffix, (subfolder, property_name) in FOLDERS.items():
        mailbox = name + suffix
        parent = drive.folder(subfolder, root)
        status, _ = imap.select(f'"{mailbox}"', readonly=True)
        if status != "OK":
            print(f"{mailbox}: not in the mailbox, skipped")
            continue
        _, data = imap.uid("SEARCH", None, "ALL")
        uids = data[0].split()
        copied = skipped = 0
        seen = set()
        for uid in reversed(uids):  # newest first: of two mails with the same key the newer wins
            _, fetched = imap.uid("FETCH", uid, "(RFC822)")
            message = email.message_from_bytes(fetched[0][1], policy=email.policy.default)
            subject = decoded(message["Subject"])
            key = key_of(subject, property_name)
            # A dossier once per topic; conversations by their subject (two can start in the
            # same minute), as the device does.
            unique = key if property_name == "topic" else subject
            if not key or unique in seen:
                skipped += 1
                continue
            seen.add(unique)
            existing = (f"appProperties has {{ key='{property_name}' and value={quoted(key)} }}"
                        if property_name == "topic" else f"name={quoted(subject + '.txt')}")
            if drive.search(f"{existing} and '{parent}' in parents and trashed=false"):
                skipped += 1
                continue
            if not dry_run:
                drive.create(parent, subject + ".txt", body_text(message), {property_name: key})
            copied += 1
            print(f"{'would copy' if dry_run else 'copied'}: {subfolder}/{subject}")
        totals[mailbox] = (len(uids), copied, skipped)
    imap.logout()
    for mailbox, (count, copied, skipped) in totals.items():
        print(f"{mailbox}: {count} mails, {copied} {'to copy' if dry_run else 'copied'}, {skipped} skipped "
              "(already in Drive, or an older mail of the same topic)")


if __name__ == "__main__":
    main()
