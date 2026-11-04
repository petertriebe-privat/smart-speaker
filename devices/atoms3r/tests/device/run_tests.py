"""Regression tests for the assistant on the device, over USB serial (opening the port does not reset it).

Every tool is called for real through the serial ">tool {json}" command (system/console).
Read-only calls where possible; tests that write clean up after themselves (create, check,
delete). Default: free and without side effects outside the device, Home Assistant, the
mailbox backups and a test event on 2030-01-01 in the Google Calendar (deleted again). Close any serial monitor first (the port can only be opened once).

    python tests/device/run_tests.py                 # all free tests
    python tests/device/run_tests.py --mail          # + a test mail to the owner (sent folder)
    python tests/device/run_tests.py --live          # + a short GPT Live session (~1 ct)
    python tests/device/run_tests.py --telegram      # + a test message to the owner's Telegram chat
    python tests/device/run_tests.py --maps          # + Places/Routes (of 1000 a month), weather, a YouTube search
    python tests/device/run_tests.py --search        # + one Tavily web search (of 1000 free a month)
    python tests/device/run_tests.py --mcp           # + DeepWiki's public MCP server (free, no login)
    python tests/device/run_tests.py --update-golden # accept the current tool definitions

The contract test compares the tool definitions (names, descriptions, schemas) with
golden/tools.json: a refactoring must not change them. Home Assistant's tools (names with "__")
come from HA and are reported separately.
"""
import argparse
import json
import pathlib
import re
import sys
import time

import serial  # pyserial (PlatformIO's Python has it)

HERE = pathlib.Path(__file__).parent
GOLDEN = HERE / "golden" / "tools.json"
# The agent's extra tool (mail commands, scheduled tasks), as before the tool registry.
SPEAK = {"type": "function", "name": "speak",
         "description": "Says a text out loud on the speaker at home (announcements, reading out mails or "
                        "answers). Write it as natural spoken German, without formatting.",
         "parameters": {"type": "object", "additionalProperties": False, "properties": {"text": {"type": "string"}},
                        "required": ["text"]}}
VOICE_ONLY = ["end_call", "press_keys"]   # tools of conversations (phone calls) the agent does not get
AGENT_ONLY = ["start_conversation"]  # tools of the agent a conversation does not get
CRASH = re.compile(r"Guru Meditation|stack overflow|Rebooting|assert failed|abort\(\)|panic")


class Device:
    def __init__(self, port):
        self.started = time.time()
        self.serial = serial.Serial()
        self.serial.port, self.serial.baudrate, self.serial.timeout = port, 115200, 0.2
        self.serial.dtr = self.serial.rts = False  # no reset
        self.serial.open()
        self.pending = b""
        self.log = []  # every line, for the crash check

    def lines(self, timeout):
        """Yields lines as they arrive until `timeout` seconds pass (the caller stops earlier)."""
        end = time.time() + timeout
        while time.time() < end:
            self.pending += self.serial.read(4096)
            while b"\n" in self.pending:
                line, self.pending = self.pending.split(b"\n", 1)
                text = line.decode("utf-8", "replace").rstrip("\r")
                self.log.append(f"{time.time() - self.started:7.1f} {text}")
                yield text

    def send(self, text):
        self.serial.write(text.encode("utf-8"))
        self.serial.flush()

    def drain(self, seconds=0.5):
        for _ in self.lines(seconds):
            pass

    def tool(self, name, arguments=None, timeout=30, person=None):
        """Runs a tool (slow ones in the registry's worker, as in a conversation), as `person` if given
        (assistant/people id); returns its result text (None on timeout). The device echoes name and
        arguments exactly: a late answer to an earlier call is never taken for this one."""
        self.drain(0.2)
        call = f"{name} {json.dumps(arguments or {}, ensure_ascii=False)}"
        self.send(f">@{person} {call}\n" if person else f">{call}\n")
        result, inside = [], False
        for line in self.lines(timeout):
            if line == f"[Tool] {call}":
                inside = True
            elif inside and line == "[Tool-End]":
                return "\n".join(result)
            elif inside:
                result.append(line[3:] if not result and line.startswith("-> ") else line)
        return None

    def command(self, letter, until, timeout=10):
        """Sends a console letter; the lines until one contains `until` (all of them on timeout)."""
        self.drain(0.2)
        self.send(letter)
        text = []
        for line in self.lines(timeout):
            text.append(line)
            if until in line:
                break
        return "\n".join(text)

    def member(self):
        """A member of a network of devices (PEER_KEY): memory, tasks and people are the leader's."""
        return '[Peer] "' in self.command("Y", "[Shared]")

    def notes(self, timeout=15):
        """The memory notes as the backend model sees them (serial "g")."""
        self.drain(0.2)
        self.send("g")
        text = []
        for line in self.lines(timeout):
            if line.startswith("[Memory] ") and "on the device" in line:
                return "\n".join(text)
            text.append(line)
        return None

    def dump_tools(self, timeout=15, agent=False):
        """The tool definitions (serial "D", agent "A"). Another task's log line can land in the middle of
        the dump (e.g. the memory backup going to the mailbox): then it asks again."""
        for _ in range(3):
            try:
                return self._dump_tools(timeout, agent)
            except json.JSONDecodeError:
                self.drain(1)
        return None

    def _dump_tools(self, timeout, agent):
        self.drain(0.2)
        self.send("A" if agent else "D")
        inside, text = False, []
        for line in self.lines(timeout):
            if line == "[TOOLS-BEGIN]":
                inside = True
            elif line == "[TOOLS-END]":
                return json.loads("".join(text))
            elif inside:
                text.append(line)
        return None


class Suite:
    def __init__(self, device):
        self.device, self.results = device, []

    def check(self, name, ok, detail=""):
        self.results.append((name, bool(ok), detail))
        print(f"  {'OK  ' if ok else 'FAIL'} {name}" + (f"  ({detail})" if detail and not ok else ""))
        if not ok:  # what the device said meanwhile
            for line in self.device.log[-8:]:
                print(f"         | {line[:150]}")
        return ok

    def contains(self, name, text, *needles):
        missing = [n for n in needles if text is None or n not in text]
        return self.check(name, not missing, f"missing {missing!r} in {str(text)[:160]!r}")


def short(tool):
    return tool.get("name") or tool.get("type")


def contract(suite, device, update):
    print("Tool contract")
    tools = device.dump_tools()
    if not suite.check("tool list read", tools is not None):
        return
    # External, not part of the contract: Home Assistant's tools ("__" in the name) and those of MCP
    # servers the admin connected (description "[Server] ...", services/mcp).
    connected = [t for t in tools if (t.get("description") or "").startswith("[")]
    own = [t for t in tools if "__" not in (t.get("name") or "") and t not in connected]
    ha = [t for t in tools if "__" in (t.get("name") or "")]
    if connected:
        print(f"  (not compared: {len(connected)} tools of connected MCP servers)")
    if update or not GOLDEN.exists():
        GOLDEN.parent.mkdir(parents=True, exist_ok=True)
        # Home Assistant's tools come from HA: only their names are kept.
        GOLDEN.write_text(json.dumps({"own": own, "home_assistant": [short(t) for t in ha]}, ensure_ascii=False,
                                     indent=1), encoding="utf-8")
        suite.check(f"golden file written ({len(own)} own, {len(ha)} HA)", True)
        return
    golden = json.loads(GOLDEN.read_text(encoding="utf-8"))
    names = lambda items: [short(t) for t in items]
    suite.check("order and names of the own tools", names(own) == names(golden["own"]),
                f"now {names(own)}")
    before = {short(t): t for t in golden["own"]}
    for tool in own:
        old = before.get(short(tool))
        if old is not None and old != tool:
            suite.check(f"definition {short(tool)} unchanged", False, "description or schema changed")
    suite.check("definitions of the own tools unchanged",
                all(before.get(short(t)) == t for t in own))
    same_ha = names(ha) == golden["home_assistant"]
    suite.check("Home Assistant tools as when recorded (external)", same_ha, f"now {names(ha)}")
    agent = device.dump_tools(agent=True) or []
    # Only in conversations: end_call (phone). Only for the agent: speak.
    shared_agent = [n for n in names(agent) if n not in AGENT_ONLY]
    shared_voice = [n for n in names(tools) if n not in VOICE_ONLY]
    suite.check("agent: speak + the same tools", shared_agent == ["speak"] + shared_voice,
                f"now {names(agent)[:5]} ... {names(agent)[-3:]}")
    speak = next((t for t in agent if t.get("name") == "speak"), {})
    suite.check("definition speak unchanged", speak == SPEAK, str(speak)[:160])


def cleanup(device):
    """Leftovers of an aborted run (answers that came too late): tasks, automations, facts."""
    for task in re.findall(r"#(\d+) Regressionstest", device.tool("list_tasks") or ""):
        device.tool("delete_task", {"id": int(task)})
    for automation in re.findall(r"Regressionstest 4711 \[(jarvis_\d+)\]", device.tool("list_home_automations") or ""):
        device.tool("delete_home_automation", {"id": automation})
    device.tool("delete_skill", {"name": "Regressionstest 4711"})
    device.tool("remove_person", {"person": PERSON})
    found = device.tool("list_events", {"from": TEST_DAY, "to": TEST_DAY, "query": "Regressionstest 4711"}, 45) or ""
    for event in re.findall(r"\[id (\S+)\]", found):
        device.tool("delete_event", {"id": event}, 45)
    found = device.tool("recall", {"query": "Regressionstest-Fakt 4711", "date": ""}) or ""
    if "Remembered: Regressionstest-Fakt" in found:
        print("  (note: Regressionstest-Fakt from an earlier run, ids only in the notes)")


PERSON = "Regressionstest"
CHILD = "Regressionstest-Kind"


def secret(name, default=""):
    """A #define from local/secrets.h (the household's own values), else `default`."""
    path = HERE.resolve().parents[3] / "local" / "secrets.h"
    text = path.read_text(encoding="utf-8") if path.exists() else ""
    match = re.search(rf'^#define {name}\s+"([^"]*)"', text, re.M)
    return match.group(1) if match else default


OWNER = secret("OWNER_NAME", "Admin")            # the admin, as the firmware registers him
OWNER_ID = re.sub(r"\s+", "-", OWNER.strip().lower())
OWNER_EMAIL = secret("OWNER_EMAIL")


def people(suite, device):
    print("People")
    listed = device.tool("list_people")
    suite.contains("list_people names the admin", listed, f"(id {OWNER_ID}, admin)")
    made = device.tool("add_person", {"name": PERSON, "phones": ["+49 170 4711471"], "telegram": 0, "email": ""})
    suite.contains("add_person registers (number normalized)", made, "Registered: " + PERSON, "01704711471")
    suite.contains("send_welcome without an address sends nothing",
                   device.tool("send_welcome", {"person": PERSON}, 45), "Error", "no e-mail address")
    suite.contains("duplicate name refused",
                   device.tool("add_person", {"name": PERSON, "phones": [], "telegram": 0, "email": ""}), "Error")
    suite.contains("another person's number refused",
                   device.tool("add_person", {"name": "Regressionstest B", "phones": ["01704711471"], "telegram": 0,
                                              "email": ""}), "already belongs")
    who = "regressionstest"
    suite.contains("private tools only for the admin",
                   device.tool("list_emails", {"unread_only": True, "folder": "inbox"}, person=who), f"only {OWNER}")
    suite.contains("administration only for the admin",
                   device.tool("remove_person", {"person": OWNER_ID}, person=who), f"only {OWNER}")
    shared = device.tool("list_tasks", person=who)
    suite.check("shared tools for everyone", shared is not None and "Error" not in shared, str(shared)[:160])
    # Profiles: each person has their own (assistant/memory).
    suite.contains("update_profile for a named person",
                   device.tool("update_profile", {"person": PERSON, "field": "interests", "value": "Schach"}),
                   "Profile of " + PERSON + " updated")
    notes = device.notes()
    suite.contains("notes: a profile per person, speaker unknown", notes, "You do not know for sure who is speaking",
                   f"Profile of {OWNER}", f"Profile of {PERSON} (id {who})", "Interessen und Vorlieben: Schach")
    suite.contains("without person and speaker: ask back",
                   device.tool("update_profile", {"person": "", "field": "interests", "value": "x"}), "whose profile")
    suite.contains("as the person: their own profile",
                   device.tool("update_profile", {"person": "", "field": "interests", "value": "Schach, Tee"}, person=who),
                   "Profile of " + PERSON + " updated")
    suite.contains("unknown person refused",
                   device.tool("update_profile", {"person": "Niemand 4711", "field": "interests", "value": "x"}),
                   "is not registered as a person")
    suite.contains("Telegram to a person without an account refused",
                   device.tool("send_telegram", {"text": "x", "to": PERSON}), "no Telegram account")
    made = device.tool("create_task", {"title": "Regressionstest 4711", "kind": "reminder", "text": "x",
                                       "schedule": "once", "date": "2030-01-01", "time": "12:00"}, person=who)
    suite.contains("a task remembers for whom", made, "für " + PERSON)
    task = re.search(r"#(\d+)", made or "")
    if task:
        device.tool("delete_task", {"id": int(task.group(1))})
    device.tool("update_profile", {"person": PERSON, "field": "interests", "value": ""})
    notes = device.notes() or device.notes()  # a backup's log line can cut the first read short
    suite.check("cleared profile field is gone", notes is not None and "Schach" not in notes, str(notes)[:160])
    # Activity log (memory index with person and channel).
    everyone = device.tool("list_activities", {"person": "", "date": "", "channel": ""}, 45)
    suite.check("list_activities answers", everyone is not None and everyone.startswith(("Activities", "No activities")),
                str(everyone)[:160])
    suite.contains("activities of a person without entries",
                   device.tool("list_activities", {"person": PERSON, "date": "", "channel": "telegram"}, 45),
                   "No activities")
    suite.contains("non-admin sees only their own activities",
                   device.tool("list_activities", {"person": OWNER, "date": "", "channel": ""}, 45, person=who),
                   "only the admin")
    suite.contains("activities: unknown person refused",
                   device.tool("list_activities", {"person": "Niemand 4711", "date": "", "channel": ""}, 45),
                   "is not registered as a person")
    changed = device.tool("update_person", {"person": PERSON, "name": PERSON, "phones": ["01704711471"],
                                            "telegram": 4711, "email": "test@example.org", "blocked": ["set_volume"], "child": False})
    suite.contains("update_person changes", changed, "Telegram 4711", "test@example.org", "blocked: set_volume")
    suite.contains("set_humor: own level", device.tool("set_humor", {"person": "", "level": 3}, person=who), "level 3")
    suite.contains("set_humor: not for others", device.tool("set_humor", {"person": OWNER, "level": 0}, person=who),
                   f"only {OWNER}")
    suite.contains("blocked tool refused", device.tool("set_volume", {"step": 0}, person=who), "blocked")
    if "locate_person" in [t.get("name") for t in device.dump_tools() or []]:
        suite.contains("locate_person only for the admin",
                       device.tool("locate_person", {"person": OWNER}, 45, person=who), f"only {OWNER}")
        suite.contains("locate_person: unknown person refused",
                       device.tool("locate_person", {"person": "Niemand 4711"}, 45), "not a person of the household")
    suite.contains("the admin cannot be removed", device.tool("remove_person", {"person": OWNER}), "Error")
    suite.contains("remove_person removes", device.tool("remove_person", {"person": PERSON}), "removed")
    left = device.tool("list_people")
    suite.check("person is gone", left is not None and PERSON not in left)
    if left is not None and left.count("(id ") == 1:  # only the admin registered: he is the speaker
        suite.contains("only the admin registered: he is the speaker", device.notes(), f"You are talking with {OWNER} (admin")


def tasks(suite, device):
    print("Scheduler")
    suite.check("list_tasks answers", device.tool("list_tasks") is not None)
    bad = device.tool("create_task", {"title": "Regressionstest", "kind": "reminder", "text": "x",
                                      "schedule": "weekly", "time": "10:00", "days": []})
    suite.contains("invalid schedule refused", bad, "Error")
    made = device.tool("create_task", {"title": "Regressionstest 4711", "kind": "reminder",
                                       "text": "Das ist ein Regressionstest.", "schedule": "once",
                                       "date": "2030-01-01", "time": "12:00"})
    suite.contains("create_task creates", made, "Created: #", "Regressionstest 4711", "2030")
    far = device.tool("create_task", {"title": "Regressionstest", "kind": "reminder", "text": "x",
                                      "schedule": "once", "date": "2099-01-01", "time": "12:00"})
    suite.contains("date after 2037 clearly refused (32-bit time)", far, "2037")
    match = re.search(r"#(\d+)", made or "")
    if not match:
        return
    task = int(match.group(1))
    suite.contains("list_tasks shows it", device.tool("list_tasks"), "Regressionstest 4711")
    suite.contains("pause_task pauses", device.tool("pause_task", {"id": task, "paused": True}), "Paused")
    suite.contains("pause_task resumes", device.tool("pause_task", {"id": task, "paused": False}), "Running again")
    suite.contains("delete_task deletes", device.tool("delete_task", {"id": task}), "deleted")
    left = device.tool("list_tasks")
    suite.check("task is gone", left is not None and f"#{task} " not in left)
    suite.contains("unknown id refused", device.tool("delete_task", {"id": 999999}), "Error")


def memory(suite, device):
    print("Memory")
    made = device.tool("remember", {"text": "Regressionstest-Fakt 4711", "category": "Sonstiges", "replaces": 0})
    suite.contains("remember stores", made, "Remembered (id ")
    match = re.search(r"id (\d+)", made or "")
    suite.contains("recall finds it", device.tool("recall", {"query": "Regressionstest-Fakt 4711", "date": ""}),
                   "Regressionstest-Fakt 4711")
    if match:
        suite.contains("forget deletes", device.tool("forget", {"ids": [int(match.group(1))]}), "Deleted")
    suite.contains("unknown profile field refused",
                   device.tool("update_profile", {"field": "schuhgroesse", "value": "44"}), "Error")
    suite.contains("open_topic without a hit", device.tool("open_topic", {"title": "Regressionstest-Thema 4711"}, 45),
                   "no detailed notes")
    suite.contains("open_conversation without a hit",
                   device.tool("open_conversation", {"at": "1999-01-01 00:00"}, 45), "No conversation")
    # The oldest of the newest conversations recall names (the newest may still wait in the outbox).
    # Mail notices are listed there too but have no transcript (since the Echo watches the mail):
    # the first entry, oldest first, that opens counts.
    newest = re.findall(r"Conversation (\d{4}-\d\d-\d\d \d\d:\d\d)", device.tool("recall", {"query": "", "date": ""}) or "")
    if newest:
        opened = ""
        for at in sorted(set(newest)):
            opened = device.tool("open_conversation", {"at": at}, 45) or ""
            if "The earlier conversation" in opened:
                break
        suite.contains("open_conversation finds an archived one", opened, "The earlier conversation", "Transkript")


def skills(suite, device):
    print("Skills")
    name = "Regressionstest 4711"
    made = device.tool("save_skill", {"name": name, "when": "Wenn der Nutzer Regressionstest sagt.",
                                      "instructions": "Schritt 1: nichts tun. Schritt 2: Bescheid sagen."})
    suite.contains("save_skill stores", made, "Skill saved: " + name)
    suite.contains("notes name it with its trigger", device.notes(), name + ": Wenn der Nutzer Regressionstest sagt.")
    suite.contains("use_skill loads the instructions (name ignoring case)",
                   device.tool("use_skill", {"name": " regressionstest 4711 "}), "Schritt 1: nichts tun")
    again = device.tool("save_skill", {"name": name, "when": "Neuer Auslöser.", "instructions": "Nur noch Schritt 3."})
    suite.contains("save_skill replaces the same name", again, "Skill updated")
    loaded = device.tool("use_skill", {"name": name}) or ""
    suite.check("replaced instructions apply", "Schritt 3" in loaded and "Schritt 1" not in loaded, loaded[:160])
    # The limits are checked alike; "when" is short enough for the console's USB receive buffer.
    suite.contains("too long trigger refused",
                   device.tool("save_skill", {"name": "Regressionstest lang", "when": "x" * 210, "instructions": "x"}),
                   "Error: when too long")
    suite.contains("empty name refused",
                   device.tool("save_skill", {"name": " ", "when": "x", "instructions": "x"}), "Error")
    suite.contains("delete_skill deletes", device.tool("delete_skill", {"name": name}), "Skill deleted")
    notes = device.notes()
    suite.check("skill is gone from the notes", notes is not None and name not in notes)
    suite.contains("unknown skill refused", device.tool("use_skill", {"name": name}), "Error: no skill")


def home_assistant(suite, device):
    print("Home Assistant")
    suite.contains("find_entities finds lights", device.tool("find_entities", {"query": "", "domain": "light"}),
                   "light.")
    tools = device.dump_tools() or []
    context = next((t["name"] for t in tools if (t.get("name") or "").endswith("GetLiveContext")), None)
    if suite.check("HA tool GetLiveContext present", context):
        suite.contains("GetLiveContext (MCP) answers", device.tool(context, {}), "Live Context")
    config = {"triggers": [{"trigger": "event", "event_type": "jarvis_regressionstest_nie"}], "conditions": [],
              "actions": [{"event": "jarvis_announce", "event_data": {"text": "Regressionstest"}}], "mode": "single"}
    made = device.tool("create_home_automation", {"alias": "Regressionstest 4711", "description": "Test",
                                                  "config": json.dumps(config)})
    suite.contains("create_home_automation creates", made, "Created", "jarvis_")
    match = re.search(r"(jarvis_\d+)", made or "")
    suite.contains("list_home_automations shows it", device.tool("list_home_automations"), "Regressionstest 4711")
    if match:
        suite.contains("delete_home_automation deletes",
                       device.tool("delete_home_automation", {"id": match.group(1)}), "deleted")
    suite.contains("someone else's automation is not deleted",
                   device.tool("delete_home_automation", {"id": "automation_des_nutzers"}), "Error")
    # alexa without side effects: only calls that stop before anything reaches an Echo.
    suite.contains("alexa without a device names the Echos",
                   device.tool("alexa", {"device": "", "action": "command", "text": "Pause"}, 45), "Error", "Echo")
    suite.contains("alexa with an unknown device", device.tool("alexa", {"device": "Regressionstest 4711",
                                                                        "action": "speak", "text": "x"}, 45),
                   "no device")
    suite.contains("alexa with a wrong action", device.tool("alexa", {"device": "Echo Wohnzimmer", "action": "sing",
                                                                      "text": "x"}, 45), "Error: action")
    # fire_tv without side effects: the read-only status and calls that stop before a stick.
    suite.contains("fire_tv status lists the sticks",
                   device.tool("fire_tv", {"device": "", "action": "status", "text": "", "repeat": 1}, 45), "Fire TV")
    suite.contains("fire_tv without a device names the sticks",
                   device.tool("fire_tv", {"device": "", "action": "key", "text": "home", "repeat": 1}, 45),
                   "Error", "Fire TV")
    suite.contains("fire_tv with an unknown device",
                   device.tool("fire_tv", {"device": "Regressionstest 4711", "action": "on", "text": "",
                                           "repeat": 1}, 45), "no Fire TV")
    suite.contains("fire_tv with a wrong action",
                   device.tool("fire_tv", {"device": "", "action": "sing", "text": "x", "repeat": 1}, 45),
                   "Error: action")
    suite.contains("broken config refused",
                   device.tool("create_home_automation", {"alias": "x", "description": "x", "config": "{"}), "Error")
    # quick home commands (voice/quick_home, #75): the rules, and HA's Assist with a room that does not exist
    quick = device.command("Q", "[Quick test] HA:", 15)
    suite.contains("quick home rules", quick, "[Quick test] ok")
    suite.contains("HA Assist answers (unknown room, nothing switched)", quick, "[Quick test] HA: error no_valid_targets")


def network(suite, device):
    """A member of a network of devices (docs/architecture.md, A network of devices): the leader's
    tools and context, and the emergency memory while it is away (serial O). Writes one test task,
    which ends on the leader and is deleted there."""
    print("Network of devices")
    link = device.command("Y", "journal", 15)
    suite.check("a leader is present", "[Shared] leader " in link and "leader none" not in link, link[-300:])
    suite.contains("the context comes from the leader", link, "context from the leader")
    start = len(device.log)
    device.tool("list_tasks")
    suite.check("list_tasks runs on the leader", any("list_tasks ran on the leader" in l for l in device.log[start:]))
    away = device.command("O", "(test)")
    try:
        suite.contains("leader away (test)", away, "leader away")
        made = device.tool("create_task", {"title": "Regressionstest 4711", "kind": "reminder", "text": "x",
                                           "schedule": "once", "date": TEST_DAY, "time": "09:00"}) or ""
        suite.contains("without a leader a task is kept here", made, "Created: #", "not reachable")
        suite.contains("the instructions name what is pending", device.command("Y", "journal", 15),
                       "Not yet passed on", "Regressionstest 4711")
    finally:
        back = device.command("O", "(test)")
    suite.contains("leader back (test)", back, "leader back")
    passed = next((l for l in device.lines(40) if "passed on to the leader" in l), None)
    suite.check("the journal goes to the leader when it is back", passed is not None)
    suite.contains("the task is on the leader now", device.tool("list_tasks"), "Regressionstest 4711")
    suite.contains("the journal is empty", device.command("Y", "journal", 15), "journal empty")
    for task in re.findall(r"#(\d+) Regressionstest", device.tool("list_tasks") or ""):
        device.tool("delete_task", {"id": int(task)})


def various(suite, device, member=False):
    print("Various")
    suite.contains("set_volume reports the volume", device.tool("set_volume", {"step": 0}), '"volume":')
    suite.contains("send_last_image checks the delivery",
                   device.tool("send_last_image", {"print": False, "email_to": ""}), "Error")
    suite.contains("unknown tool", device.tool("gibt_es_nicht"), "unknown function")
    # Nothing is dialled in these: the checks refuse before. A second speaker (JARVIS_INBOUND 0)
    # has no phone line at all and refuses every call.
    called = device.tool("phone_call", {"to": "Niemand 4711", "task": "Regressionstest, nicht anrufen"}) or ""
    second_speaker = "no phone line" in called
    if second_speaker:
        suite.check("phone_call refused on a second speaker", "the main Jarvis" in called, called[:160])
    else:
        suite.contains("phone_call: an unknown name is no number", called, "not a person of the household")
    # A test child. SAFETY: phone_call below would really dial if the person were not a child, so it
    # only runs when update_person confirmed the child flag (add_person has none: on 01.10. an
    # unconfirmed "child" let a test call ring out). A member has no line and its people are the
    # leader's: nothing to register.
    if not member:
        children(suite, device, second_speaker)
    files_and_more(suite, device)


def children(suite, device, second_speaker):
    device.tool("add_person", {"name": CHILD, "phones": [], "telegram": 0, "email": ""})
    made = device.tool("update_person", {"person": CHILD, "name": CHILD, "phones": [], "telegram": 0, "email": "",
                                         "blocked": [], "child": True}) or ""
    child = re.search(r"\(id ([^,)]+)", made)
    if second_speaker:
        pass  # no phone line: nothing to check about children's calls
    elif child and "child (child-friendly)" in made:
        suite.contains("phone_call: outside the household not for children",
                       device.tool("phone_call", {"to": "07161 123456", "task": "Regressionstest, nicht anrufen"},
                                   person=child.group(1)), "children may not")
    else:
        suite.check("test child registered as a child (phone_call skipped, nothing dialled)", False, made[:160])
    device.tool("remove_person", {"person": CHILD})


def files_and_more(suite, device):
    # Files: nothing leaves the device in these (unknown file, CSV to the printer is refused).
    suite.check("list_files answers", (device.tool("list_files") or "").startswith(("Stored files", "No stored files")))
    suite.contains("send_file: unknown stored file refused",
                   device.tool("send_file", {"file": "gibt-es-nicht-4711.pdf", "name": "", "content": "", "via": "telegram",
                                             "to": "", "text": ""}), "Error: no stored file")
    suite.contains("send_file: the printer takes no CSV",
                   device.tool("send_file", {"file": "", "name": "regressionstest.csv", "content": "a;b\n1;2\n",
                                             "via": "print", "to": "", "text": ""}), "Error: the printer")
    suite.contains("get_presence reads the presence from Home Assistant", device.tool("get_presence", {}, 45),
                   "Presence according to Home Assistant")
    # The amplifier loses its setup (as after a brown-out): board::checkAmplifier sets it up again.
    device.drain(0.2)
    device.send("V")
    recovered = next((l for l in device.lines(12) if "amplifier ready again" in l), None)
    suite.check("amplifier set up again after a failure", recovered is not None)
    # phone_call must never dial emergency, premium or foreign numbers (nothing is dialled here).
    for number in ["110", "112", "09001234567", "0137123456", "0044201234567"]:
        suite.contains(f"phone_call blocks {number}",
                       device.tool("phone_call", {"to": number, "task": "Regressionstest, nicht anrufen"}),
                       "Error")


TEST_DAY = "2030-01-01"  # the calendar test's event lives here, far from real ones


def google(suite, device):
    print("Google (calendar, contacts)")
    tools = [t.get("name") for t in device.dump_tools() or []]
    if "list_events" not in tools:
        suite.check("Google logged in (GOOGLE_* in secrets.h, tools/google_auth.py)", False)
        return
    today = device.tool("list_events", {"from": "", "to": ""}, 45)
    suite.check("list_events today", today is not None and "events" in today.lower(), str(today)[:160])
    suite.contains("find_contact finds contacts", device.tool("find_contact", {"name": "a"}, 45), "Contacts for", "- ")
    suite.contains("find_contact without a hit", device.tool("find_contact", {"name": "Xyzzy Regressionstest"}, 45),
                   "No contact")
    contact = device.tool("save_contact", {"id": "", "name": "Regressionstest 4711", "phone": "0171 0000000"}, 45)
    suite.contains("save_contact creates", contact, "Contact created", "0171 0000000", "[id people/")
    match = re.search(r"\[id (people/\S+)\]", contact or "")
    if match:
        suite.contains("save_contact adds to it",
                       device.tool("save_contact", {"id": match.group(1), "name": "", "email": "test@example.com"}, 45),
                       "Contact updated", "0171 0000000", "test@example.com")
        suite.contains("test contact deleted", device.tool("delete_contact", {"id": match.group(1)}, 45), "Deleted")
    suite.contains("save_contact without anything refused",
                   device.tool("save_contact", {"id": "", "name": ""}), "Error")
    suite.contains("caller_name does not know a stranger's number",
                   device.tool("caller_name", {"number": "0301234567890"}), "unknown")
    birthdays = device.tool("upcoming_birthdays", {"days": 366}, 60)
    suite.contains("upcoming_birthdays", birthdays, "Birthdays in the next 366 days:", "(turns ")
    suite.contains("find_free_time: bad window refused",
                   device.tool("find_free_time", {"from": "", "to": "", "duration_minutes": 60, "earliest": "20:00",
                                                  "latest": "08:00"}), "Error")
    suite.contains("invalid date refused",
                   device.tool("create_event", {"title": "Regressionstest", "date": "2030-02-31", "time": "10:00"}), "Error")
    made = device.tool("create_event", {"title": "Regressionstest 4711", "date": TEST_DAY, "time": "12:00",
                                        "duration_minutes": 30, "location": "Testort"}, 45)
    suite.contains("create_event creates", made, "Created", "Di 01.01.2030 12:00–12:30", "Regressionstest 4711",
                   "place: Testort")
    match = re.search(r"\[id (\S+)\]", made or "")
    if not match:
        return
    event = match.group(1)
    suite.contains("list_events finds it", device.tool("list_events", {"from": TEST_DAY, "to": TEST_DAY,
                                                                         "query": "Regressionstest 4711"}, 45), event)
    suite.contains("update_event moves and keeps the duration",
                   device.tool("update_event", {"id": event, "time": "15:00"}, 45), "Changed", "15:00–15:30")
    suite.contains("find_free_time leaves the test event out",
                   device.tool("find_free_time", {"from": TEST_DAY, "to": TEST_DAY, "duration_minutes": 60}, 45),
                   "Free time", "08:00–15:00", "15:30–20:00")
    suite.contains("update_event makes it all day",
                   device.tool("update_event", {"id": event, "time": "all_day", "end_date": "2030-01-02"}, 45),
                   "Di 01.01.2030 to Mi 02.01.2030 all day")
    # "" means unchanged (the backend fills what it does not change, 24.10.2026): the title stays
    suite.contains("update_event keeps what is empty",
                   device.tool("update_event", {"id": event, "title": "", "location": "", "description": "",
                                                "date": "", "time": "", "end_date": "", "duration_minutes": 0}, 45),
                   "Error: nothing to change")
    suite.contains("update_event without a change refused", device.tool("update_event", {"id": event}, 45), "Error")
    suite.contains("delete_event deletes", device.tool("delete_event", {"id": event}, 45), "Deleted", "Regressionstest 4711")
    suite.contains("deleted event refused", device.tool("delete_event", {"id": event}, 45), "Error")


def todos(suite, device):
    print("To-do lists (Google Tasks)")
    tools = [t.get("name") for t in device.dump_tools() or []]
    if "list_todos" not in tools:
        suite.check("Google logged in (list_todos registered)", False)
        return
    suite.contains("list_todos reads all lists", device.tool("list_todos", {"list": ""}, 45), "List \"")
    suite.contains("list_todos: unknown list refused",
                   device.tool("list_todos", {"list": "Regressionstest-Liste-4711"}, 45), "Error: no list")
    suite.contains("add_todo: bad due refused",
                   device.tool("add_todo", {"title": "Regressionstest", "list": "", "due": "morgen"}, 45), "Error")
    made = device.tool("add_todo", {"title": "Regressionstest 4711", "list": "", "due": "2030-01-01"}, 45)
    suite.contains("add_todo adds", made, "Added to", "Regressionstest 4711", "due 01.01.2030")
    match = re.search(r"\[id (\S+)\]", made or "")
    if not match:
        return
    task = match.group(1)
    suite.contains("change_todo completes", device.tool("change_todo", {"id": task, "action": "complete"}, 45),
                   "Done:", "| done")
    suite.contains("change_todo reopens", device.tool("change_todo", {"id": task, "action": "reopen"}, 45), "Open again")
    suite.contains("change_todo edits", device.tool("change_todo", {"id": task, "action": "edit",
                                                                    "title": "Regressionstest 4712", "due": ""}, 45),
                   "Changed:", "Regressionstest 4712")
    suite.contains("change_todo deletes", device.tool("change_todo", {"id": task, "action": "delete"}, 45), "Deleted")
    suite.contains("deleted task refused", device.tool("change_todo", {"id": task, "action": "complete"}, 45), "Error")


def spotify(suite, device):
    print("Spotify (read only: nothing is played or changed)")
    tools = [t.get("name") for t in device.dump_tools() or []]
    if "play_music" not in tools:
        suite.check("Spotify logged in (play_music registered)", False)
        return
    suite.contains("music_speakers lists the devices", device.tool("music_speakers", {"action": "list"}, 45),
                   "Speakers Spotify sees")
    status = device.tool("music_control", {"action": "status"}, 45)
    suite.check("music_control status", status and not status.startswith("Error"), str(status)[:160])
    suite.contains("music_playlist lists own and followed", device.tool("music_playlist", {"action": "list"}, 45),
                   "Own playlists", "Followed")
    suite.contains("music_playlist: unknown playlist refused",
                   device.tool("music_playlist", {"action": "show", "playlist": "Regressionstest 4711"}, 45), "Error")
    suite.contains("music_insights top tracks", device.tool("music_insights", {"action": "top_tracks", "period": "4_weeks"}, 45),
                   "Top songs")
    suite.contains("music_insights recent", device.tool("music_insights", {"action": "recent"}, 45), "Recently played")
    suite.contains("music_insights artist albums",
                   device.tool("music_insights", {"action": "artist_albums", "what": "Daft Punk"}, 45), "Daft Punk")
    suite.contains("music_library check", device.tool("music_library", {"action": "check", "type": "track",
                                                                        "what": "Daft Punk Get Lucky"}, 45), "library")
    suite.contains("play_music: unknown speaker refused (nothing played)",
                   device.tool("play_music", {"what": "Daft Punk Get Lucky", "type": "track",
                                              "speaker": "Regressionstest 4711"}, 45), "Error", "Visible")
    suite.contains("play_music: unknown account refused",
                   device.tool("play_music", {"what": "x", "type": "track", "speaker": "",
                                              "account": "Regressionstest 4711"}, 45), "Error")


def sheets(suite, device):
    print("Tables (Google Sheets)")
    tools = [t.get("name") for t in device.dump_tools() or []]
    if "read_sheet" not in tools:
        suite.check("Google logged in (read_sheet registered)", False)
        return
    name = "Regressionstest 4711"
    suite.contains("add_sheet_row without columns refused",
                   device.tool("add_sheet_row", {"sheet": name, "values": [1], "columns": []}, 45), "Error")
    suite.contains("add_sheet_row makes the table", device.tool(
        "add_sheet_row", {"sheet": name, "values": ["30.09.2026", 42.5], "columns": ["Datum", "Liter"]}, 60),
        "New sheet", "Row 2 added", "docs.google.com/spreadsheets")
    suite.contains("add_sheet_row appends", device.tool("add_sheet_row", {"sheet": name, "values": ["01.10.2026", 38],
                                                                          "columns": []}, 45), "Row 3 added")
    suite.contains("update_sheet_row replaces", device.tool("update_sheet_row", {"sheet": name, "row": 2,
                                                                                 "values": ["30.09.2026", 40]}, 45),
                   "Row 2", "is now")
    suite.contains("read_sheet reads", device.tool("read_sheet", {"sheet": name}, 45),
                   "row 1 (header): Datum | Liter", "row 2: 30.09.2026 | 40", "row 3: 01.10.2026 | 38")
    suite.contains("read_sheet lists", device.tool("read_sheet", {"sheet": ""}, 45), name)
    suite.contains("test table trashed", device.tool("delete_sheet", {"sheet": name}, 45), "trashed")


def backups(suite, device):
    print("Backups (Drive)")
    result = device.tool("backup_check", {}, 60)
    suite.contains("all backups readable", result, "core.json: ", "people.json: ", "tasks.json: ")
    suite.check("no backup missing or broken", result is not None and "Error" not in result and "none" not in result,
                str(result)[:300])


def search(suite, device):
    print("Web search (Tavily, monthly quota)")
    tools = device.dump_tools() or []
    if not any(t.get("name") == "web_search" for t in tools):
        suite.check("Tavily key set (TAVILY_API_KEY in secrets.h; else OpenAI's hosted search)", False)
        return
    suite.contains("web_search without a query refused", device.tool("web_search", {"query": " "}), "Error")
    before = device.tool("maps_usage")
    found = device.tool("web_search", {"query": "Wetterdienst Deutschland Startseite"}, 45)
    suite.contains("web_search finds sources", found, "Web search for", "Sources:", "http")
    after = device.tool("maps_usage")

    def searches(usage):
        match = re.search(r'"tavily":(\d+)', usage or "")
        return int(match.group(1)) if match else 0
    suite.check("the search was counted", searches(after) == searches(before) + 1, f"{before} -> {after}")


MCP_SERVER = "Regressionstest MCP"
MCP_URL = "https://mcp.deepwiki.com/mcp"  # public, free, no login (Streamable HTTP, answers as SSE)


def mcp(suite, device):
    print("MCP servers (DeepWiki, public)")
    suite.contains("bad url refused", device.tool("connect_mcp_server", {"name": MCP_SERVER, "url": "ftp://x"}), "Error")
    made = device.tool("connect_mcp_server", {"name": MCP_SERVER, "url": MCP_URL, "description": "Doku zu GitHub-Projekten",
                                              "only_tools": ["read_wiki_structure", "ask_wiki_question"]}, 60)
    if not suite.contains("connect_mcp_server loads the tools", made, "Connected " + MCP_SERVER + " with 2 tools",
                          "regressionstest_mcp_read_wiki_structure"):
        return
    tools = device.dump_tools() or []
    offered = [t for t in tools if t.get("name", "").startswith("regressionstest_mcp_")]
    suite.check("tools offered to the model with schema", len(offered) == 2 and
                all(t["parameters"].get("type") == "object" and t["description"].startswith("[" + MCP_SERVER + "]")
                    for t in offered), [t.get("name") for t in offered])
    listed = device.tool("list_mcp_servers")
    suite.contains("list_mcp_servers shows host, not the url", listed, "mcp.deepwiki.com", "connected")
    suite.check("list_mcp_servers hides the path", listed is not None and "deepwiki.com/mcp" not in listed)
    found = device.tool("regressionstest_mcp_read_wiki_structure", {"repoName": "bblanchon/ArduinoJson"}, 60)
    suite.check("a tool call returns the server's answer", found is not None and not found.startswith("Error") and
                len(found) > 50, (found or "")[:160])
    device.tool("add_person", {"name": PERSON, "phones": ["+49 170 4711471"], "telegram": 0, "email": ""})
    suite.contains("a private server's tools are the admin's only",
                   device.tool("regressionstest_mcp_read_wiki_structure", {"repoName": "bblanchon/ArduinoJson"},
                               person="regressionstest"), f"only {OWNER}")
    device.tool("remove_person", {"person": PERSON})
    suite.contains("disconnect_mcp_server", device.tool("disconnect_mcp_server", {"name": MCP_SERVER}), "Disconnected")
    tools = device.dump_tools() or []
    suite.check("its tools are gone", not any(t.get("name", "").startswith("regressionstest_mcp_") for t in tools))


def maps(suite, device):
    print("Google Maps (Places, monthly quota)")
    tools = [t.get("name") for t in device.dump_tools() or []]
    if "find_place" not in tools:
        suite.check("Maps key set (GOOGLE_MAPS_API_KEY in secrets.h)", False)
        return
    suite.contains("find_place without a query refused", device.tool("find_place", {"query": " "}), "Error")
    before = device.tool("maps_usage")
    found = device.tool("find_place", {"query": "Rathaus"}, 45)
    suite.contains("find_place finds places", found, "Places for \"Rathaus\"", "- ", " | ")
    after = device.tool("maps_usage")

    def places(usage):
        match = re.search(r'"places":(\d+)', usage or "")
        return int(match.group(1)) if match else 0
    suite.check("the search was counted", places(after) == places(before) + 1, f"{before} -> {after}")

    suite.contains("travel_time: home to home refused",
                   device.tool("travel_time", {"destination": "", "mode": "drive"}), "Error")
    suite.contains("travel_time: unknown mode refused",
                   device.tool("travel_time", {"destination": "Stuttgart", "mode": "boat"}), "Error")
    suite.contains("travel_time: past time refused",
                   device.tool("travel_time", {"destination": "Stuttgart", "mode": "drive", "date": "2026-01-01",
                                               "time": "08:00"}), "Error")
    suite.contains("travel_time by car", device.tool("travel_time", {"destination": "Stuttgart Hauptbahnhof",
                                                                     "mode": "drive"}, 45),
                   "By car from home to Stuttgart Hauptbahnhof", " km", "arrival about")
    suite.contains("travel_time by public transport",
                   device.tool("travel_time", {"destination": "Stuttgart Hauptbahnhof", "mode": "transit"}, 45),
                   "By public transport", " from ", "arrives")
    suite.contains("routes counted", device.tool("maps_usage"), '"routes":')

    suite.contains("get_weather without include refused", device.tool("get_weather", {"include": []}), "Error")
    weather = device.tool("get_weather", {"include": ["now", "hours", "days", "alerts", "pollen", "air"],
                                          "hours": 3, "days": 2}, 60)
    suite.contains("get_weather: all parts for home", weather, "Weather for home", "Now: ", " °C", "Hourly:",
                   "Daily:", "sunrise", "Official weather warnings", "Pollen", "Air quality:")
    suite.check("get_weather: no part failed", weather is not None and "Error" not in weather, str(weather)[:300])
    suite.contains("get_weather for another place", device.tool("get_weather", {"include": ["now"], "place": "Hamburg"}, 45),
                   "Weather for ", "Hamburg", "Now: ")
    suite.contains("find_video finds videos", device.tool("find_video", {"query": "Sendung mit der Maus"}, 45),
                   "Videos for", "youtube.com/watch?v=", "[video_id ")
    if "summarize_video" in tools:
        suite.contains("summarize_video: Gemini tells what a video says",
                       device.tool("summarize_video", {"video": "https://youtu.be/gieP0jUwIf8?si=test",
                                                       "question": ""}, 120),
                       "About the video gieP0jUwIf8", "Gemini watched it")
    suite.contains("control_video: unknown action refused",
                   device.tool("control_video", {"tv": "x", "action": "rewind"}), "Error")
    suite.contains("play_video lists the TVs for an unknown one (plays nothing)",
                   device.tool("play_video", {"video_id": "x", "tv": "Regressionstest-TV"}, 45), "Error: no TV", "TVs:")
    suite.contains("weather, pollen and air counted", device.tool("maps_usage"), '"weather":', '"pollen":', '"air":')


def mail(suite, device, send):
    print("Mail")
    inbox = device.tool("list_emails", {"unread_only": False, "folder": "inbox"}, 45)
    suite.check("list_emails (inbox)", inbox is not None and (inbox.startswith("[") or "empty" in inbox),
                str(inbox)[:120])
    subject = None
    if send and not OWNER_EMAIL:
        suite.check("--mail needs OWNER_EMAIL in secrets.h", False)
    elif send:
        subject = f"Regressionstest {time.strftime('%H:%M:%S')}"
        sent = device.tool("send_email", {"to": OWNER_EMAIL, "subject": subject,
                                               "html": "<p>Regressionstest von Jarvis (äöü).</p>"})
        suite.contains("send_email", sent, "sent")
    listing = device.tool("list_emails", {"unread_only": False, "folder": "sent"}, 45)
    suite.check("list_emails (sent)", listing is not None and (listing.startswith("[") or "No " in listing),
                str(listing)[:120])
    if subject:
        suite.contains("copy in the sent folder", listing, subject)
    uids = re.findall(r'"uid":(\d+)', listing or "")
    if uids:
        suite.contains("read_email (sent)", device.tool("read_email", {"uid": int(uids[0]), "folder": "sent"}, 45),
                       "Jarvis sent")


def mailsort(suite, device):
    """Folders and rules of the watched mailboxes; moves no mail (a rule for a subject nobody sends)."""
    print("Mail folders and rules")
    tools = [t.get("name") for t in device.dump_tools() or []]
    if "sort_mail" not in tools:
        suite.check("sort_mail set up (WATCHED_MAILBOXES in secrets.h)", False)
        return
    folders = device.tool("sort_mail", {"mailbox": "", "action": "folders", "folder": "", "refs": [], "category": "",
                                        "days": 1}, 90)
    suite.contains("sort_mail folders", folders, "Folders of")
    rule = {"action": "add", "mailbox": "", "category": "", "from": "", "subject": "Regressionstest-Regel",
            "target": "Regressionstest", "id": 0}
    added = device.tool("mail_rule", rule)
    suite.contains("mail_rule add", added, "Saved rule")
    number = re.search(r"rule (\d+):", added or "")
    listing = device.tool("mail_rule", dict(rule, action="list"))
    suite.contains("mail_rule list", listing, "Regressionstest-Regel")
    if number:
        suite.contains("mail_rule delete", device.tool("mail_rule", dict(rule, action="delete", id=int(number.group(1)))),
                       "Deleted rule")
    suite.contains("mail_rule needs a condition", device.tool("mail_rule", dict(rule, subject="")), "needs a category")


def telegram(suite, device):
    print("Telegram")
    tools = [t.get("name") for t in device.dump_tools() or []]
    if "send_telegram" not in tools:
        suite.check("send_telegram set up (TELEGRAM_BOT_TOKEN / TELEGRAM_OWNER_ID in secrets.h)", False)
        return
    sent = device.tool("send_telegram", {"text": f"<b>Regressionstest</b> {time.strftime('%H:%M:%S')} "
                                                 "(äöü, <a href=\"https://example.com\">Link</a>)"}, 45)
    suite.contains("send_telegram", sent, "sent")
    # Sources of the web search come as Markdown links: they must become valid HTML links.
    before = len(device.log)
    sent = device.tool("send_telegram", {"text": "Sonnenaufgang 7:16 Uhr. ([timeanddate.de]"
                                                 "(https://www.timeanddate.de/sonne/?month=9&year=2026))"}, 45)
    suite.contains("send_telegram with a Markdown source", sent, "sent")
    suite.check("Markdown source accepted as an HTML link",
                not any("HTML refused" in line for line in device.log[before:]))
    sent = device.tool("send_file", {"file": "", "name": "regressionstest.csv", "content": "Monat;Wert\nJuli;1\n",
                                     "via": "telegram", "to": "", "text": "Regressionstest (CSV)"}, 60)
    suite.contains("send_file: CSV as a Telegram document", sent, "Sent regressionstest.csv")
    # Broken markup: Telegram rejects it, the assistant sends it as plain text instead.
    suite.contains("send_telegram without valid HTML", device.tool("send_telegram", {"text": "Test <b>kaputt & offen"}, 45),
                   "sent")


def live(suite, device):
    print("GPT Live")
    device.drain(0.5)
    device.send("s")
    size = None
    for line in device.lines(25):
        match = re.search(r"\[Session\] \S+ \((\d+) bytes\)", line)
        if match:
            size = int(match.group(1))
            break
    suite.check("session starts", size is not None)
    if size:
        # The WebSockets library drops frames above 256 KB (scripts/patch_libraries.py): warn early.
        suite.check(f"session.started {size} bytes below 200 KB", size < 200000)
    time.sleep(2)
    device.send("s")
    suite.check("session ends", next((l for l in device.lines(15) if "Session ended" in l), None) is not None)


def mqtt_publish(topic, payload):
    """One QoS 0 message to the broker of secrets.h (MQTT_URL, MQTT_USER, MQTT_PASSWORD)."""
    import socket
    url = secret("MQTT_URL")
    host, _, port = url[len("mqtt://"):].partition(":")

    def text(s):
        b = s.encode()
        return len(b).to_bytes(2, "big") + b

    def packet(kind, body):
        n, length = len(body), b""
        while True:
            digit, n = n % 128, n // 128
            length += bytes([digit | (0x80 if n else 0)])
            if not n:
                return bytes([kind]) + length + body

    login = text("jarvis_test") + text(secret("MQTT_USER")) + text(secret("MQTT_PASSWORD"))
    with socket.create_connection((host, int(port or 1883)), 5) as s:
        s.sendall(packet(0x10, text("MQTT") + bytes([4, 0xC2, 0, 30]) + login))
        if s.recv(4)[-1:] != b"\x00":
            raise RuntimeError("broker refused the login")
        s.sendall(packet(0x30, text(topic) + payload.encode()))
        s.sendall(bytes([0xE0, 0]))


def ha_device(suite, device):
    """This speaker as a Home Assistant device over MQTT (services/ha_device, #103): connected,
    the states sent, and a command from the broker arrives (the volume set to what it is)."""
    print("Home Assistant device (MQTT)")
    status = device.command("J", "version", 10)
    suite.contains("connected to the broker", status, "[HA device] connected")
    found = re.search(r"id (jarvis_[0-9a-f]{12})", status)
    suite.check("the id is the WiFi MAC", found is not None, status[-200:])
    volume = re.search(r"volume = (\d+)", device.command("J", "started", 10))
    suite.check("the states were sent", volume is not None)
    if not (found and volume):
        return
    start = len(device.log)
    mqtt_publish("jarvis/%s/volume/set" % found.group(1), volume.group(1))
    heard = next((l for l in device.lines(10) if "[HA device] volume:" in l), None)
    suite.check("a command from HA arrives", heard is not None, "\n".join(device.log[start:])[-300:])


def main():
    parser = argparse.ArgumentParser(description=__doc__, formatter_class=argparse.RawDescriptionHelpFormatter)
    parser.add_argument("--port", default="COM5")
    parser.add_argument("--mail", action="store_true", help="send a test mail to the owner")
    parser.add_argument("--live", action="store_true", help="open a short GPT Live session (~1 ct)")
    parser.add_argument("--telegram", action="store_true", help="send test messages to the owner's Telegram chat")
    parser.add_argument("--maps", action="store_true", help="one Places search (counts against the monthly limit)")
    parser.add_argument("--search", action="store_true", help="one Tavily web search (of 1000 free a month)")
    parser.add_argument("--mcp", action="store_true", help="connect DeepWiki's public MCP server and call one tool")
    parser.add_argument("--update-golden", action="store_true", help="accept the current tool definitions")
    parser.add_argument("--only", help="comma separated: contract,people,tasks,memory,skills,ha,hadevice,various,network,google,todos,sheets,spotify,backups,maps,search,mcp,mail,mailsort,telegram,live")
    args = parser.parse_args()

    device = Device(args.port)
    suite = Suite(device)
    member = device.member()
    groups = {
        "contract": lambda: contract(suite, device, args.update_golden),
        "people": lambda: people(suite, device),
        "tasks": lambda: tasks(suite, device),
        "memory": lambda: memory(suite, device),
        "skills": lambda: skills(suite, device),
        "ha": lambda: home_assistant(suite, device),
        "hadevice": lambda: ha_device(suite, device),
        "various": lambda: various(suite, device, member),
        "network": lambda: network(suite, device),
        "google": lambda: google(suite, device),
        "todos": lambda: todos(suite, device),
        "sheets": lambda: sheets(suite, device),
        "spotify": lambda: spotify(suite, device),
        "backups": lambda: backups(suite, device),
        "maps": lambda: maps(suite, device),
        "search": lambda: search(suite, device),
        "mcp": lambda: mcp(suite, device),
        "mail":lambda: mail(suite, device, args.mail),
        "mailsort": lambda: mailsort(suite, device),
        "telegram": lambda: telegram(suite, device),
        "live": lambda: live(suite, device),
    }
    optional = {"live": args.live, "telegram": args.telegram, "maps": args.maps, "search": args.search, "mcp": args.mcp}
    # A member of a network of devices: people, tasks, memory and skills are the leader's (an Echo,
    # tested there); the network's own tests instead.
    leaders = ["people", "tasks", "memory", "skills"]
    optional["network"] = member
    selected = args.only.split(",") if args.only else [g for g in groups if optional.get(g, True)
                                                        and not (member and g in leaders)]
    if member:
        print("Member of a network of devices: people, tasks, memory and skills are the leader's (not tested here)")
    started = time.time()
    cleanup(device)
    for group in selected:
        groups[group]()
    (HERE / "last_run.log").write_text("\n".join(device.log), encoding="utf-8")  # the device's log of this run
    crashes = [l for l in device.log if CRASH.search(l)]
    suite.check("no crash during the tests", not crashes, crashes[0] if crashes else "")

    failed = [r for r in suite.results if not r[1]]
    print(f"\n{len(suite.results) - len(failed)}/{len(suite.results)} passed in {time.time() - started:.0f} s")
    sys.exit(1 if failed else 0)


if __name__ == "__main__":
    main()
