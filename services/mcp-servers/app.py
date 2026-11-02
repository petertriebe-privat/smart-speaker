"""Serves the MCP servers over Streamable HTTP, each under its own path and bearer token:
/imap/mcp, /google/mcp, /n8n/mcp. A server runs only when its token is set (MCP_TOKEN_IMAP,
MCP_TOKEN_GOOGLE, MCP_TOKEN_N8N), so each client gets just the servers it is given a token for.
"""

import contextlib
import hmac
import logging
import os

import uvicorn
from starlette.applications import Starlette
from starlette.middleware.base import BaseHTTPMiddleware
from starlette.responses import JSONResponse
from starlette.routing import Mount

import google_server
import imap_server
import n8n_server

SERVERS = {"imap": imap_server.mcp, "google": google_server.mcp, "n8n": n8n_server.mcp}
TOKENS = {name: os.environ.get(f"MCP_TOKEN_{name.upper()}", "") for name in SERVERS}
ACTIVE = {name: server for name, server in SERVERS.items() if TOKENS[name]}


class BearerPerServer(BaseHTTPMiddleware):
    async def dispatch(self, request, call_next):
        name = request.url.path.strip("/").split("/", 1)[0]
        token = TOKENS.get(name, "")
        given = request.headers.get("authorization", "")
        if name not in ACTIVE or not hmac.compare_digest(given, f"Bearer {token}"):
            return JSONResponse({"error": "unauthorized"}, status_code=401)
        return await call_next(request)


@contextlib.asynccontextmanager
async def lifespan(app):
    async with contextlib.AsyncExitStack() as stack:
        for server in ACTIVE.values():
            await stack.enter_async_context(server.session_manager.run())
        yield


app = Starlette(routes=[Mount(f"/{name}", app=server.streamable_http_app()) for name, server in ACTIVE.items()],
                lifespan=lifespan)
app.add_middleware(BearerPerServer)

if __name__ == "__main__":
    logging.basicConfig(level=logging.INFO, format="%(asctime)s %(name)s %(levelname)s %(message)s")
    logging.getLogger("httpx").setLevel(logging.WARNING)
    logging.getLogger("mcp.server.streamable_http_manager").setLevel(logging.WARNING)
    logging.info("serving: %s", ", ".join(ACTIVE) or "nothing (no MCP_TOKEN_* set)")
    uvicorn.run(app, host=os.environ.get("HOST", "127.0.0.1"), port=int(os.environ.get("PORT", "8120")),
                log_level="warning")
