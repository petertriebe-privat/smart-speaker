"""MCP server for a Google account: Gmail (read, labels, archive; never trash), one Drive folder,
calendars and contacts (read).

GOOGLE_CREDENTIALS: path of a JSON {client_id, client_secret, refresh_token}, e.g. from
scripts/google_auth.py --agent (scopes gmail.modify, drive.file, calendar.readonly,
contacts.readonly; with gmail.readonly only the label tools fail). Drive access is limited to GOOGLE_DRIVE_FOLDER ("Parent/Child", the first
part found anywhere); files written get appProperties.source = GOOGLE_DRIVE_SOURCE.
"""

import base64
import datetime as dt
import html
import json
import mimetypes
import os
import time
import urllib.error
import urllib.parse
import urllib.request
from email import policy
from email.parser import BytesParser

from mcp.server.fastmcp import FastMCP

from common import attachment_result, attachments, blocking, body_text, header_text, tidy

CREDENTIALS = os.environ.get("GOOGLE_CREDENTIALS", "/run/secrets/google.json")
FOLDER = [p for p in os.environ.get("GOOGLE_DRIVE_FOLDER", "").split("/") if p]
SOURCE = os.environ.get("GOOGLE_DRIVE_SOURCE", "MCP")
GMAIL = "https://gmail.googleapis.com/gmail/v1/users/me"
DRIVE = "https://www.googleapis.com/drive/v3/files"
FOLDER_MIME = "application/vnd.google-apps.folder"

mcp = FastMCP("google", stateless_http=True, json_response=True)
_token = {"value": "", "until": 0.0}


def token() -> str:
    if time.time() < _token["until"]:
        return _token["value"]
    with open(CREDENTIALS, encoding="utf-8") as f:
        c = json.load(f)
    body = urllib.parse.urlencode({"client_id": c["client_id"], "client_secret": c["client_secret"],
                                   "refresh_token": c["refresh_token"], "grant_type": "refresh_token"}).encode()
    with urllib.request.urlopen("https://oauth2.googleapis.com/token", body, timeout=30) as r:
        answer = json.load(r)
    _token.update(value=answer["access_token"], until=time.time() + answer.get("expires_in", 3600) - 120)
    return _token["value"]


def call(method, url, params=None, body=None, headers=None, raw=False):
    if params:
        url += "?" + urllib.parse.urlencode(params, doseq=True)
    h = {"Authorization": "Bearer " + token()}
    h.update(headers or {})
    if isinstance(body, (dict, list)):
        body = json.dumps(body).encode()
        h["Content-Type"] = "application/json"
    req = urllib.request.Request(url, data=body, headers=h, method=method)
    try:
        with urllib.request.urlopen(req, timeout=60) as r:
            data = r.read()
    except urllib.error.HTTPError as e:
        raise RuntimeError(f"Google API {e.code}: {e.read().decode(errors='replace')[:400]}") from None
    return data if raw else json.loads(data or b"{}")


def b64(data: str) -> bytes:
    return base64.urlsafe_b64decode(data + "=" * (-len(data) % 4))


# --- Gmail -------------------------------------------------------------------------------------

def header(msg, name):
    for h in msg.get("payload", {}).get("headers", []):
        if h["name"].lower() == name.lower():
            return h["value"]
    return ""


def walk(part):
    yield part
    for p in part.get("parts", []) or []:
        yield from walk(p)


def message_ids(query, limit):
    ids, page = [], None
    while len(ids) < limit:
        params = {"q": query, "maxResults": min(500, limit - len(ids))}
        if page:
            params["pageToken"] = page
        answer = call("GET", f"{GMAIL}/messages", params)
        ids += [m["id"] for m in answer.get("messages", [])]
        page = answer.get("nextPageToken")
        if not page:
            break
    return ids


def gmail_search_sync(query, limit, ids_only=False):
    if ids_only:
        ids = message_ids(query, limit)
        return f"{len(ids)} ids: " + ",".join(ids) if ids else "No mails."
    ids = call("GET", f"{GMAIL}/messages", {"q": query, "maxResults": limit}).get("messages", [])
    lines = []
    for m in ids:
        msg = call("GET", f"{GMAIL}/messages/{m['id']}",
                   {"format": "metadata", "metadataHeaders": ["From", "Subject", "Date"]})
        unread = " | unread" if "UNREAD" in msg.get("labelIds", []) else ""
        lines.append(f"{m['id']} | {header(msg, 'Date')} | {header(msg, 'From')} | {header(msg, 'Subject')}{unread}\n"
                     f"    {tidy(html.unescape(msg.get('snippet', '')))}")
    return "\n".join(lines) or "No mails."


def gmail_read_sync(mid):
    raw = call("GET", f"{GMAIL}/messages/{mid}", {"format": "raw"})["raw"]
    msg = BytesParser(policy=policy.default).parsebytes(b64(raw))
    head = [f"{h}: {header_text(msg[h])}" for h in ("Date", "From", "To", "Cc", "Subject") if msg[h]]
    files = attachments(msg)
    tail = ("\n\nAttachments:\n" + "\n".join(f"- {n} ({m}, {s} bytes)" for n, m, s in files)) if files else ""
    return "\n".join(head) + "\n\n" + body_text(msg) + tail


def gmail_attachment_sync(mid, filename):
    msg = call("GET", f"{GMAIL}/messages/{mid}", {"format": "full"})
    for p in walk(msg["payload"]):
        if p.get("filename") == filename:
            body = p["body"]
            data = body.get("data") or call("GET", f"{GMAIL}/messages/{mid}/attachments/{body['attachmentId']}")["data"]
            return attachment_result(filename, p.get("mimeType", "application/octet-stream"), b64(data))
    return {"error": f"no attachment '{filename}'"}


FORBIDDEN_LABELS = {"TRASH", "SPAM"}  # nothing goes to the bin or spam through here


def labels():
    return call("GET", f"{GMAIL}/labels").get("labels", [])


def gmail_labels_sync():
    rows = []
    for label in sorted(labels(), key=lambda l: (l.get("type") != "user", l["name"])):
        info = call("GET", f"{GMAIL}/labels/{label['id']}") if label.get("type") == "user" else {}
        count = f" | {info.get('messagesTotal', '?')} mails" if info else ""
        rows.append(f"{label['name']} | {label.get('type')}{count}")
    return "\n".join(rows)


def gmail_create_label_sync(name):
    for label in labels():
        if label["name"].lower() == name.lower():
            return f"Label {label['name']} exists already."
    call("POST", f"{GMAIL}/labels", body={"name": name, "labelListVisibility": "labelShow",
                                          "messageListVisibility": "show"})
    return f"Created label {name}."


def gmail_modify_sync(ids, add, remove):
    by_name = {l["name"].lower(): l["id"] for l in labels()}
    def resolve(names):
        out = []
        for n in names:
            if n.upper() in FORBIDDEN_LABELS:
                raise ValueError(f"label {n} is not allowed (nothing is trashed or marked as spam)")
            if n.lower() not in by_name:
                raise ValueError(f"no label '{n}' (gmail_create_label first)")
            out.append(by_name[n.lower()])
        return out
    add_ids, remove_ids = resolve(add), resolve(remove)
    for i in range(0, len(ids), 1000):
        call("POST", f"{GMAIL}/messages/batchModify",
             body={"ids": ids[i:i + 1000], "addLabelIds": add_ids, "removeLabelIds": remove_ids})
    return f"Changed {len(ids)} mails: +{', '.join(add) or '-'}, -{', '.join(remove) or '-'}."


# --- Drive (one folder) ------------------------------------------------------------------------

def quoted(text):
    return "'" + text.replace("\\", "\\\\").replace("'", "\\'") + "'"


def folder_id():
    if not FOLDER:
        raise RuntimeError("GOOGLE_DRIVE_FOLDER is not set: no Drive access")
    parent = None
    for name in FOLDER:
        q = f"name={quoted(name)} and mimeType='{FOLDER_MIME}' and trashed=false"
        if parent:
            q += f" and {quoted(parent)} in parents"
        found = call("GET", DRIVE, {"q": q, "fields": "files(id)"}).get("files", [])
        if not found:
            raise RuntimeError(f"Drive folder {'/'.join(FOLDER)} not found")
        parent = found[0]["id"]
    return parent


def find(parent, name):
    q = f"name={quoted(name)} and {quoted(parent)} in parents and trashed=false"
    found = call("GET", DRIVE, {"q": q, "fields": "files(id,mimeType)"}).get("files", [])
    return found[0] if found else None


def drive_list_sync():
    q = f"{quoted(folder_id())} in parents and trashed=false"
    files = call("GET", DRIVE, {"q": q, "orderBy": "modifiedTime desc", "pageSize": 100,
                               "fields": "files(name,mimeType,size,modifiedTime,appProperties)"}).get("files", [])
    return "\n".join(f"{f['modifiedTime'][:16]} | {f['name']} | {f.get('size', '-')} bytes | {f['mimeType']}"
                     f" | {(f.get('appProperties') or {}).get('source', '')}" for f in files) or "No files."


def drive_put_sync(name, data: bytes, mime):
    parent = folder_id()
    mime = mime or mimetypes.guess_type(name)[0] or "application/octet-stream"
    meta = {"name": name, "appProperties": {"source": SOURCE}}
    existing = find(parent, name)
    if not existing:
        meta["parents"] = [parent]
    boundary = "mcp-boundary-7f3a"
    body = (f"--{boundary}\r\nContent-Type: application/json; charset=UTF-8\r\n\r\n{json.dumps(meta)}\r\n"
            f"--{boundary}\r\nContent-Type: {mime}\r\n\r\n").encode() + data + f"\r\n--{boundary}--".encode()
    url = "https://www.googleapis.com/upload/drive/v3/files" + (f"/{existing['id']}" if existing else "")
    call("PATCH" if existing else "POST", url, {"uploadType": "multipart"}, body,
         {"Content-Type": f"multipart/related; boundary={boundary}"})
    return f"{'Updated' if existing else 'Created'} {'/'.join(FOLDER)}/{name} ({len(data)} bytes)."


def drive_get_sync(name):
    f = find(folder_id(), name)
    if not f:
        return {"error": f"no file '{name}'"}
    if f["mimeType"].startswith("application/vnd.google-apps."):  # Docs, Sheets: export
        kind = {"spreadsheet": "text/csv", "document": "text/plain"}.get(f["mimeType"].rsplit(".", 1)[-1], "application/pdf")
        data = call("GET", f"{DRIVE}/{f['id']}/export", {"mimeType": kind}, raw=True)
        return attachment_result(name, kind, data)
    data = call("GET", f"{DRIVE}/{f['id']}", {"alt": "media"}, raw=True)
    return attachment_result(name, f["mimeType"], data)


# --- Calendar and contacts ---------------------------------------------------------------------

def calendar_sync(days, past, query):
    now = dt.datetime.now(dt.timezone.utc)
    params = {"timeMin": (now - dt.timedelta(days=past)).isoformat(), "timeMax": (now + dt.timedelta(days=days)).isoformat(),
              "singleEvents": "true", "orderBy": "startTime", "maxResults": 250}
    if query:
        params["q"] = query
    rows = []
    for c in call("GET", "https://www.googleapis.com/calendar/v3/users/me/calendarList").get("items", []):
        url = f"https://www.googleapis.com/calendar/v3/calendars/{urllib.parse.quote(c['id'])}/events"
        for e in call("GET", url, params).get("items", []):
            start = e.get("start", {}).get("dateTime") or e.get("start", {}).get("date", "")
            where = f" | {e['location']}" if e.get("location") else ""
            rows.append(f"{start[:16]} | {c.get('summary', '')} | {e.get('summary', '(no title)')}{where}")
    return "\n".join(sorted(rows)) or "No events."


def contacts_sync(query):
    found = call("GET", "https://people.googleapis.com/v1/people:searchContacts",
                 {"query": query, "readMask": "names,emailAddresses,phoneNumbers,addresses,birthdays,organizations"}
                 ).get("results", [])
    lines = []
    for r in found:
        p = r["person"]
        parts = [", ".join(n.get("displayName", "") for n in p.get("names", []))]
        parts += [o.get("name", "") for o in p.get("organizations", [])]
        parts += [e["value"] for e in p.get("emailAddresses", [])] + [t["value"] for t in p.get("phoneNumbers", [])]
        parts += [a.get("formattedValue", "").replace("\n", ", ") for a in p.get("addresses", [])]
        for b in p.get("birthdays", []):
            d = b.get("date", {})
            parts.append("born " + "-".join(str(d[k]) for k in ("year", "month", "day") if k in d))
        lines.append(" | ".join(x for x in parts if x))
    return "\n".join(lines) or "No contacts."


# --- Tools -------------------------------------------------------------------------------------

@mcp.tool()
async def gmail_search(query: str, limit: int = 20, ids_only: bool = False) -> str:
    """Searches Gmail with Gmail's query syntax (from:, subject:, has:attachment, newer_than:30d,
    after:2026/01/01, label:, in:inbox, category:promotions ...). Newest first: id | date | from |
    subject, preview (up to 100). ids_only: just the ids, up to 5000, e.g. for gmail_modify."""
    top = 5000 if ids_only else 100
    return await blocking(gmail_search_sync, query, max(1, min(limit, top)), ids_only)


@mcp.tool()
async def gmail_labels() -> str:
    """All labels (system and own) with the number of mails of own labels."""
    return await blocking(gmail_labels_sync)


@mcp.tool()
async def gmail_create_label(name: str) -> str:
    """Creates a label ("Parent/Child" nests it). Needs the gmail.modify scope."""
    return await blocking(gmail_create_label_sync, name.strip())


@mcp.tool()
async def gmail_modify(message_ids: list[str], add_labels: list[str] | None = None,
                       remove_labels: list[str] | None = None) -> str:
    """Adds and removes labels on any number of mails at once, by label name. Archive = remove
    INBOX; read = remove UNREAD. TRASH and SPAM are refused: nothing is deleted. Needs gmail.modify."""
    if not message_ids:
        return "Error: no message ids."
    return await blocking(gmail_modify_sync, list(dict.fromkeys(message_ids)), add_labels or [], remove_labels or [])


@mcp.tool()
async def gmail_read(message_id: str) -> str:
    """One Gmail message: headers, text (HTML converted), attachments."""
    return await blocking(gmail_read_sync, message_id)


@mcp.tool()
async def gmail_attachment(message_id: str, filename: str) -> dict:
    """One attachment: text files as text, others as base64 (max 8 MB)."""
    return await blocking(gmail_attachment_sync, message_id, filename)


@mcp.tool()
async def drive_list() -> str:
    """Files in the shared Drive folder, newest first: modified | name | size | type | source."""
    return await blocking(drive_list_sync)


@mcp.tool()
async def drive_put(name: str, text: str = "", base64_data: str = "", mime_type: str = "") -> str:
    """Writes a file into the shared Drive folder (same name: a new revision). Give `text` for text
    files or `base64_data` for binary ones (PDF, images, xlsx)."""
    data = base64.b64decode(base64_data) if base64_data else text.encode("utf-8")
    return await blocking(drive_put_sync, name, data, mime_type)


@mcp.tool()
async def drive_get(name: str) -> dict:
    """Reads a file of the shared Drive folder: text as text, binary as base64; Google Docs and
    Sheets exported as text/CSV."""
    return await blocking(drive_get_sync, name)


@mcp.tool()
async def calendar_events(days_ahead: int = 14, days_back: int = 0, query: str = "") -> str:
    """Events of all calendars in a time window around now, optionally filtered by a text query."""
    return await blocking(calendar_sync, max(0, days_ahead), max(0, days_back), query)


@mcp.tool()
async def contacts_search(query: str) -> str:
    """Finds contacts by name, e-mail, phone or company: name | company | mails | phones | addresses."""
    return await blocking(contacts_sync, query)
