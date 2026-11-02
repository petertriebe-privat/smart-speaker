"""Offline check of the approval flow (#57): OpenHands and Telegram are faked. Run: python test_server.py"""
import asyncio
import importlib
import os
import sys
import tempfile

sys.path.insert(0, os.path.dirname(os.path.abspath(__file__)))
tmp = tempfile.mkdtemp()
os.environ.update(LOCAL_BACKEND_API_KEY="x", MCP_BEARER_TOKEN="t", TELEGRAM_BOT_TOKEN="b",
                  TELEGRAM_CHAT_ID="1", STATE_FILE=os.path.join(tmp, "notified.json"))


class Resp:
    def __init__(self, code, data):
        self.status_code, self._data, self.text = code, data, ""

    def json(self):
        return self._data

    def raise_for_status(self):
        pass


def load(approval):
    os.environ["APPROVAL"] = approval
    import server
    s = importlib.reload(server)
    s.started, s.sent = [], []

    async def post(path, json=None):
        s.started.append((path, json))
        return Resp(200, {"id": "abcdef12-0000"})

    async def get(path, params=None):
        return Resp(200, {"profiles": [{"name": s.PROFILE, "id": "p1"}]})

    async def telegram(text, as_html=False, markup=None):
        s.sent.append((text, markup))
        return True

    s.oh.post, s.oh.get, s.telegram = post, get, telegram
    return s


async def main():
    s = load("off")
    out = await s.delegate_task("Fix the backup script", scope="change")
    assert "is working on it" in out and len(s.started) == 1, out
    assert "Scope: research only" not in s.started[0][1]["initial_message"]["content"][0]["text"]
    # a code change goes through a pull request; the owner's merge deploys it (#122)
    assert "never push to main" in s.started[0][1]["initial_message"]["content"][0]["text"]
    # a new feature comes back as a proposal with questions before anything is built (#131)
    assert "first only the proposal" in s.started[0][1]["initial_message"]["content"][0]["text"]
    assert len(s.DELEGATE) <= 600, "Jarvis cuts tool descriptions at 600 characters"
    # unasked, the result goes to Telegram only; a speaker only when named (27.10.2026)
    assert s.started[0][1]["tags"]["notify"] == "message" and out.endswith("comes by Telegram."), out
    out = await s.delegate_task("Check the disks", notify="speak", room="Küche", scope="research")
    assert s.started[1][1]["tags"]["room"] == "Küche" and "speaker Küche" in out, out
    s.started.clear()

    assert (await s.set_approval("")).startswith("Freigabe aus")
    assert (await s.set_approval("an")).startswith("Freigabe an")
    assert (await s.delegate_task("Deploy", scope="change")).startswith("Waiting"), "switched on"
    assert (await s.set_approval("aus")).startswith("Freigabe aus")
    assert "is working on it" in await s.delegate_task("Deploy", scope="change"), "switched off"
    os.remove(s.SETTINGS)

    s = load("change")
    out = await s.delegate_task("Read the logs", scope="research")
    assert "is working on it" in out, out
    assert "Scope: research only" in s.started[0][1]["initial_message"]["content"][0]["text"]
    out = await s.delegate_task("Deploy the relay", scope="change")
    assert out.startswith("Waiting for the owner's approval"), out
    assert len(s.started) == 1 and len(s.sent) == 1, (s.started, s.sent)
    data = s.sent[0][1]["inline_keyboard"][0][0]["callback_data"]
    assert data.startswith("mcp:task_decide:") and len(data.encode()) <= 64, data
    nonce_answer = data[len("mcp:task_decide:"):]
    assert "waiting for the owner's approval" in await s.task_status()
    assert (await s.task_decide(nonce_answer)).startswith("Freigegeben"), "approve"
    assert len(s.started) == 2
    assert "abgelaufen" in await s.task_decide(nonce_answer), "second tap"
    out = await s.delegate_task("Delete old images", scope="change")
    no = s.sent[-1][1]["inline_keyboard"][0][1]["callback_data"][len("mcp:task_decide:"):]
    assert await s.task_decide(no) == "Abgelehnt." and len(s.started) == 2
    print("ok")


asyncio.run(main())
