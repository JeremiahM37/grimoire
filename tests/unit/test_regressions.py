"""Regression tripwires for bugs found during real-world testing.

Each test pins a specific past failure so it cannot silently return. Keep the
"regression:" note in each docstring — it documents the original incident.
"""
import sqlite3
from pathlib import Path

import pytest

ROOT = Path(__file__).resolve().parents[2]


def test_dockerfile_ships_every_runtime_directory():
    """regression: the image once shipped without plugins/ — the container ran
    but every first-party plugin (katex, mermaid, kanban…) was silently absent.
    The Dockerfile must COPY every directory the server serves at runtime."""
    df = (ROOT / "deploy" / "Dockerfile").read_text()
    for required in ("COPY server", "COPY web", "COPY cli", "COPY plugins"):
        assert required in df, f"Dockerfile is missing '{required}'"


def test_dockerfile_installs_all_runtime_deps():
    """regression: argon2-cffi was once missing from the image — the app booted
    but the secret vault crashed on first use. Pin the runtime dep list."""
    df = (ROOT / "deploy" / "Dockerfile").read_text()
    for dep in ("fastapi", "uvicorn", "watchdog", "cryptography",
                "python-multipart", "argon2-cffi"):
        assert dep in df, f"Dockerfile is missing runtime dep '{dep}'"


def test_dockerfile_runs_as_non_root_with_healthcheck():
    """Production posture: non-root user + a healthcheck (added after the
    container audit). Losing either is a silent hardening regression."""
    df = (ROOT / "deploy" / "Dockerfile").read_text()
    assert "USER grimoire" in df
    assert "HEALTHCHECK" in df and "/api/health" in df


def test_db_reinit_closes_previous_connection(tmp_path):
    """regression: db.init() used to leak the previous sqlite connection when
    called twice (test fixture + app lifespan both init). Hundreds of leaked
    WAL handles caused intermittent interpreter segfaults and cross-test
    IntegrityErrors. Re-init must close the old handle."""
    from server import db
    db.init(tmp_path / "a.db")
    first = db._conn
    db.init(tmp_path / "b.db")
    try:
        with pytest.raises(sqlite3.ProgrammingError):
            first.execute("SELECT 1")          # closed handles refuse queries
    finally:
        db.close()


def test_watcher_singleton_is_disabled_in_this_suite(monkeypatch, vaultdir):
    """regression: the module-level watcher singleton, shared by every
    TestClient app, once replayed a previous test's vault into the next test's
    index (the historic notes.path IntegrityError). Unit/api tests must run
    with the watcher off; the watcher has its own integration test."""
    import os
    assert os.environ.get("GRIMOIRE_NO_WATCHER") == "1"
    from server.watcher import watcher
    assert getattr(watcher, "_observer", None) is None or \
        not getattr(watcher._observer, "is_alive", lambda: False)()


def test_note_body_comparison_ignores_trailing_newline(client):
    """regression: the serializer guarantees a trailing newline on disk, so a
    naive equality check treated every unchanged save as a content change and
    flooded version history with identical snapshots."""
    client.post("/api/notes", json={"title": "NL", "body": "same"})
    for _ in range(3):
        client.put("/api/notes/nl.md", json={"body": "same"})
    assert client.get("/api/notes/nl.md/history").json() == []


def test_history_routes_not_swallowed_by_greedy_note_route(client):
    """regression: FastAPI matches in registration order and
    /notes/{path:path} is greedy — the /history sub-routes were registered
    after it and 404'd. They must stay registered first."""
    client.post("/api/notes", json={"title": "Greedy", "body": "a"})
    client.put("/api/notes/greedy.md", json={"body": "b"})
    r = client.get("/api/notes/greedy.md/history")
    assert r.status_code == 200 and len(r.json()) == 1


def test_version_is_one_point_oh():
    """Ship as v1.0.0 — pyproject and the FastAPI app must agree."""
    py = (ROOT / "pyproject.toml").read_text()
    assert 'version = "1.0.0"' in py
    app_src = (ROOT / "server" / "app.py").read_text()
    assert 'version="1.0.0"' in app_src


def test_agent_setup_emits_discoverability_snippet(capsys):
    """regression (benchmark round 3): agents with mounted-but-unadvertised MCP
    tools made ZERO knowledge-base calls. Discoverability must be deployable:
    agent-setup prints the MCP config plus a context-file snippet."""
    from cli.grimoire import cmd_agent_setup
    cmd_agent_setup(["http://example-host:9111"])
    out = capsys.readouterr().out
    assert "mcpServers" in out and "http://example-host:9111" in out
    assert "get_briefing" in out and "CLAUDE.md" in out


def test_agent_setup_ships_the_reflection_hook(capsys):
    """regression (episode benchmark): agents solved tasks and wrote ZERO
    memories — reflection is not in their natural stop path. agent-setup must
    ship the Stop-hook that asks once before a session ends."""
    from cli.grimoire import cmd_agent_setup
    cmd_agent_setup([])
    out = capsys.readouterr().out
    assert "stop_hook_active" in out and "remember" in out
    assert "grimoire-reflect.py" in out


def test_chunk_text_splits_blank_line_free_transcripts():
    """A transcript/log with only single newlines must not become one giant
    chunk — that made retrieval return whole documents instead of passages."""
    from server.ai import chunk_text
    lines = "\n".join(f"Speaker {i % 2}: utterance number {i} with some words"
                      for i in range(200))
    chunks = chunk_text(lines, target=800)
    assert len(chunks) > 5
    assert all(len(c) <= 800 * 1.6 for c in chunks)
    # nothing lost
    assert "".join(c.replace("\n", "") for c in chunks) == lines.replace("\n", "")


def test_chunk_text_splits_single_enormous_line_on_sentences():
    from server.ai import chunk_text
    text = " ".join(f"Sentence number {i} is here." for i in range(300))
    chunks = chunk_text(text, target=800)
    assert len(chunks) > 3
    assert all(len(c) <= 800 * 1.6 for c in chunks)


def test_chunk_text_normal_prose_unchanged():
    from server.ai import chunk_text
    text = "Para one.\n\nPara two.\n\nPara three."
    assert chunk_text(text) == [text]


def test_retrieve_ranks_rare_term_chunk_first(client):
    """Hybrid retrieval: a chunk holding the rare, discriminative query word
    must outrank chunks that share only filler words with the query."""
    client.post("/api/notes", json={"title": "Ops Log", "body": (
        "the deploy went fine and the team was happy about the deploy\n\n"
        "the team met about the roadmap and the team talked a lot\n\n"
        "kubernetes ingress crashed with error INV-9931 during the deploy")})
    hits = client.get("/api/retrieve", params={"q": "what was the INV-9931 error"}).json()
    assert hits and "INV-9931" in hits[0]["chunk"]


def test_search_natural_language_or_fallback(client):
    """A question that doesn't match every term still surfaces the best note."""
    client.post("/api/notes", json={"title": "Backup Runbook",
                                    "body": "restic prune runs on sundays"})
    hits = client.get("/api/search",
                      params={"q": "when exactly does the restic prune job run"}).json()
    assert any(h["title"] == "Backup Runbook" for h in hits)


def test_search_full_excerpts_long_notes(client):
    """full=True on a long note returns the query-relevant excerpt, flagged,
    not the whole body."""
    filler = "\n\n".join(f"paragraph {i} about nothing in particular" for i in range(80))
    body = filler + "\n\nthe vault passphrase rotates every 90 days\n\n" + filler
    client.post("/api/notes", json={"title": "Big Note", "body": body})
    hits = client.get("/api/search",
                      params={"q": "passphrase rotates", "full": True}).json()
    h = next(x for x in hits if x["title"] == "Big Note")
    assert h.get("excerpted") is True
    assert "rotates every 90 days" in h["body"]
    assert len(h["body"]) < len(body) / 2


def test_retrieve_top_hits_include_neighbor_chunks(client):
    """Small-to-big: the top-ranked hits come back with their neighbouring
    chunks merged, so an answer straddling a chunk boundary stays whole."""
    paras = [f"filler paragraph {i} about ordinary things" for i in range(12)]
    paras[6] = "the incident started when the ZX-500 router dropped all packets"
    paras[7] = "the resolution was replacing the ZX-500 power supply unit"
    client.post("/api/notes", json={"title": "Incident Log", "body": "\n\n".join(paras)})
    hits = client.get("/api/retrieve", params={"q": "ZX-500 router incident"}).json()
    top = hits[0]["chunk"]
    assert "dropped all packets" in top
    # the neighbouring chunk's content rides along with the top hit
    assert "power supply" in top or any("power supply" in h["chunk"] for h in hits[:2])


def test_retrieve_expansion_does_not_duplicate_chunks(client):
    body = "\n\n".join(f"alpha section {i} mentions the keyword zebra" for i in range(8))
    client.post("/api/notes", json={"title": "Zebra Doc", "body": body})
    hits = client.get("/api/retrieve", params={"q": "zebra keyword"}).json()
    joined = [h["chunk"] for h in hits]
    for i, a in enumerate(joined):
        for bpart in joined[i + 1:]:
            assert a not in bpart and bpart not in a


def test_embed_signature_change_triggers_reembed(client, monkeypatch):
    """Switching embedding backends must re-embed the vault — cosine over
    mixed-backend vectors is meaningless."""
    from server import ai, db, index
    client.post("/api/notes", json={"title": "Sig Note", "body": "hello world"})
    assert index.ensure_embed_signature() is False        # first run just records
    before = db.one("SELECT embedding FROM vectors LIMIT 1")["embedding"]
    monkeypatch.setattr(ai, "embed_signature", lambda: "other:backend")
    monkeypatch.setattr(ai, "embed",
                        lambda texts: [[1.0] + [0.0] * (ai.EMBED_DIM - 1) for _ in texts])
    assert index.ensure_embed_signature() is True         # change → re-embed
    after = db.one("SELECT embedding FROM vectors LIMIT 1")["embedding"]
    assert after != before
    assert index.ensure_embed_signature() is False        # new sig recorded, stable now


def test_retrieve_scores_are_rankable_not_uniform(client):
    """Regression: RRF fusion made every /retrieve score ~0.03, so the UI's
    score*100 showed a uniform '3%'. The scores must still strictly rank
    (distinct, descending) so the UI can normalize them to a relevance %."""
    for i in range(6):
        client.post("/api/notes", json={"title": f"Doc {i}",
                    "body": f"paragraph about topic alpha and detail number {i} " * 3})
    client.post("/api/notes", json={"title": "Bullseye",
                "body": "the rollback procedure for a bad alpha deploy is force-recreate"})
    hits = client.get("/api/retrieve", params={"q": "alpha rollback deploy", "k": 5}).json()
    scores = [h["score"] for h in hits]
    assert scores == sorted(scores, reverse=True)          # descending
    assert scores[0] > scores[-1]                          # genuinely spread, not flat


def test_openai_compatible_generation_backend(monkeypatch):
    """GRIMOIRE_LLM=openai routes answer synthesis to any OpenAI-compatible
    /chat/completions endpoint (OpenAI, vLLM, LM Studio, OpenRouter, …)."""
    import io
    import json as _json

    from server import ai
    monkeypatch.setenv("GRIMOIRE_LLM", "openai")
    monkeypatch.setenv("GRIMOIRE_LLM_BASE_URL", "http://fake.local/v1")
    monkeypatch.setenv("GRIMOIRE_LLM_API_KEY", "sk-test")
    monkeypatch.setenv("GRIMOIRE_LLM_MODEL", "gpt-test")
    seen = {}

    def fake_urlopen(req, timeout=0):
        seen["url"] = req.full_url
        seen["auth"] = req.headers.get("Authorization")
        seen["body"] = _json.loads(req.data)
        payload = {"choices": [{"message": {"content": "Force-recreate the deploy [1]."}}]}
        return io.BytesIO(_json.dumps(payload).encode())

    monkeypatch.setattr(ai.urllib.request, "urlopen", fake_urlopen)
    out = ai.answer("how do I roll back?",
                    [{"path": "a.md", "title": "Runbook", "chunk": "force-recreate"}])
    assert out == "Force-recreate the deploy [1]."
    assert seen["url"] == "http://fake.local/v1/chat/completions"
    assert seen["auth"] == "Bearer sk-test"
    assert seen["body"]["model"] == "gpt-test"
    assert seen["body"]["messages"][0]["role"] == "user"


def test_openai_backend_falls_back_on_outage(monkeypatch):
    """A backend error must never fail the request — fall back to extractive."""
    from server import ai
    monkeypatch.setenv("GRIMOIRE_LLM", "openai")
    monkeypatch.setenv("GRIMOIRE_LLM_BASE_URL", "http://unreachable.local/v1")

    def boom(req, timeout=0):
        raise OSError("connection refused")

    monkeypatch.setattr(ai.urllib.request, "urlopen", boom)
    out = ai.answer("q", [{"path": "a.md", "title": "T", "chunk": "the answer text"}])
    assert "the answer text" in out       # extractive fallback, not an error


def test_openai_compatible_embeddings_backend(monkeypatch):
    """GRIMOIRE_EMBED_BASE_URL routes embeddings to any OpenAI-compatible
    /embeddings endpoint, batched, and switches the embed signature so the
    index re-embeds on backend change."""
    import io
    import json as _json

    from server import ai
    monkeypatch.setenv("GRIMOIRE_EMBED_BASE_URL", "http://fake.local/v1")
    monkeypatch.setenv("GRIMOIRE_EMBED_API_KEY", "sk-emb")
    monkeypatch.setenv("GRIMOIRE_EMBED_MODEL", "text-embedding-3-small")
    seen = {}

    def fake_urlopen(req, timeout=0):
        seen["url"] = req.full_url
        seen["auth"] = req.headers.get("Authorization")
        seen["body"] = _json.loads(req.data)
        n = len(seen["body"]["input"])
        payload = {"data": [{"embedding": [0.1, 0.2, 0.3]} for _ in range(n)]}
        return io.BytesIO(_json.dumps(payload).encode())

    monkeypatch.setattr(ai.urllib.request, "urlopen", fake_urlopen)
    out = ai.embed(["one", "two"])
    assert out == [[0.1, 0.2, 0.3], [0.1, 0.2, 0.3]]        # batched, both returned
    assert seen["url"] == "http://fake.local/v1/embeddings"
    assert seen["auth"] == "Bearer sk-emb"
    assert seen["body"]["input"] == ["one", "two"]
    assert ai.embed_signature().startswith("openai:")


def test_embed_backend_falls_back_when_remote_down(monkeypatch):
    from server import ai
    monkeypatch.setenv("GRIMOIRE_EMBED_BASE_URL", "http://down.local/v1")

    def boom(req, timeout=0):
        raise OSError("refused")

    monkeypatch.setattr(ai.urllib.request, "urlopen", boom)
    v = ai.embed(["hello world"])                          # falls to hashing, no crash
    assert len(v) == 1 and len(v[0]) == ai.EMBED_DIM


def test_retrieve_vectorized_matches_scalar(client, monkeypatch):
    """The numpy fast path and the pure-Python fallback must return the same
    ranking — the optimization changes speed, never results."""
    from server import index
    for i in range(8):
        client.post("/api/notes", json={"title": f"Filler {i}",
                    "body": f"topic alpha beta gamma passage number {i} " * 4})
    client.post("/api/notes", json={"title": "Bullseye",
                "body": "the rollback runbook uses force-recreate for a bad deploy"})
    q = {"q": "rollback bad deploy force-recreate", "k": 5}
    hits_fast = [h["path"] for h in client.get("/api/retrieve", params=q).json()]
    monkeypatch.setattr(index, "_np", None)     # force scalar path
    index._vec_cache.clear()
    hits_slow = [h["path"] for h in client.get("/api/retrieve", params=q).json()]
    assert hits_fast == hits_slow and hits_fast


def test_retrieve_corpus_cache_invalidates_on_write(client):
    """A new note must be retrievable immediately — the corpus cache can't
    serve a stale matrix after an index write."""
    client.post("/api/notes", json={"title": "A", "body": "alpha content about widgets"})
    client.get("/api/retrieve", params={"q": "zeppelin airship", "k": 5}).json()   # warm cache
    client.post("/api/notes", json={"title": "Zep",
                "body": "the zeppelin airship history described in great detail"})
    r2 = client.get("/api/retrieve", params={"q": "zeppelin airship", "k": 5}).json()
    assert any(h["title"] == "Zep" for h in r2)


def test_retrieve_corpus_cache_invalidates_on_delete(client):
    client.post("/api/notes", json={"title": "Deletable",
                "body": "the quokka is a small marsupial from rottnest island"})
    assert client.get("/api/retrieve", params={"q": "quokka marsupial"}).json()
    client.delete("/api/notes/deletable.md")
    r = client.get("/api/retrieve", params={"q": "quokka marsupial"}).json()
    assert not any(h["title"] == "Deletable" for h in r)


def test_mcp_transport_env_mapping(monkeypatch):
    """GRIMOIRE_MCP_TRANSPORT selects the MCP transport; anything unknown or
    unset stays on stdio (the safe local default)."""
    from server import mcp_server
    for val, expect in [("stdio", "stdio"), ("http", "streamable-http"),
                        ("streamable-http", "streamable-http"), ("sse", "sse"),
                        ("HTTP", "streamable-http"), ("nonsense", "stdio")]:
        monkeypatch.setenv("GRIMOIRE_MCP_TRANSPORT", val)
        assert mcp_server._transport() == expect, val
    monkeypatch.delenv("GRIMOIRE_MCP_TRANSPORT", raising=False)
    assert mcp_server._transport() == "stdio"


def test_candidate_generation_path_matches_full_topk(client, monkeypatch):
    """Above the threshold, retrieval switches to indexed candidate generation.
    On a small vault the candidate pool covers everything, so the top result
    must match the full-fusion path — same recipe, just index-sourced."""
    from server import index
    for i in range(12):
        client.post("/api/notes", json={"title": f"Filler {i}",
                    "body": f"generic passage {i} about assorted unrelated topics " * 3})
    client.post("/api/notes", json={"title": "Target",
                "body": "the incident was caused by a stale kubernetes ingress namespace"})
    q = {"q": "stale kubernetes ingress namespace incident", "k": 5}
    monkeypatch.setattr(index, "_CAND_THRESHOLD", 10**9)      # force full path
    index._vec_cache.clear()
    full = [h["path"] for h in client.get("/api/retrieve", params=q).json()]
    monkeypatch.setattr(index, "_CAND_THRESHOLD", 0)          # force candidate path
    index._vec_cache.clear()
    cand = [h["path"] for h in client.get("/api/retrieve", params=q).json()]
    assert full and cand and full[0] == cand[0] == "target.md"


def test_fts_chunks_index_stays_in_sync(client, monkeypatch):
    """The chunk-level FTS index must track writes and deletes, or the
    candidate path would surface stale/ghost chunks."""
    from server import db, index
    monkeypatch.setattr(index, "_CAND_THRESHOLD", 0)          # exercise the FTS leg
    client.post("/api/notes", json={"title": "Doc", "body": "quokka marsupial rottnest island"})
    assert db.one("SELECT COUNT(*) c FROM fts_chunks WHERE chunk LIKE '%quokka%'")["c"] >= 1
    index._vec_cache.clear()
    assert any(h["title"] == "Doc"
               for h in client.get("/api/retrieve", params={"q": "quokka marsupial"}).json())
    client.delete("/api/notes/doc.md")
    assert db.one("SELECT COUNT(*) c FROM fts_chunks WHERE note='doc.md'")["c"] == 0
