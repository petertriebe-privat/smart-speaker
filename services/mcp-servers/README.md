# MCP servers

General MCP servers for agents (OpenHands, Claude Code, any MCP client), one service, each server
under its own path and bearer token (Streamable HTTP). Credentials stay with the service; a client
only gets the tokens of the servers it may use.

| Path | Tools | Access |
|---|---|---|
| `/imap/mcp` | `imap_accounts`, `imap_folders`, `imap_search`, `imap_read`, `imap_attachment`, `imap_move`, `imap_create_folder` | any IMAP mailbox; read only unless an account allows moving (then also folders; many uids per call); never deletes; reading does not mark as seen |
| `/google/mcp` | `gmail_search`, `gmail_read`, `gmail_attachment`, `gmail_labels`, `gmail_create_label`, `gmail_modify`, `drive_list`, `drive_put`, `drive_get`, `calendar_events`, `contacts_search` | Gmail read, labels and archive (needs `gmail.modify`; trash and spam refused); calendars, contacts read only; Drive limited to one folder |
| `/n8n/mcp` | `n8n_workflows`, `n8n_get_workflow`, `n8n_create_workflow`, `n8n_update_workflow`, `n8n_activate_workflow`, `n8n_executions`, `n8n_execution` | build and change workflows; no deactivating or deleting |

## Configuration (environment)

| Variable | Meaning |
|---|---|
| `MCP_TOKEN_IMAP`, `MCP_TOKEN_GOOGLE`, `MCP_TOKEN_N8N` | bearer token per server; a server without token is off |
| `IMAP_ACCOUNTS_FILE` | JSON file (default `/run/secrets/imap.json`): `{"name": {"host": "imap.example.com", "user": "...", "password": "...", "port": 993, "allow_move": false}}`; a file because env files mangle `$` and ` #` in passwords |
| `GOOGLE_CREDENTIALS` | path of `{client_id, client_secret, refresh_token}` (`devices/atoms3r/scripts/google_auth.py --agent`) |
| `GOOGLE_DRIVE_FOLDER`, `GOOGLE_DRIVE_SOURCE` | the one Drive folder (`Parent/Child`), the `appProperties.source` of written files |
| `N8N_URL`, `N8N_API_KEY` | n8n instance and its API key (Settings → n8n API) |
| `HOST`, `PORT` | default `127.0.0.1:8120` |

## Running

```yaml
services:
  mcp-servers:
    build: .
    restart: unless-stopped
    network_mode: host
    env_file: mcp.env
    volumes: [./secrets:/run/secrets:ro]
    mem_limit: 256m
```

Behind a web server with TLS, forward `/<server>/mcp` to `http://127.0.0.1:8120/<server>/mcp` with
`Host: 127.0.0.1:8120` (the MCP SDK checks the host against DNS rebinding).

Clients, e.g. Claude Code: `claude mcp add --transport http imap https://<host>/imap/mcp
--header "Authorization: Bearer <MCP_TOKEN_IMAP>"`; OpenHands: Settings → MCP → custom server,
URL and the same header.
