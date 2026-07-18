#!/usr/bin/env python3
"""Grimoire MCP server — the substrate's primary agent interface.

An agent that mounts this server gets the four facets of the context server as
first-class tools:

  knowledge   search_notes / ask_notes / read_note / create_note / update_note …
  memory      remember / recall — an agent-writable namespace whose entries are
              plain notes the human can read, edit, diff, and roll back
  credentials use_credential / list_grants — brokered calls where the agent
              USES a secret but never sees its value (scoped, time-boxed, audited)
  links       backlinks / list_tags — the knowledge graph

Run (stdio): .venv/bin/python -m server.mcp_server
Talks to a running Grimoire server (GRIMOIRE_API, default http://127.0.0.1:9111).
Set GRIMOIRE_AGENT_NAME to attribute memories to this agent. Private notes are
never returned by search/ask unless include_private is set.
"""
import json
import os
import sys
import urllib.parse
import urllib.request

sys.path.insert(0, os.path.dirname(os.path.dirname(os.path.abspath(__file__))))

from mcp.server.fastmcp import FastMCP  # noqa: E402

API = os.environ.get("GRIMOIRE_API", "http://127.0.0.1:9111").rstrip("/")
TOKEN = os.environ.get("GRIMOIRE_AUTH_TOKEN", "")
mcp = FastMCP("grimoire")


def api(method: str, path: str, body: dict | None = None):
    headers = {"Content-Type": "application/json"}
    if TOKEN:
        headers["Authorization"] = f"Bearer {TOKEN}"
    req = urllib.request.Request(API + "/api" + path, method=method, headers=headers,
                                 data=json.dumps(body).encode() if body is not None else None)
    with urllib.request.urlopen(req, timeout=60) as r:
        return json.load(r) if r.status != 204 else None


@mcp.tool()
def search_notes(query: str, limit: int = 20) -> list:
    """Full-text search the vault. Returns path/title/snippet."""
    return api("GET", f"/search?q={urllib.parse.quote(query)}&limit={limit}")


@mcp.tool()
def ask_notes(question: str, include_private: bool = False) -> dict:
    """Answer a question from the notes (RAG) with citations. Private notes are
    excluded unless include_private=True."""
    return api("POST", "/ask", {"q": question, "include_private": include_private})


@mcp.tool()
def read_note(path: str) -> dict:
    """Read a note's title, body, tags, links, and backlinks."""
    return api("GET", f"/notes/{urllib.parse.quote(path)}")


@mcp.tool()
def list_notes(tag: str = "") -> list:
    """List notes, optionally filtered by tag."""
    q = f"?tag={urllib.parse.quote(tag)}" if tag else ""
    return api("GET", f"/notes{q}")


@mcp.tool()
def create_note(title: str, body: str, tags: list[str] | None = None) -> dict:
    """Create a new note. Returns its path."""
    return api("POST", "/notes", {"title": title, "body": body, "tags": tags or []})


@mcp.tool()
def update_note(path: str, body: str) -> dict:
    """Replace a note's body (keeps frontmatter/title)."""
    return api("PUT", f"/notes/{urllib.parse.quote(path)}", {"body": body})


@mcp.tool()
def append_daily(text: str) -> dict:
    """Append a line to today's daily note (creates it if needed)."""
    return api("POST", "/capture", {"text": text, "source": "mcp"})


@mcp.tool()
def backlinks(path: str) -> list:
    """Notes that link to the given note."""
    return api("GET", f"/notes/{urllib.parse.quote(path)}").get("backlinks", [])


@mcp.tool()
def list_tags() -> list:
    """All tags with counts."""
    return api("GET", "/tags")


AGENT_NAME = os.environ.get("GRIMOIRE_AGENT_NAME", "agent")


@mcp.tool()
def remember(text: str, topic: str = "", task: str = "") -> dict:
    """Persist a memory. Memories accrete on a per-topic note under memory/ in
    the vault, attributed to this agent — the human can read, edit, and roll
    them back like any note. Use a stable topic to build up knowledge over time."""
    return api("POST", "/memory", {"text": text, "topic": topic,
                                   "agent": AGENT_NAME, "task": task})


@mcp.tool()
def recall(query: str = "", limit: int = 10) -> list:
    """Recall memories. With a query: full-text search over the memory
    namespace. Without: the most recently touched memory notes."""
    q = urllib.parse.urlencode({"q": query, "limit": limit})
    return api("GET", f"/memory?{q}")


@mcp.tool()
def use_credential(grant: str, url: str, method: str = "GET",
                   header: str = "Authorization", body: str | None = None) -> dict:
    """Call a service with a secret injected server-side — you never see the
    value. Requires a grant token the human minted for you (scoped to a URL
    prefix, time-boxed, audited). 403 = outside your grant's scope or expired."""
    return api("POST", "/secrets/broker", {"grant": grant, "method": method,
                                           "url": url, "header": header, "body": body})


@mcp.tool()
def list_grants() -> list:
    """Active credential grants (grantee, scope, expiry — never values).
    Errors with 423 while the human has the vault locked."""
    return api("GET", "/grants")


if __name__ == "__main__":
    mcp.run()
