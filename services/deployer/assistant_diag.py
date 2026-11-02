#!/usr/bin/env python3
"""Read-only diagnostics of every Echo for the server agent (#124), on the Raspberry Pi.

The server agent's key on the Pi (user agent) is bound to the command assistant-diag, which runs this as
assistant-deploy (the user with the speakers' keys) with the command the agent asked for. Only the
commands below exist, each a fixed command line on the speaker; nothing here writes, restarts or
speaks. Usage over SSH:

    ssh assistant-diag help
    ssh assistant-diag "echo2 log 300"
    ssh assistant-diag "all status"

Offline test: python3 test_assistant_diag.py
"""
import json
import re
import shlex
import subprocess
import sys
from pathlib import Path

DEVICES = Path("/opt/assistant-deployer/local/devices")  # one <host>.json per speaker
MAX_LINES = 5000
PATTERN = re.compile(r"^[\w .:=/+,#@()\[\]-]{1,80}$")  # a grep pattern, nothing a shell would read

# Per board: the binary and the logs (devices/echo/docs/16-assistant.md, hardware/dot2/README.md)
BOARDS = {
    "echo": {"assistant": "/usr/local/echo-dev/assistant/assistant", "log": "/mnt/echo-net/assistant.log",
             "health": "/mnt/data/assistant/health.log",
             "settings": "/usr/local/echo-dev/assistant/settings.json /usr/local/echo-dev/assistant/device.json"},
    "dot": {"assistant": "/data/echo-dot-dev/assistant/assistant", "log": "/run/echo-dot-dev/assistant.sh.log",
            "health": "", "settings": "/data/echo-dot-dev/assistant/device.json"},
}

HELP = """Read-only diagnostics of the assistant's Echos. Usage: <host|all> <command> [args]
hosts                      the speakers
<host> status              ctl status: version, state, volume, mic level, clock
<host> log [N]             the last N lines of the assistant's log (default 200, at most 5000)
<host> grep TEXT [N]       the last N log lines containing TEXT (fixed string)
<host> errors              ctl errors: starts, failed conversations, self-test
<host> health [N]          the minute-by-minute load, memory and network log (Echo Gen 1)
<host> sys                 uptime, memory, disks, the biggest processes
<host> dmesg [N]           the kernel's last N lines
<host> tools               ctl tools: the tools the assistant offers
<host> peers               ctl peers: the other assistant devices it sees
<host> settings            settings.json / device.json (no secrets)
all <command>              the command on every speaker"""


def hosts(devices=DEVICES):
    return sorted(p.stem for p in devices.glob("*.json"))


def count(args, default):
    if not args:
        return default
    if not args[0].isdigit():
        raise ValueError(f"not a line count: {args[0]}")
    return max(1, min(int(args[0]), MAX_LINES))


def remote_command(board, cmd, args):
    """The fixed command line on the speaker for cmd, or ValueError."""
    b = BOARDS[board]
    j = b["assistant"]
    if cmd in ("status", "errors", "tools", "peers"):
        if args:
            raise ValueError(f"{cmd} takes no arguments")
        return f"{j} ctl {cmd}"
    if cmd == "log":
        return f"tail -n {count(args, 200)} {b['log']}"
    if cmd == "grep":
        if not args or not PATTERN.match(args[0]):
            raise ValueError("grep needs a plain TEXT (letters, digits, spaces, .:=/+,#@()[]- ; at most 80)")
        return f"grep -F -- {shlex.quote(args[0])} {b['log']} | tail -n {count(args[1:], 200)}"
    if cmd == "health":
        if not b["health"]:
            raise ValueError("the Dot keeps no health log")
        return f"tail -n {count(args, 60)} {b['health']}"
    if cmd == "sys":
        return "uptime; free; df -h; ps -o pid,rss,vsz,comm 2>/dev/null | sort -k2 -n -r | head -15 || ps | head -20"
    if cmd == "dmesg":
        return f"dmesg | tail -n {count(args, 100)}"
    if cmd == "settings":
        return f"for f in {b['settings']}; do echo \"== $f\"; cat \"$f\" 2>/dev/null; done"
    raise ValueError(f"unknown command: {cmd} (try help)")


def ssh(host, command, timeout=60):
    return subprocess.run(["ssh", "-o", "BatchMode=yes", host, command], capture_output=True,
                          text=True, timeout=timeout)


def board_of(host, run=ssh):
    return "dot" if run(host, "test -f /etc/echo-dot-dev-release").returncode == 0 else "echo"


def main(request, run=ssh, devices=DEVICES, out=sys.stdout):
    try:
        words = shlex.split(request or "help")
    except ValueError as e:
        print(f"cannot read the command: {e}", file=out)
        return 2
    known = hosts(devices)
    if words[0] in ("help", "-h", "--help"):
        print(HELP, file=out)
        return 0
    if words[0] == "hosts":
        print("\n".join(known), file=out)
        return 0
    if len(words) < 2:
        print("usage: <host|all> <command> [args] (try help)", file=out)
        return 2
    targets = known if words[0] == "all" else [words[0]]
    if not set(targets) <= set(known):
        print(f"unknown host: {words[0]} (hosts: {', '.join(known)})", file=out)
        return 2
    status = 0
    for host in targets:
        try:
            command = remote_command(board_of(host, run), words[1], words[2:])
        except ValueError as e:
            print(str(e), file=out)
            return 2
        r = run(host, command)
        if len(targets) > 1:
            print(f"== {host}", file=out)
        print((r.stdout + r.stderr).rstrip(), file=out)
        status = status or r.returncode
    return status


if __name__ == "__main__":
    sys.exit(main(" ".join(sys.argv[1:])))
