"""Offline tests for the BEAM harness. No network, no model, no Grimoire build.

    python -m unittest benchmarks/beam/test_beam_dryrun.py -v
    python -m pytest benchmarks/beam/test_beam_dryrun.py      # also works

They run the real pipeline end to end on the synthetic fixture:
  - the stub model proves the plumbing and must score 0%;
  - the stub-oracle answers with the reference, so the scoring path must score 100%;
  - the grimoire arm runs against a fake HTTP server that speaks the four
    endpoints the harness uses, so goserver.launch, the vault check and
    LoCoMo's context assembly all execute.
"""
import contextlib
import io
import json
import os
import socket
import sys
import tempfile
import unittest
from pathlib import Path

HERE = Path(__file__).resolve().parent
sys.path.insert(0, str(HERE))

import run_beam  # noqa: E402

FAKE_GRIMOIRE = '''#!{python}
"""Fake grimoire: /api/health, /api/reindex, /api/retrieve, /api/search over the
vault's .md files, scored by token overlap. Test double only."""
import json, os, pathlib, re, urllib.parse
from http.server import ThreadingHTTPServer, BaseHTTPRequestHandler

VAULT = pathlib.Path(os.environ["GRIMOIRE_VAULT"]).resolve()
PORT = int(os.environ["GRIMOIRE_PORT"])

def notes():
    return [(p, p.read_text()) for p in sorted(VAULT.glob("*.md"))]

def toks(s):
    return set(re.findall(r"[a-z0-9]+", s.lower()))

def hits(q):
    t = toks(q)
    scored = [(len(t & toks(text)), p, text) for p, text in notes()]
    return sorted([h for h in scored if h[0]], key=lambda h: -h[0])

class H(BaseHTTPRequestHandler):
    def log_message(self, *a):
        pass

    def _send(self, obj):
        b = json.dumps(obj).encode()
        self.send_response(200)
        self.send_header("Content-Type", "application/json")
        self.send_header("Content-Length", str(len(b)))
        self.end_headers()
        self.wfile.write(b)

    def do_GET(self):
        u = urllib.parse.urlparse(self.path)
        qs = urllib.parse.parse_qs(u.query)
        if u.path == "/api/health":
            return self._send({{"vault": str(VAULT)}})
        q = qs.get("q", [""])[0]
        if u.path == "/api/retrieve":
            k = int(qs.get("k", ["10"])[0])
            return self._send([{{"path": p.name, "title": p.stem, "chunk": text}}
                               for _, p, text in hits(q)[:k]])
        if u.path == "/api/search":
            k = int(qs.get("limit", ["5"])[0])
            return self._send([{{"path": p.name, "title": p.stem, "body": text}}
                               for _, p, text in hits(q)[:k]])
        self.send_response(404)
        self.end_headers()

    def do_POST(self):
        n = int(self.headers.get("Content-Length") or 0)
        if n:
            self.rfile.read(n)
        if urllib.parse.urlparse(self.path).path == "/api/reindex":
            return self._send({{"indexed": len(notes())}})
        self.send_response(404)
        self.end_headers()

ThreadingHTTPServer(("127.0.0.1", PORT), H).serve_forever()
'''


def _free_port() -> int:
    with socket.socket() as s:
        s.bind(("127.0.0.1", 0))
        return s.getsockname()[1]


def _quiet(fn, *a, **kw):
    buf = io.StringIO()
    with contextlib.redirect_stdout(buf):
        rc = fn(*a, **kw)
    return rc, buf.getvalue()


class BeamDryRunTests(unittest.TestCase):
    def setUp(self):
        self._tmp = tempfile.TemporaryDirectory()
        self.tmp = Path(self._tmp.name)

    def tearDown(self):
        self._tmp.cleanup()

    def _summary(self, out: Path) -> dict:
        return json.loads((out / "summary.json").read_text())

    def test_fixture_matches_beam_schema(self):
        convs = run_beam.load_conversations(run_beam.FIXTURE)
        self.assertEqual(len(convs), 2)
        self.assertEqual(sum(len(c["questions"]) for c in convs), 40)
        for c in convs:
            self.assertEqual({q["ability"] for q in c["questions"]}, set(run_beam.ABILITIES))
            self.assertTrue(all(q["rubric"] for q in c["questions"]))
            self.assertTrue(c["batches"][0]["messages"])

    def test_loader_accepts_datasets_server_wrapper_in_jsonl(self):
        raw = json.loads(run_beam.FIXTURE.read_text())
        d = self.tmp / "rows"
        d.mkdir()
        (d / "Fixture-1M.jsonl").write_text(
            "\n".join(json.dumps({"row_idx": i, "row": r}) for i, r in enumerate(raw)))
        convs = run_beam.load_conversations(d)
        self.assertEqual([c["conv"] for c in convs], ["syn-001", "syn-002"])

    def test_dry_run_stub_scores_zero_and_writes_outputs(self):
        out = self.tmp / "dry"
        rc, log = _quiet(run_beam.main, ["dry-run", "--out", str(out)])
        self.assertEqual(rc, 0)
        self.assertIn("NOT a result", log)
        s = self._summary(out)
        self.assertTrue(s["dry_run"])
        self.assertEqual(s["status"], "dry-run-not-a-result")
        for arm in ("none", "full"):
            self.assertEqual(s["arms"][arm]["n"], 40)
            self.assertEqual(s["arms"][arm]["overall_score_pct"], 0.0)
            for ab in run_beam.ABILITIES:  # 2 conversations x 2 items per ability
                self.assertEqual(s["arms"][arm]["by_ability"][ab]["n"], 4)
        self.assertEqual(s["arms"]["none"]["median_approx_context_tokens"], 0)
        self.assertGreater(s["arms"]["full"]["median_approx_context_tokens"], 0)

    def test_oracle_ceiling_scores_100(self):
        out = self.tmp / "oracle"
        rc, _ = _quiet(run_beam.main, ["dry-run", "--backend", "stub-oracle",
                                       "--arms", "full", "--out", str(out)])
        self.assertEqual(rc, 0)
        s = self._summary(out)["arms"]["full"]
        self.assertEqual(s["overall_score_pct"], 100.0)
        self.assertEqual(s["overall_strict_pct"], 100.0)
        for ab in run_beam.ABILITIES:
            self.assertEqual(s["by_ability"][ab]["score_pct"], 100.0, ab)

    def test_resume_does_not_duplicate_rows(self):
        out = self.tmp / "resume"
        _quiet(run_beam.main, ["dry-run", "--arms", "none", "--out", str(out)])
        _quiet(run_beam.main, ["dry-run", "--arms", "none", "--out", str(out)])
        rows = [r["qid"] for r in run_beam.read_jsonl(out / "none" / "answers.jsonl")]
        self.assertEqual(len(rows), 40)
        self.assertEqual(len(set(rows)), 40)

    def test_grimoire_arm_against_fake_server(self):
        fake = self.tmp / "fake-grimoire"
        fake.write_text(FAKE_GRIMOIRE.format(python=sys.executable))
        fake.chmod(0o755)
        out = self.tmp / "grim"
        port = _free_port()
        rc, _ = _quiet(run_beam.main, ["dry-run", "--arms", "grimoire", "--binary", str(fake),
                                       "--port", str(port), "--out", str(out)])
        self.assertEqual(rc, 0)
        rows = run_beam.read_jsonl(out / "grimoire" / "answers.jsonl")
        self.assertEqual(len(rows), 40)
        self.assertTrue(all(r["context_chars"] > 0 for r in rows),
                        "every grimoire question should receive retrieved context")
        # the vault holds the LAST conversation's batches, one note per batch
        vault = sorted(p.name for p in (out / "grimoire-vault").glob("*.md"))
        self.assertEqual(vault, ["batch-01.md", "batch-02.md"])
        self.assertEqual(self._summary(out)["arms"]["grimoire"]["n"], 40)

    def test_grimoire_arm_requires_binary(self):
        with self.assertRaises(SystemExit):
            _quiet(run_beam.main, ["run", "--data", str(run_beam.FIXTURE),
                                   "--arms", "grimoire", "--out", str(self.tmp / "x")])


if __name__ == "__main__":
    unittest.main()
