"""MCP server that lets the assistant delegate tasks to an OpenHands agent profile.

Tools: delegate_task, task_status, task_reply, task_cancel. Tasks are OpenHands conversations
tagged source=assistant; a background loop sends each finished task's final message to the owner
by Telegram. The agent's name (AGENT_NAME) only appears in texts, so it can be renamed.

Approval (#57): a task or message with scope "change" waits until the owner taps "Freigeben" in
Telegram; scope "research" runs at once with the order to change nothing. The buttons carry
"mcp:task_decide:<nonce>:y|n"; the device's Telegram poller calls the device-only tool
task_decide with it (tools whose description starts with "[device]" are never offered to the
model), so only the owner's tap can start a change.
"""

import asyncio
import contextlib
import html
import json
import logging
import os
import re
import secrets
import time
from datetime import datetime, timezone
from pathlib import Path

import httpx
import uvicorn
from mcp.server.fastmcp import FastMCP
from starlette.applications import Starlette
from starlette.middleware.base import BaseHTTPMiddleware
from starlette.responses import JSONResponse
from starlette.routing import Mount

NAME = os.environ.get("AGENT_NAME", "Agent")
PROFILE = os.environ.get("AGENT_PROFILE", NAME)
OH_URL = os.environ.get("OPENHANDS_URL", "http://127.0.0.1:8100")
OH_KEY = os.environ["LOCAL_BACKEND_API_KEY"]
BEARER = os.environ["MCP_BEARER_TOKEN"]
TG_TOKEN = os.environ.get("TELEGRAM_BOT_TOKEN", "")
TG_CHAT = os.environ.get("TELEGRAM_CHAT_ID", "")
RELAY_URL = os.environ.get("RELAY_URL", "").rstrip("/")
RELAY_TOKEN = os.environ.get("RELAY_EVENT_TOKEN", "")
PORT = int(os.environ.get("PORT", "8110"))
STATE = Path(os.environ.get("STATE_FILE", "/data/notified.json"))
PENDING = STATE.with_name("pending.json")
# Approval mode: "off" (default) = every task runs at once, the owner allows everything;
# "change" = tasks of scope "change" wait for the owner's tap in Telegram. APPROVAL is the start
# value; the owner switches it with /freigabe in Telegram (set_approval), kept in SETTINGS.
APPROVAL_DEFAULT = os.environ.get("APPROVAL", "off").strip().lower()
SETTINGS = STATE.with_name("settings.json")
PENDING_MAX_S = 24 * 3600
POLL_S = 15
TAG = {"source": "assistant"}
PREFIX = "[" + os.environ.get("ASSISTANT_NAME", "Jarvis") + "] "
DONE = ("finished", "error", "stuck")

log = logging.getLogger("agent-mcp")
oh = httpx.AsyncClient(base_url=OH_URL, headers={"X-Session-API-Key": OH_KEY}, timeout=30)


# --- OpenHands helpers -------------------------------------------------------------------------

async def profile_id() -> str:
    r = await oh.get("/api/agent-profiles")
    r.raise_for_status()
    for p in r.json()["profiles"]:
        if p["name"] == PROFILE:
            return p["id"]
    raise RuntimeError(f"agent profile '{PROFILE}' not found")


async def assistant_tasks(limit: int = 30) -> list[dict]:
    r = await oh.get("/api/conversations/search", params={"limit": limit})
    r.raise_for_status()
    return [c for c in r.json().get("items", []) if (c.get("tags") or {}).get("source") == "assistant"]


async def find(task_id: str) -> dict | None:
    task_id = task_id.strip().lower()
    for c in await assistant_tasks(100):
        if c["id"].replace("-", "").startswith(task_id.replace("-", "")):
            return c
    return None


REPORT_MARK = re.compile(r"[*#]*\s*REPORT\s*[*#:]*[ \t]*(?:\n|$)")


async def final_response(cid: str) -> str:
    """The agent's report: the text after the last marker "REPORT" (the prompt asks for it on a line
    of its own; models also glue it to the end of a progress note: "...moves.REPORT"), so that notes
    written between tool calls are not sent. All of it without a marker."""
    r = await oh.get(f"/api/conversations/{cid}/agent_final_response")
    if r.status_code != 200:
        return ""
    text = (r.json().get("response") or "").strip()
    marks = list(REPORT_MARK.finditer(text))
    return text[marks[-1].end():].strip() if marks else text


async def current_step(cid: str) -> str:
    r = await oh.get(f"/api/conversations/{cid}/events/search",
                     params={"limit": 1, "kind": "ACPToolCallEvent", "sort_order": "TIMESTAMP_DESC"})
    if r.status_code != 200:
        return ""
    items = r.json().get("items", [])
    return (items[0].get("title") or "")[:160] if items else ""


def short(cid: str) -> str:
    return cid.replace("-", "")[:8]


def age(ts: str) -> str:
    try:
        t = datetime.fromisoformat(ts.replace("Z", "+00:00"))
        if t.tzinfo is None:
            t = t.replace(tzinfo=timezone.utc)
        m = int((datetime.now(timezone.utc) - t).total_seconds() // 60)
    except ValueError:
        return ""
    return f"{m} min" if m < 120 else f"{m // 60} h"


def text_message(text: str) -> dict:
    return {"role": "user", "content": [{"type": "text", "text": text}], "run": True}


# --- MCP tools ---------------------------------------------------------------------------------

mcp = FastMCP("agent", stateless_http=True, json_response=True)


# The assistant cuts tool descriptions at 600 characters (MCP_DESCRIPTION_CHARS).
DELEGATE = (
    f"Starts a NEW task for {NAME}, an autonomous agent with a Linux machine, internet, code and "
    "admin access to the owner's server: multi-step work of minutes (code, server, research). Only "
    "on the owner's own words; pass them on as he said them, add no requirements. A new feature "
    "comes back as a proposal with questions: relay them, his answers go by task_reply. Earlier "
    "work: task_status, task_reply. scope 'research' runs at once, 'change' may wait for his tap. "
    "Report to his Telegram; notify 'speak' (room) or 'call' only if he asks.")
STATUS = (
    f"{NAME}'s tasks. Without task_id: the latest ones (id | title | status | age | outcome), also to "
    "find the task the owner means when he refers to earlier work. With task_id: status, running "
    f"time, current step, the full report once done. Use it when the owner asks what {NAME} does or "
    f"did, or refers to a Telegram message from {NAME}.")
REPLY = (
    f"Continues an existing task of {NAME} in its context (he remembers what he did): answer his "
    "question, change course, or go on with finished work ('sortier fertig', 'und jetzt noch ...'). "
    "Prefer this over a new task when the owner refers to earlier work. Write the message "
    "self-contained. scope, notify and room as for a new task.")
DECIDE = "[device] The owner's tap on an approval button in Telegram. Never offered to a model."
SET_APPROVAL = "[device] The owner's /freigabe command in Telegram: on, off or '' (show). Never offered to a model."

# Sent with every task of scope "research": the agent itself keeps to it; anything more needs a
# task of scope "change", which only the owner releases.
RESEARCH_ONLY = (
    "Scope: research only. Read, look up and report; you may write notes and files inside your "
    "workspace. Change nothing outside it: no commits, pushes, deploys, server or service changes, "
    "messages to anyone, purchases or accounts. If the task needs such a change, stop and say so in "
    "your report; the assistant then asks the owner for approval.\n\n")

# Sent with every task of scope "change" (#122): a change to the assistant itself goes through a pull
# request; the owner merges it, and the CI deploys it on every Echo from the Raspberry Pi
# (services/deployer). The server agent works with the owner's GitHub account: only this rule keeps it from
# merging itself.
CODE_RULES = (
    "A new feature or a change of behaviour: first only the proposal and your questions (your "
    "instructions say how), no branch yet; build after the owner's answer. A bug fix with a clear "
    f"cause, or once agreed, if it changes the assistant's own code ({os.environ.get('ASSISTANT_REPO', 'his repository')}): "
    "work on a new branch agent/<topic> from the current main, keep to the repository's CLAUDE.md, run go vet "
    "and go test in core/ and add a test for the change, push the branch and open a pull request "
    "with gh that says what changed and how to test it; put its link in your report. Never merge, "
    "never push to main, never deploy and never log in to a speaker, even though your GitHub access would allow it: the "
    "owner merges it, and his merge deploys it.\n\n")


def scope_of(scope: str) -> str:
    return "research" if scope == "research" else "change"


def notify_of(notify: str) -> str:
    """How the result reaches the owner: "message" (default) = the Telegram report only, "speak" =
    also announced on a assistant speaker, "call" = the assistant phones it. Results came as announcements
    in the living room while the owner was elsewhere (27.10.2026): unasked, only his phone."""
    return notify if notify in ("speak", "call") else "message"


def after_text(notify: str, room: str) -> str:
    if notify == "call":
        return "The report comes by Telegram and the assistant calls the owner when it is done."
    if notify == "speak":
        return f"The report comes by Telegram and is announced on the speaker {room or 'here'}."
    return "The report comes by Telegram."


async def start_task(task: str, notify: str, scope: str, room: str = "") -> str:
    """Starts the OpenHands conversation; the result text for the assistant."""
    lead = RESEARCH_ONLY if scope == "research" else CODE_RULES
    body = {
        "agent_profile_id": await profile_id(),
        "workspace": {"kind": "LocalWorkspace", "working_dir": "/projects"},
        "initial_message": text_message(PREFIX + lead + task.strip()),
        "tags": {**TAG, "notify": notify, "scope": scope, "room": room},
        "autotitle": True,
    }
    r = await oh.post("/api/conversations", json=body)
    if r.status_code >= 300:
        log.warning("start failed: %s %s", r.status_code, r.text[:300])
        return f"Error: {NAME} could not be started (HTTP {r.status_code})."
    cid = r.json()["id"]
    log.info("started %s (%s)", cid, scope)
    return f"{NAME} is working on it, task id {short(cid)}. {after_text(notify, room)}"


async def send_reply(c: dict, message: str, notify: str, scope: str, room: str = "") -> str:
    """Posts a message into an existing task; the result text for the assistant."""
    tags = c.get("tags") or {}
    # A change released once keeps its scope; a research task becomes one only by approval.
    scope = "change" if tags.get("scope", "change") == "change" or scope == "change" else "research"
    want = {"notify": notify, "scope": scope, "room": room}
    if any(tags.get(k, "") != v for k, v in want.items()):  # how this round runs and is delivered
        await oh.patch(f"/api/conversations/{c['id']}", json={"tags": {**tags, **want}})
    lead = RESEARCH_ONLY if scope == "research" else CODE_RULES
    r = await oh.post(f"/api/conversations/{c['id']}/events", json=text_message(PREFIX + lead + message.strip()))
    if r.status_code >= 300:
        return f"Error: message not delivered (HTTP {r.status_code})."
    return f"Sent to {NAME}, task {short(c['id'])} continues. {after_text(notify, room)}"


# --- Approval ----------------------------------------------------------------------------------

def approval_mode() -> str:
    try:
        mode = json.loads(SETTINGS.read_text()).get("approval", APPROVAL_DEFAULT)
    except (OSError, ValueError):
        mode = APPROVAL_DEFAULT
    return "change" if mode == "change" else "off"


def asks() -> bool:
    return approval_mode() == "change"


def load_pending() -> dict:
    try:
        pending = json.loads(PENDING.read_text())
    except (OSError, ValueError):
        return {}
    now = time.time()
    return {k: v for k, v in pending.items() if now - v.get("created", 0) < PENDING_MAX_S}


def save_pending(pending: dict) -> None:
    PENDING.parent.mkdir(parents=True, exist_ok=True)
    tmp = PENDING.with_suffix(".tmp")
    tmp.write_text(json.dumps(pending))
    tmp.replace(PENDING)


def approval_buttons(nonce: str) -> dict:
    return {"inline_keyboard": [[
        {"text": "✅ Freigeben", "callback_data": f"mcp:task_decide:{nonce}:y"},
        {"text": "❌ Ablehnen", "callback_data": f"mcp:task_decide:{nonce}:n"},
    ]]}


async def ask_approval(entry: dict) -> str:
    """Keeps the request and sends the owner the buttons; the result text for the assistant."""
    if not (TG_TOKEN and TG_CHAT):
        return f"Error: {NAME} needs the owner's approval for this, and Telegram is not configured."
    nonce = secrets.token_hex(8)
    entry["created"] = time.time()
    pending = load_pending()
    pending[nonce] = entry
    save_pending(pending)
    what = "Neuer Auftrag" if entry["kind"] == "new" else f"Weiter mit Aufgabe {short(entry['cid'])}"
    text = (f"<b>{html.escape(NAME)} · Freigabe nötig</b>\n{what}, der etwas verändern darf:\n\n"
            f"{html.escape(entry['text'][:3000])}")
    if not await telegram(text, as_html=True, markup=approval_buttons(nonce)):
        pending.pop(nonce, None)
        save_pending(pending)
        return "Error: the approval request could not be sent by Telegram."
    log.info("approval asked %s (%s)", nonce, entry["kind"])
    return (f"Waiting for the owner's approval: a request with buttons went to his Telegram. {NAME} "
            "starts once he taps 'Freigeben' (valid 24 hours). Tell him so.")


@mcp.tool(description=DELEGATE)
async def delegate_task(task: str, notify: str = "message", scope: str = "change", room: str = "") -> str:
    notify, scope, room = notify_of(notify), scope_of(scope), room.strip()[:40]
    if scope == "research" or not asks():
        return await start_task(task, notify, scope, room)
    return await ask_approval({"kind": "new", "text": task.strip(), "notify": notify, "room": room})


@mcp.tool(description=STATUS)
async def task_status(task_id: str = "") -> str:
    if not task_id:
        tasks = (await assistant_tasks())[:8]
        lines = [f"waiting for the owner's approval in Telegram | {e['text'][:120]}"
                 for e in load_pending().values()]
        if not tasks and not lines:
            return f"{NAME} has no tasks."
        for c in tasks:
            line = f"{short(c['id'])} | {title_of(c)} | {c['execution_status']} | {age(c['updated_at'])} ago"
            if c["execution_status"] in DONE:
                first = (await final_response(c["id"])).split("\n")[0][:200]
                if first:
                    line += f" | {first}"
            lines.append(line)
        return "\n".join(lines)
    c = await find(task_id)
    if not c:
        return f"Error: no task {task_id}."
    out = [f"Task {short(c['id'])}: {c.get('title') or '-'}",
           f"Status: {c['execution_status']}, started {age(c['created_at'])} ago"]
    if c["execution_status"] in DONE:
        out.append("Report:\n" + ((await final_response(c["id"]))[:4000] or "(none)"))
    else:
        step = await current_step(c["id"])
        if step:
            out.append(f"Current step: {step}")
    return "\n".join(out)


@mcp.tool(description=REPLY)
async def task_reply(task_id: str, message: str, notify: str = "message", scope: str = "change",
                     room: str = "") -> str:
    notify, scope, room = notify_of(notify), scope_of(scope), room.strip()[:40]
    c = await find(task_id)
    if not c:
        return f"Error: no task {task_id}."
    if scope == "research" or not asks() or (c.get("tags") or {}).get("scope", "change") == "change":
        # Research, or a task the owner released (or one from before approvals existed).
        return await send_reply(c, message, notify, scope, room)
    return await ask_approval({"kind": "reply", "cid": c["id"], "text": message.strip(), "notify": notify,
                               "room": room})


@mcp.tool(description=DECIDE)
async def task_decide(data: str) -> str:
    """data: "<nonce>:y" or "<nonce>:n" from the button."""
    nonce, _, answer = data.partition(":")
    pending = load_pending()
    entry = pending.pop(nonce, None)
    if entry is None:
        return "Diese Anfrage ist abgelaufen oder schon entschieden."
    save_pending(pending)
    if answer != "y":
        log.info("declined %s", nonce)
        return "Abgelehnt."
    log.info("approved %s", nonce)
    if entry["kind"] == "new":
        result = await start_task(entry["text"], entry["notify"], "change", entry.get("room", ""))
    else:
        c = await find(entry["cid"])
        result = (await send_reply(c, entry["text"], entry["notify"], "change", entry.get("room", ""))
                  if c else "Error: the task is gone.")
    if result.startswith("Error"):
        return "Freigegeben, aber: " + result
    return f"Freigegeben. {NAME} arbeitet daran."


@mcp.tool(description=SET_APPROVAL)
async def set_approval(data: str = "") -> str:
    data = data.strip().lower()
    if data in ("an", "on", "ein"):
        mode = "change"
    elif data in ("aus", "off"):
        mode = "off"
    elif data:
        return "Unbekannt. /freigabe an, /freigabe aus oder /freigabe für den Stand."
    else:
        mode = ""
    if mode:
        SETTINGS.parent.mkdir(parents=True, exist_ok=True)
        SETTINGS.write_text(json.dumps({"approval": mode}))
        log.info("approval mode %s", mode)
    if asks():
        return (f"Freigabe an: Aufträge an {NAME}, die etwas verändern, warten auf deinen Klick. "
                "Recherche läuft sofort. Ausschalten: /freigabe aus")
    return f"Freigabe aus: {NAME} startet jeden Auftrag sofort. Einschalten: /freigabe an"


@mcp.tool(description=f"Stop one of {NAME}'s running tasks.")
async def task_cancel(task_id: str) -> str:
    c = await find(task_id)
    if not c:
        return f"Error: no task {task_id}."
    r = await oh.post(f"/api/conversations/{c['id']}/interrupt")
    if r.status_code >= 300:
        return f"Error: could not stop it (HTTP {r.status_code})."
    return f"Task {short(c['id'])} stopped."


# --- Telegram notifier -------------------------------------------------------------------------

def load_state() -> dict | None:
    try:
        return json.loads(STATE.read_text())
    except (OSError, ValueError):
        return None


def save_state(state: dict) -> None:
    STATE.parent.mkdir(parents=True, exist_ok=True)
    tmp = STATE.with_suffix(".tmp")
    tmp.write_text(json.dumps(state))
    tmp.replace(STATE)


HEADING = re.compile(r"^[^\s•\-][^:]{0,40}:$")  # "Erledigt:", "Brauche von dir:"


def telegram_html(title: str, report: str) -> str:
    """The report as Telegram HTML: name and task as a bold head, the outcome line and the
    section headings bold, the rest as written (bullets "•")."""
    lines = report.strip().split("\n")
    out = [f"<b>{html.escape(NAME)} · {html.escape(title)}</b>", ""]
    for i, line in enumerate(lines):
        text = html.escape(line.strip())
        out.append(f"<b>{text}</b>" if text and (i == 0 or HEADING.match(line.strip())) else text)
    return "\n".join(out)


async def telegram(text: str, as_html: bool = False, markup: dict | None = None) -> bool:
    if not (TG_TOKEN and TG_CHAT):
        return False
    body = {"chat_id": TG_CHAT, "text": text[:4000], "disable_web_page_preview": True}
    if as_html:
        body["parse_mode"] = "HTML"
    if markup:
        body["reply_markup"] = markup
    async with httpx.AsyncClient(timeout=20) as tg:
        r = await tg.post(f"https://api.telegram.org/bot{TG_TOKEN}/sendMessage", json=body)
        if r.status_code == 400 and as_html:  # malformed HTML: plain text rather than nothing
            body.pop("parse_mode")
            body["text"] = re.sub(r"</?b>", "", body["text"])
            r = await tg.post(f"https://api.telegram.org/bot{TG_TOKEN}/sendMessage", json=body)
    if r.status_code != 200:
        log.warning("telegram failed: %s %s", r.status_code, r.text[:200])
    return r.status_code == 200


async def announce(text: str, deliver: str, room: str = "") -> None:
    """Through the WhatsApp relay's /event queue, which the device polls: "speak" = the assistant announces
    `text` on the speaker in `room` ('' = the device itself), "call" = the assistant phones the owner and
    tells it (then he can ask back)."""
    if not (RELAY_URL and RELAY_TOKEN):
        return
    limit = 1500 if deliver == "call" else 300
    async with httpx.AsyncClient(timeout=20) as relay:
        r = await relay.post(f"{RELAY_URL}/event", headers={"Authorization": f"Bearer {RELAY_TOKEN}"},
                             json={"source": NAME, "text": text[:limit], "deliver": deliver, "room": room})
    if r.status_code != 200:
        log.warning("announcement failed: %s %s", r.status_code, r.text[:200])


def title_of(c: dict) -> str:
    return (c.get("title") or short(c["id"])).replace(PREFIX.strip(), "").strip()


async def notifier() -> None:
    state = load_state()
    while True:
        try:
            tasks = await assistant_tasks()
            if state is None:  # first start: do not announce old tasks
                state = {c["id"]: c["updated_at"] for c in tasks if c["execution_status"] in DONE}
                save_state(state)
            for c in tasks:
                if c["execution_status"] not in DONE or state.get(c["id"]) == c["updated_at"]:
                    continue
                report = await final_response(c["id"])
                tags = c.get("tags") or {}
                notify = notify_of(tags.get("notify", ""))
                if notify == "speak" and "room" not in tags:  # "speak" was the default before rooms
                    notify = "message"
                if c["execution_status"] != "finished" or not report:
                    report = (f"Abgebrochen (Status {c['execution_status']}).\n" + report).strip()
                first = report.split("\n")[0]
                if notify == "call":
                    spoken = f"Auftrag \"{title_of(c)}\", Aufgaben-ID {short(c['id'])}. Bericht:\n{report}"
                else:
                    spoken = f"{NAME} ist fertig: {first}"
                if await telegram(telegram_html(title_of(c), report), as_html=True):
                    state[c["id"]] = c["updated_at"]
                    save_state(state)
                    log.info("notified %s (%s)", c["id"], notify)
                    if notify != "message":
                        await announce(spoken, notify, tags.get("room", ""))
        except Exception as e:  # keep the loop alive, log the cause
            log.warning("notifier: %s", e)
        await asyncio.sleep(POLL_S)


# --- App ---------------------------------------------------------------------------------------

class BearerAuth(BaseHTTPMiddleware):
    async def dispatch(self, request, call_next):
        if request.headers.get("authorization", "") != f"Bearer {BEARER}":
            return JSONResponse({"error": "unauthorized"}, status_code=401)
        return await call_next(request)


@contextlib.asynccontextmanager
async def lifespan(app):
    task = asyncio.create_task(notifier())
    async with mcp.session_manager.run():
        yield
    task.cancel()


app = Starlette(routes=[Mount("/", app=mcp.streamable_http_app())], lifespan=lifespan)
app.add_middleware(BearerAuth)

if __name__ == "__main__":
    logging.basicConfig(level=logging.INFO, format="%(asctime)s %(name)s %(levelname)s %(message)s")
    logging.getLogger("httpx").setLevel(logging.WARNING)  # one line per poll otherwise
    uvicorn.run(app, host="127.0.0.1", port=PORT, log_level="warning")
