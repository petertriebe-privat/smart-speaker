"""MCP server for IMAP mailboxes (any provider): folders, search, read, attachments.

Accounts as JSON {"name": {"host": "...", "user": "...", "password": "...", "port": 993,
"allow_move": false}} in the file IMAP_ACCOUNTS_FILE (default /run/secrets/imap.json; a file, as
env files mangle "$" and " #" in passwords) or in IMAP_ACCOUNTS. Read only unless an account sets
allow_move (then imap_move moves mails into other folders and imap_create_folder makes folders;
nothing is ever deleted). Reading never marks a mail as seen (BODY.PEEK).
"""

import base64
import email
import imaplib
import json
import os
import re
from datetime import date
from email import policy

from mcp.server.fastmcp import FastMCP

from common import attachment_result, attachments, blocking, body_text, header_text

def load_accounts() -> dict:
    path = os.environ.get("IMAP_ACCOUNTS_FILE", "/run/secrets/imap.json")
    if os.path.exists(path):
        with open(path, encoding="utf-8") as f:
            return json.load(f)
    return json.loads(os.environ.get("IMAP_ACCOUNTS", "{}") or "{}")


ACCOUNTS: dict = load_accounts()
LIST_RE = re.compile(r'\((?P<flags>[^)]*)\) (?P<sep>"[^"]*"|NIL) (?P<name>.+)')

mcp = FastMCP("imap", stateless_http=True, json_response=True)


def account(name: str) -> dict:
    if not name and len(ACCOUNTS) == 1:
        return next(iter(ACCOUNTS.values()))
    if name not in ACCOUNTS:
        raise ValueError(f"unknown account '{name}', configured: {', '.join(ACCOUNTS) or 'none'}")
    return ACCOUNTS[name]


def connect(acc: dict) -> imaplib.IMAP4_SSL:
    conn = imaplib.IMAP4_SSL(acc["host"], int(acc.get("port", 993)), timeout=60)
    conn.login(acc["user"], acc["password"])
    return conn


def mutf7_decode(name: str) -> str:
    """IMAP's modified UTF-7 folder names ("Entw&APw-rfe") as text ("Entwürfe")."""
    def part(m):
        chunk = m.group(1)
        if not chunk:
            return "&"
        data = chunk.replace(",", "/")
        return base64.b64decode(data + "=" * (-len(data) % 4)).decode("utf-16-be")
    return re.sub(r"&([^-]*)-", part, name)


def mutf7_encode(name: str) -> str:
    out, run = [], []

    def flush():
        if run:
            data = base64.b64encode("".join(run).encode("utf-16-be")).decode().rstrip("=")
            out.append("&" + data.replace("/", ",") + "-")
            run.clear()
    for ch in name:
        if 0x20 <= ord(ch) <= 0x7E:
            flush()
            out.append("&-" if ch == "&" else ch)
        else:
            run.append(ch)
    flush()
    return "".join(out)


def quote(folder: str) -> str:
    folder = mutf7_encode(folder)
    return '"' + folder.replace("\\", "\\\\").replace('"', '\\"') + '"'


def imap_date(text: str) -> str:
    d = date.fromisoformat(text)
    return f"{d.day}-{['Jan','Feb','Mar','Apr','May','Jun','Jul','Aug','Sep','Oct','Nov','Dec'][d.month - 1]}-{d.year}"


def check(typ, data, what):
    if typ != "OK":
        raise RuntimeError(f"{what} failed: {data}")
    return data


# --- Blocking implementations ------------------------------------------------------------------

def folders_sync(acc: dict) -> str:
    conn = connect(acc)
    try:
        lines = []
        for raw in check(*conn.list(), "LIST"):
            m = LIST_RE.match(raw.decode(errors="replace"))
            if not m:
                continue
            name = mutf7_decode(m.group("name").strip('"'))
            typ, data = conn.status(quote(name), "(MESSAGES UNSEEN)")
            counts = data[0].decode(errors="replace").split("(", 1)[-1].rstrip(")") if typ == "OK" else ""
            lines.append(f"{name}  {counts}  {m.group('flags')}".rstrip())
        return "\n".join(lines)
    finally:
        conn.logout()


def search_sync(acc, folder, sender, subject, text, since, before, unseen, limit, uids_only=False) -> str:
    conn = connect(acc)
    try:
        check(*conn.select(quote(folder), readonly=True), f"SELECT {folder}")
        criteria, literal = [], None
        for key, value in (("FROM", sender), ("SUBJECT", subject), ("TEXT", text)):
            if not value:
                continue
            if value.isascii():
                criteria += [key, '"' + value.replace('"', "") + '"']
            elif literal is None:  # imaplib sends one literal, at the end of the command
                literal = (key, value.encode())
            else:
                raise ValueError("at most one search term with non-ASCII characters")
        if since:
            criteria += ["SINCE", imap_date(since)]
        if before:
            criteria += ["BEFORE", imap_date(before)]
        if unseen:
            criteria.append("UNSEEN")
        if literal:
            criteria.append(literal[0])
            conn.literal = literal[1]
            typ, data = conn.uid("SEARCH", "CHARSET", "UTF-8", *criteria)
        else:
            typ, data = conn.uid("SEARCH", *(criteria or ["ALL"]))
        uids = check(typ, data, "SEARCH")[0].split()
        total, uids = len(uids), uids[-limit:][::-1]  # newest first
        if not uids:
            return "No mails."
        if uids_only:
            more = f" (newest {len(uids)} of {total})" if total > len(uids) else ""
            return f"{len(uids)} uids{more}: " + ",".join(u.decode() for u in uids)
        typ, data = conn.uid("FETCH", b",".join(uids).decode(),
                             "(UID FLAGS RFC822.SIZE BODY.PEEK[HEADER.FIELDS (FROM SUBJECT DATE)])")
        rows = {}
        for item in check(typ, data, "FETCH"):
            if not isinstance(item, tuple):
                continue
            meta = item[0].decode(errors="replace")
            uid = re.search(r"UID (\d+)", meta).group(1)
            size = re.search(r"RFC822\.SIZE (\d+)", meta)
            seen = "\\Seen" in meta
            msg = email.message_from_bytes(item[1], policy=policy.default)
            rows[uid] = (f"{uid} | {header_text(msg['Date'])} | {header_text(msg['From'])} | "
                         f"{header_text(msg['Subject'])} | {size.group(1) if size else '?'} bytes"
                         f"{'' if seen else ' | unread'}")
        lines = [rows[u.decode()] for u in uids if u.decode() in rows]
        more = f"\n({total} found, newest {len(lines)} shown)" if total > len(lines) else ""
        return "\n".join(lines) + more
    finally:
        conn.logout()


def fetch(acc, folder, uid):
    conn = connect(acc)
    try:
        check(*conn.select(quote(folder), readonly=True), f"SELECT {folder}")
        typ, data = conn.uid("FETCH", str(uid), "(BODY.PEEK[])")
        parts = [d for d in check(typ, data, "FETCH") if isinstance(d, tuple)]
        if not parts:
            raise ValueError(f"no mail with uid {uid} in {folder}")
        return email.message_from_bytes(parts[0][1], policy=policy.default)
    finally:
        conn.logout()


def read_sync(acc, folder, uid) -> str:
    msg = fetch(acc, folder, uid)
    head = [f"{h}: {header_text(msg[h])}" for h in ("Date", "From", "To", "Cc", "Reply-To", "Subject") if msg[h]]
    files = attachments(msg)
    tail = ("\n\nAttachments:\n" + "\n".join(f"- {n} ({m}, {s} bytes)" for n, m, s in files)) if files else ""
    return "\n".join(head) + "\n\n" + body_text(msg) + tail


def attachment_sync(acc, folder, uid, filename) -> dict:
    msg = fetch(acc, folder, uid)
    for part in msg.iter_attachments():
        if header_text(part.get_filename() or "") == filename:
            return attachment_result(filename, part.get_content_type(), part.get_payload(decode=True) or b"")
    return {"error": f"no attachment '{filename}'"}


def move_sync(acc, folder, uids, target) -> str:
    conn = connect(acc)
    try:
        check(*conn.select(quote(folder)), f"SELECT {folder}")
        moved = 0
        for i in range(0, len(uids), 500):  # one command per 500 mails
            chunk = ",".join(str(u) for u in uids[i:i + 500])
            typ, data = conn.uid("MOVE", chunk, quote(target))
            if typ != "OK":  # servers without MOVE: copy, flag, expunge just these uids
                check(*conn.uid("COPY", chunk, quote(target)), f"COPY to {target}")
                check(*conn.uid("STORE", chunk, "+FLAGS", "(\\Deleted)"), "STORE")
                conn.uid("EXPUNGE", chunk)
            moved += len(uids[i:i + 500])
        return f"Moved {moved} mails from {folder} to {target}."
    finally:
        conn.logout()


def create_sync(acc, name) -> str:
    conn = connect(acc)
    try:
        typ, data = conn.list()
        names = []
        for raw in data if typ == "OK" else []:
            m = LIST_RE.match(raw.decode(errors="replace"))
            if m:
                names.append(mutf7_decode(m.group("name").strip('"')))
        if name in names:
            return f"Folder {name} exists already."
        check(*conn.create(quote(name)), f"CREATE {name}")
        conn.subscribe(quote(name))
        sep = "." if any("." in n for n in names) else "/"
        hint = f" (subfolders are separated by '{sep}', e.g. INBOX{sep}Name)"
        return f"Created folder {name}." + ("" if sep in name else hint)
    finally:
        conn.logout()


# --- Tools -------------------------------------------------------------------------------------

@mcp.tool()
async def imap_accounts() -> str:
    """The configured mailboxes (names to pass as `account`) and whether mails may be moved."""
    return "\n".join(f"{n}: {a['user']} on {a['host']}{' (move allowed)' if a.get('allow_move') else ' (read only)'}"
                     for n, a in ACCOUNTS.items()) or "No accounts configured."


@mcp.tool()
async def imap_folders(account_name: str = "") -> str:
    """Folders of a mailbox with message and unread counts."""
    return await blocking(folders_sync, account(account_name))


@mcp.tool()
async def imap_search(account_name: str = "", folder: str = "INBOX", sender: str = "", subject: str = "",
                      text: str = "", since: str = "", before: str = "", unseen: bool = False,
                      limit: int = 30, uids_only: bool = False) -> str:
    """Searches a folder; all given criteria must match. sender/subject/text: substrings (text
    searches headers and body). since/before: dates YYYY-MM-DD. Newest first: uid | date | from |
    subject | size (up to 200). uids_only: just the uids, up to 5000, e.g. to move all mails of a
    sender at once."""
    top = 5000 if uids_only else 200
    return await blocking(search_sync, account(account_name), folder, sender, subject, text, since, before,
                          unseen, max(1, min(limit, top)), uids_only)


@mcp.tool()
async def imap_read(uid: int, folder: str = "INBOX", account_name: str = "") -> str:
    """One mail: headers, text (HTML converted), list of attachments. Does not mark it as read."""
    return await blocking(read_sync, account(account_name), folder, uid)


@mcp.tool()
async def imap_attachment(uid: int, filename: str, folder: str = "INBOX", account_name: str = "") -> dict:
    """One attachment of a mail: text files as text, others as base64 (max 8 MB)."""
    return await blocking(attachment_sync, account(account_name), folder, uid, filename)


@mcp.tool()
async def imap_move(uids: list[int], target_folder: str, folder: str = "INBOX", account_name: str = "") -> str:
    """Moves mails (any number of uids at once) into another existing folder. Only for accounts
    with allow_move; never deletes; the mails keep their read state."""
    acc = account(account_name)
    if not acc.get("allow_move"):
        return "Error: this account is read only."
    if not uids:
        return "Error: no uids."
    return await blocking(move_sync, acc, folder, list(dict.fromkeys(uids)), target_folder)


@mcp.tool()
async def imap_create_folder(name: str, account_name: str = "") -> str:
    """Creates a folder (e.g. INBOX.Newsletter; see imap_folders for the separator). Only for
    accounts with allow_move. Existing folders are left alone."""
    acc = account(account_name)
    if not acc.get("allow_move"):
        return "Error: this account is read only."
    return await blocking(create_sync, acc, name.strip())
