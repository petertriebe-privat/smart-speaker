"""Helpers shared by the MCP servers: blocking calls in a thread, mail text, sizes."""

import base64
import html
import re
from email.header import decode_header, make_header
from email.message import EmailMessage

import anyio

MAX_TEXT = 60_000          # characters of a mail body returned at most
MAX_ATTACHMENT = 8 << 20   # bytes of an attachment returned at most (base64 grows it by a third)


async def blocking(fn, *args):
    """Runs a blocking call (imaplib, urllib) without stalling the other requests."""
    return await anyio.to_thread.run_sync(lambda: fn(*args))


def header_text(value) -> str:
    if not value:
        return ""
    try:
        return str(make_header(decode_header(str(value))))
    except Exception:
        return str(value)


INVISIBLE = re.compile("[­͏​-‏⁠﻿]")  # preheader padding in newsletters


def tidy(text: str) -> str:
    """Plain text an agent can read cheaply: no invisible padding, no indentation, at most one
    blank line in a row."""
    text = INVISIBLE.sub("", text.replace("\r\n", "\n").replace("\r", "\n"))
    lines = [re.sub(r"[ \t\xa0]+", " ", line).strip() for line in text.split("\n")]
    return re.sub(r"\n{3,}", "\n\n", "\n".join(lines)).strip()


def html_to_text(text: str) -> str:
    text = re.sub(r"(?is)<(script|style|head).*?</\1>", " ", text)
    text = re.sub(r"(?is)<br\s*/?>|</p>|</div>|</tr>|</h\d>|</li>", "\n", text)
    return tidy(html.unescape(re.sub(r"<[^>]+>", " ", text)))


def body_text(msg: EmailMessage) -> str:
    """The readable text of a mail: the plain part, else the HTML part as text."""
    part = msg.get_body(preferencelist=("plain", "html"))
    if part is None:
        return ""
    try:
        content = part.get_content()
    except Exception:
        content = (part.get_payload(decode=True) or b"").decode("utf-8", errors="replace")
    text = html_to_text(content) if part.get_content_type() == "text/html" else tidy(content)
    return text[:MAX_TEXT] + ("\n[cut off]" if len(text) > MAX_TEXT else "")


def attachments(msg: EmailMessage) -> list[tuple[str, str, int]]:
    out = []
    for part in msg.iter_attachments():
        name = header_text(part.get_filename() or "")
        if name:
            out.append((name, part.get_content_type(), len(part.get_payload(decode=True) or b"")))
    return out


def attachment_result(name: str, mime: str, data: bytes) -> dict:
    """Text files as text, everything else as base64 (with a size limit)."""
    if len(data) > MAX_ATTACHMENT:
        return {"error": f"{name} has {len(data)} bytes, more than {MAX_ATTACHMENT} bytes"}
    if mime.startswith("text/") or mime in ("application/json", "application/xml", "text/csv"):
        return {"name": name, "mime": mime, "size": len(data), "text": data.decode("utf-8", errors="replace")}
    return {"name": name, "mime": mime, "size": len(data), "base64": base64.b64encode(data).decode()}
