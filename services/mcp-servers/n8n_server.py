"""MCP server for an n8n instance's REST API: read, build and change workflows.

N8N_URL (e.g. http://127.0.0.1:5678) and N8N_API_KEY. Deactivating and deleting workflows are
left out on purpose: an agent builds and changes, the owner switches off.
"""

import json
import os

import httpx
from mcp.server.fastmcp import FastMCP

URL = os.environ.get("N8N_URL", "http://127.0.0.1:5678").rstrip("/") + "/api/v1"
KEY = os.environ.get("N8N_API_KEY", "").strip()
WRITABLE = ("name", "nodes", "connections", "settings", "staticData")  # what the API accepts on create/update

mcp = FastMCP("n8n", stateless_http=True, json_response=True)


async def api(method: str, path: str, **kwargs):
    async with httpx.AsyncClient(timeout=60) as client:
        r = await client.request(method, URL + path, headers={"X-N8N-API-KEY": KEY}, **kwargs)
    if r.status_code >= 400:
        raise RuntimeError(f"n8n {r.status_code}: {r.text[:500]}")
    return r.json() if r.content else {}


def workflow_body(workflow: dict | str) -> dict:
    data = json.loads(workflow) if isinstance(workflow, str) else workflow
    body = {k: data[k] for k in WRITABLE if k in data}
    body.setdefault("settings", {})
    missing = [k for k in ("name", "nodes", "connections") if k not in body]
    if missing:
        raise ValueError(f"workflow needs {', '.join(missing)}")
    return body


@mcp.tool()
async def n8n_workflows(name_contains: str = "", active_only: bool = False) -> str:
    """Lists workflows: id | active | name | updated | tags."""
    rows, cursor = [], None
    while True:
        params = {"limit": 250}
        if active_only:
            params["active"] = "true"
        if cursor:
            params["cursor"] = cursor
        page = await api("GET", "/workflows", params=params)
        for w in page.get("data", []):
            if name_contains.lower() in w.get("name", "").lower():
                tags = ", ".join(t.get("name", "") for t in w.get("tags", []) or [])
                rows.append(f"{w['id']} | {'active' if w.get('active') else 'inactive'} | {w['name']} | "
                            f"{w.get('updatedAt', '')[:16]}" + (f" | {tags}" if tags else ""))
        cursor = page.get("nextCursor")
        if not cursor:
            break
    return "\n".join(rows) or "No workflows."


@mcp.tool()
async def n8n_get_workflow(workflow_id: str) -> dict:
    """The workflow JSON (name, nodes, connections, settings, state), to read or as the base for an
    update."""
    w = await api("GET", f"/workflows/{workflow_id}")
    keep = ("id", "name", "active", "nodes", "connections", "settings", "updatedAt", "versionId")
    out = {k: w[k] for k in keep if k in w}  # not "shared": the owner's account data
    out["tags"] = [t.get("name") for t in w.get("tags") or []]
    return out


@mcp.tool()
async def n8n_create_workflow(workflow: dict) -> str:
    """Creates a workflow from JSON with name, nodes, connections (settings optional). It starts
    inactive; credentials are referenced by id and must exist in n8n."""
    w = await api("POST", "/workflows", json=workflow_body(workflow))
    return f"Created workflow {w.get('id')} '{w.get('name')}' (inactive)."


@mcp.tool()
async def n8n_update_workflow(workflow_id: str, workflow: dict) -> str:
    """Replaces a workflow's name, nodes, connections and settings (get it first, change, send it
    back complete). An active workflow stays active with the new version."""
    w = await api("PUT", f"/workflows/{workflow_id}", json=workflow_body(workflow))
    return f"Updated workflow {w.get('id')} '{w.get('name')}'."


@mcp.tool()
async def n8n_activate_workflow(workflow_id: str) -> str:
    """Activates a workflow (its triggers start running)."""
    w = await api("POST", f"/workflows/{workflow_id}/activate")
    return f"Workflow {w.get('id')} '{w.get('name')}' is active."


@mcp.tool()
async def n8n_executions(workflow_id: str = "", status: str = "", limit: int = 20) -> str:
    """Recent executions: id | workflow | status | started | stopped. status: success, error,
    waiting or empty for all."""
    params = {"limit": max(1, min(limit, 100))}
    if workflow_id:
        params["workflowId"] = workflow_id
    if status:
        params["status"] = status
    page = await api("GET", "/executions", params=params)
    return "\n".join(f"{e['id']} | {e.get('workflowId')} | {e.get('status') or ('success' if e.get('finished') else '?')} | "
                     f"{(e.get('startedAt') or '')[:19]} | {(e.get('stoppedAt') or '')[:19]}"
                     for e in page.get("data", [])) or "No executions."


@mcp.tool()
async def n8n_execution(execution_id: str) -> dict:
    """One execution with its data (node outputs, error), to debug a workflow."""
    return await api("GET", f"/executions/{execution_id}", params={"includeData": "true"})
