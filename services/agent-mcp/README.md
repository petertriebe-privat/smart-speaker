# agent-mcp

MCP server that lets Jarvis hand tasks to an OpenHands agent profile (see *Task agent* in
[docs/architecture.md](../../devices/atoms3r/docs/architecture.md)). Runs next to OpenHands on the server, reachable
for the device behind the web server with a bearer token.

Tools: `delegate_task`, `task_status`, `task_reply`, `task_cancel`. Finished tasks are reported to
the admin's Telegram chat and announced on the speaker through the WhatsApp relay's `/event`.

**Approval (#57):** `delegate_task` and `task_reply` take `scope`: `research` (reading, looking up,
a report) runs at once with the order to change nothing outside the workspace; `change` runs at
once while approval is off (the default), and with approval on waits for the owner's tap on
"Freigeben"/"Ablehnen" in Telegram (valid 24 h, `pending.json`). The owner switches it with
`/freigabe an|aus` in Telegram (`settings.json`; `APPROVAL` is only the start value). The buttons
and the command reach the server through the device: its Telegram poller calls the tools
`task_decide` and `set_approval`, whose descriptions start with `[device]`; devices never offer
such tools to a model, so a model cannot approve its own task. Offline test: `python test_server.py`.

## Environment

| Variable | Meaning |
|---|---|
| `LOCAL_BACKEND_API_KEY` | OpenHands session API key |
| `OPENHANDS_URL` | default `http://127.0.0.1:8100` |
| `MCP_BEARER_TOKEN` | token the device sends (`connect_mcp_server`) |
| `AGENT_NAME`, `AGENT_PROFILE` | name in all texts; OpenHands agent profile to launch |
| `TELEGRAM_BOT_TOKEN`, `TELEGRAM_CHAT_ID` | Jarvis' bot and the admin's chat (sending only) |
| `RELAY_URL`, `RELAY_EVENT_TOKEN` | WhatsApp relay and its `EVENT_TOKEN` secret |
| `PORT`, `STATE_FILE` | default `8110`, `/data/notified.json` (`pending.json`, `settings.json` next to it) |
| `APPROVAL` | start value of the approval mode: `off` (default) or `change` |

## Compose service

```yaml
  agent-mcp:
    build: ./agent-mcp
    restart: unless-stopped
    network_mode: host          # reaches OpenHands on 127.0.0.1, listens on 127.0.0.1:8110
    env_file: [.env, agent-mcp.env]
    volumes: [./agent-mcp-data:/data]
    mem_limit: 256m
```

The web server forwards `/agent/mcp` to `http://127.0.0.1:8110/mcp` with
`Host: 127.0.0.1:8110` (the MCP SDK's DNS rebinding check) and without any other login.
