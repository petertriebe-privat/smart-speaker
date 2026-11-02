# OpenHands on the server

Files for the agent on the server (Ultron, issues #46, #124). The live copies are there; these
are the versions to compare against and to restore.

| Here | On the server | What |
|---|---|---|
| `jarvis/SKILL.md` | `/opt/openhands/codex/skills/jarvis/SKILL.md` and `/opt/openhands/claude/skills/jarvis/SKILL.md` | Skill for both profiles (Codex, Claude Code): read every Echo with `ssh jarvis-diag`, find the cause, fix on a branch, open a pull request; never merge or deploy. |

Not here (they belong to the server, not to Jarvis): the agents' global instructions
(`/opt/openhands/codex/AGENTS.md`, `/opt/openhands/claude/CLAUDE.md`), which since #124 say that
Jarvis changes only through a pull request the owner merges; the container's SSH config (alias
`jarvis-diag`: the Raspberry Pi over WireGuard, jump host `host`) and its key
`ultron_diag_ed25519`, which the Pi binds to the read-only diagnostics
(`services/deployer/jarvis_diag.py`). The server has no other access to the speakers.

Update the server after changing the skill here:

```sh
for p in codex claude; do
  ssh <server> "mkdir -p /opt/openhands/$p/skills/jarvis && cat > /opt/openhands/$p/skills/jarvis/SKILL.md" < devices/echo/tools/openhands/jarvis/SKILL.md
done
```

The skill was `assistant-echo` / `jarvis-echo` before 31.10.2026 (full SSH access to one Echo,
build and deploy from the agent); both are retired.
