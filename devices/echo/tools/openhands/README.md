# OpenHands on the server

Files for the agent on the server (the server agent, issues #46, #124). The live copies are there; these
are the versions to compare against and to restore.

| Here | On the server | What |
|---|---|---|
| `assistant/SKILL.md` | `/opt/openhands/codex/skills/assistant/SKILL.md` and `/opt/openhands/claude/skills/assistant/SKILL.md` | Skill for both profiles (Codex, Claude Code): read every Echo with `ssh assistant-diag`, find the cause, fix on a branch, open a pull request; never merge or deploy. |

Not here (they belong to the server, not to the assistant): the agents' global instructions
(`/opt/openhands/codex/AGENTS.md`, `/opt/openhands/claude/CLAUDE.md`), which since #124 say that
the assistant changes only through a pull request the owner merges; the container's SSH config (alias
`assistant-diag`: the Raspberry Pi over WireGuard, jump host `host`) and its key
`agent_diag_ed25519`, which the Pi binds to the read-only diagnostics
(`services/deployer/assistant_diag.py`). The server has no other access to the speakers.

Update the server after changing the skill here:

```sh
for p in codex claude; do
  ssh <server> "mkdir -p /opt/openhands/$p/skills/assistant && cat > /opt/openhands/$p/skills/assistant/SKILL.md" < devices/echo/tools/openhands/assistant/SKILL.md
done
```

