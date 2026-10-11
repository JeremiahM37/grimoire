"""Offline tests for the task-benefit study. No network model, no Grimoire build.

    python3 -m unittest benchmarks/task_benefit/test_task_benefit.py -v

A small in-process HTTP fake stands in for the server. It speaks the two routes
the study uses, POST /api/memory and GET /api/memory/context, and it escapes
< and > as \\u003c the way Go's encoder does, so the unescape path is tested.
"""
import json
import re
import sys
import tempfile
import threading
import unittest
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
from pathlib import Path
from urllib.parse import parse_qs, urlparse

HERE = Path(__file__).resolve().parent
BENCH = HERE.parent
sys.path.insert(0, str(BENCH))
sys.path.insert(0, str(HERE))

import budget  # noqa: E402
import run_task_benefit as R  # noqa: E402
from tasks import DISTRACTORS, TASKS  # noqa: E402


def _go_escape(s: str) -> str:
    return s.replace("<", "\\u003c").replace(">", "\\u003e")


class Fake(BaseHTTPRequestHandler):
    notes: list = []

    def log_message(self, *a):
        pass

    def _json(self, obj):
        b = json.dumps(obj).encode()
        self.send_response(200)
        self.send_header("Content-Type", "application/json")
        self.send_header("Content-Length", str(len(b)))
        self.end_headers()
        self.wfile.write(b)

    def do_POST(self):
        body = json.loads(self.rfile.read(int(self.headers["Content-Length"])))
        if urlparse(self.path).path != "/api/memory":
            self.send_error(404)
            return
        # The real server drops the final punctuation, and the study must cope.
        Fake.notes.append(body["text"].rstrip("."))
        self._json({"created": True})

    def do_GET(self):
        u = urlparse(self.path)
        if u.path != "/api/memory/context":
            self.send_error(404)
            return
        q = parse_qs(u.query).get("q", [""])[0]
        terms = set(re.findall(r"[a-z0-9]+", q.lower()))
        hits = []
        for text in Fake.notes:
            if len(terms & set(re.findall(r"[a-z0-9]+", text.lower()))) >= 3:
                hits.append(text)
        ctx = "\n".join(json.dumps({"text": _go_escape(t)}) for t in hits[:5])
        self._json({"context": ctx, "mode": "lexical"})


class ServerCase(unittest.TestCase):
    def setUp(self):
        Fake.notes = []
        self.srv = ThreadingHTTPServer(("127.0.0.1", 0), Fake)
        self.base = f"http://127.0.0.1:{self.srv.server_address[1]}"
        threading.Thread(target=self.srv.serve_forever, daemon=True).start()
        self.addCleanup(self.srv.server_close)
        self.addCleanup(self.srv.shutdown)


class Oracle:
    def complete(self, prompt, label=""):
        return R.OracleModel().complete(prompt, label)


class TestPlumbing(ServerCase):
    def test_oracle_on_arm_scores_only_retrieved_tasks(self):
        out = Path(tempfile.mkdtemp())
        s = R.run_study(self.base, Oracle(), 2, out, dry=True)
        self.assertEqual(s["status"], "dry-run-not-a-result")
        self.assertEqual(s["calls"], len(TASKS) * 2 * 2)
        self.assertEqual(s["totals"]["off"], [0, len(TASKS) * 2])
        # The oracle answers only when the fact is in its prompt, so the on-arm
        # pass count equals the number of retrieved tasks times the reps.
        on_pass, on_n = s["totals"]["on"]
        self.assertEqual(on_pass, s["retrieval_hits_on_arm"] * 2)
        self.assertEqual(on_n, len(TASKS) * 2)
        self.assertGreater(s["retrieval_hits_on_arm"], 0)

    def test_every_task_fact_is_seeded(self):
        out = Path(tempfile.mkdtemp())
        R.run_study(self.base, R.StubModel(), 1, out, dry=True)
        for t in TASKS:
            self.assertTrue(any(R.has_fact(n, t["fact"]) for n in Fake.notes), t["id"])
        for d in DISTRACTORS:
            self.assertIn(d.rstrip("."), Fake.notes)

    def test_stub_scores_zero_on_both_arms(self):
        out = Path(tempfile.mkdtemp())
        s = R.run_study(self.base, R.StubModel(), 1, out, dry=True)
        self.assertEqual(s["totals"]["off"][0], 0)
        self.assertEqual(s["totals"]["on"][0], 0)

    def test_answers_file_is_one_row_per_call(self):
        out = Path(tempfile.mkdtemp())
        R.run_study(self.base, R.StubModel(), 2, out, dry=True)
        rows = [json.loads(x) for x in (out / "answers.jsonl").read_text().splitlines()]
        self.assertEqual(len(rows), len(TASKS) * 2 * 2)


class TestHelpers(unittest.TestCase):
    def test_passes_requires_every_regex(self):
        self.assertTrue(R.passes("Use tessel-lint with .tessel.toml", [r"tessel-lint", r"\.tessel\.toml"]))
        self.assertFalse(R.passes("Use tessel-lint", [r"tessel-lint", r"\.tessel\.toml"]))
        self.assertFalse(R.passes("", [r"storage-guild"]))

    def test_passes_negative_wrong_port(self):
        self.assertFalse(R.passes("port 8630", [r"\b4417\b"]))
        self.assertTrue(R.passes("port 4417", [r"\b4417\b"]))

    def test_has_fact_handles_go_escapes_and_trailing_period(self):
        fact = "Quarry release branches follow the pattern rel/QX-<major>.<minor>, for example rel/QX-3.2."
        # The context string holds Go's JSON text verbatim, escapes included.
        ctx = '{"text":"' + _go_escape(fact.rstrip(".")) + '"}'
        self.assertTrue(R.has_fact(ctx, fact))
        self.assertFalse(R.has_fact(ctx.replace("QX", "QY"), fact))

    def test_sign_test_known_values(self):
        self.assertEqual(R.sign_test(0, 0), 1.0)
        self.assertAlmostEqual(R.sign_test(10, 0), 2 / 1024, places=9)
        self.assertAlmostEqual(R.sign_test(5, 5), 1.0)
        self.assertAlmostEqual(R.sign_test(8, 2), 0.109375, places=6)

    def test_prompts_differ_only_by_memory(self):
        q = TASKS[0]["question"]
        self.assertNotIn("<memory>", R.off_prompt(q))
        p = R.on_prompt(q, "CTX")
        self.assertIn("<memory>\nCTX\n</memory>", p)
        self.assertIn(q, p)


class TestBudget(unittest.TestCase):
    def setUp(self):
        self.ledger = Path(tempfile.mkdtemp()) / "ledger.jsonl"

    def test_run_cap_refuses_before_the_call(self):
        b = budget.Budget("t", run_cap=2, total_cap=150, ledger=self.ledger)
        b.spend("a")
        b.spend("b")
        with self.assertRaises(budget.BudgetExceeded):
            b.spend("c")
        self.assertEqual(budget.spent(self.ledger), 2)

    def test_total_cap_spans_runs(self):
        b1 = budget.Budget("t1", run_cap=5, total_cap=3, ledger=self.ledger)
        for i in range(3):
            b1.spend(str(i))
        b2 = budget.Budget("t2", run_cap=5, total_cap=3, ledger=self.ledger)
        with self.assertRaises(budget.BudgetExceeded):
            b2.spend("x")
        self.assertEqual(budget.spent(self.ledger), 3)

    def test_default_caps_are_the_brief_caps(self):
        self.assertEqual(budget.TOTAL_CAP, 150)
        self.assertEqual(budget.RUN_CAP, 60)


class TestLivePreflight(unittest.TestCase):
    def test_live_refuses_a_plan_above_max_calls(self):
        import argparse
        args = argparse.Namespace(binary="/nonexistent", n=3, max_calls=10, resume=False,
                                  out=str(Path(tempfile.mkdtemp())), port=9171)
        with self.assertRaises(SystemExit) as cm:
            R.cmd_live(args)
        self.assertIn("above --max-calls", str(cm.exception))


if __name__ == "__main__":
    unittest.main()
