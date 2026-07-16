#!/usr/bin/env python3
"""mnemo MCP server — exposes your notes to any MCP client (Claude Code, claude.ai
bridges, the homelab agents, agentdeck). This is what makes mnemo AI-native: the
AI reads, searches, links, and writes notes as first-class tools, not through a
chat box.

Run (stdio): .venv/bin/python -m server.mcp_server
Talks to a running mnemo server (MNEMO_API, default http://127.0.0.1:9111).
Private notes are never returned by search/ask unless include_private is set.
"""
import json
import os
import sys
import urllib.parse
import urllib.request

sys.path.insert(0, os.path.dirname(os.path.dirname(os.path.abspath(__file__))))

from mcp.server.fastmcp import FastMCP  # noqa: E402

API = os.environ.get("MNEMO_API", "http://127.0.0.1:9111").rstrip("/")
TOKEN = os.environ.get("MNEMO_AUTH_TOKEN", "")
mcp = FastMCP("mnemo")


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


if __name__ == "__main__":
    mcp.run()
