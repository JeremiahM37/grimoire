"""Offline tests for the judge-validity study. No model call is made.

    python3 -m unittest benchmarks/judge_validity/test_judge_validity.py -v
"""
import argparse
import sys
import tempfile
import unittest
from pathlib import Path

HERE = Path(__file__).resolve().parent
sys.path.insert(0, str(HERE))

import run_judge_validity as J  # noqa: E402
from triples import TRIPLES  # noqa: E402


class Verdict:
    """Test double: a judge with a fixed verdict."""

    def __init__(self, verdict):
        self.verdict = verdict

    def complete(self, prompt, label=""):
        return '{"correct": %s}' % ("true" if self.verdict else "false")


class TestTripleValidity(unittest.TestCase):
    """The checker must separate the triples; otherwise the comparison means nothing."""

    def test_checker_accepts_every_gold_answer(self):
        for t in TRIPLES:
            self.assertTrue(J.checker(t["gold"], t["require"]), t["qid"])

    def test_checker_rejects_every_wrong_answer(self):
        for t in TRIPLES:
            self.assertFalse(J.checker(t["wrong"], t["require"]), t["qid"])

    def test_checker_accepts_every_paraphrase(self):
        for t in TRIPLES:
            if "ok" in t:
                self.assertTrue(J.checker(t["ok"], t["require"]), t["qid"])

    def test_wrong_answers_are_not_the_gold(self):
        for t in TRIPLES:
            self.assertNotEqual(t["wrong"].strip(" ."), t["gold"].strip(" ."), t["qid"])

    def test_counts(self):
        self.assertEqual(len(TRIPLES), 30)
        self.assertEqual(sum(1 for t in TRIPLES if "ok" in t), 10)
        self.assertEqual(len({t["qid"] for t in TRIPLES}), 30)


class TestMetrics(unittest.TestCase):
    def test_parse_verdict(self):
        self.assertTrue(J.parse_verdict('{"correct": true}'))
        self.assertFalse(J.parse_verdict('{"correct": false}'))
        self.assertIsNone(J.parse_verdict("I think it is right"))

    def test_wilson_known_values(self):
        lo, hi = J.wilson(0, 30)
        self.assertEqual(lo, 0.0)
        self.assertAlmostEqual(hi, 0.1135, places=3)
        lo, hi = J.wilson(15, 30)
        self.assertLess(lo, 0.5)
        self.assertGreater(hi, 0.5)

    def test_dry_run_plumbing_accepting_judge(self):
        out = Path(tempfile.mkdtemp())
        s = J.grade(J.FakeJudge(), out, dry=True)
        self.assertEqual(s["status"], "dry-run-not-a-result")
        self.assertEqual(s["calls"], 40)
        self.assertEqual(s["wrong_answers"]["leniency"], 1.0)
        self.assertEqual(s["wrong_answers"]["checker_accepted"], 0)
        self.assertEqual(s["gold_answers_checker_accepted"], "30/30")
        # the always-accepting judge disagrees with the checker on every wrong answer
        self.assertEqual(len([d for d in s["disagreements"] if d["kind"] == "wrong"]), 30)

    def test_rejecting_judge_disagrees_only_on_paraphrases(self):
        out = Path(tempfile.mkdtemp())
        s = J.grade(Verdict(False), out, dry=True)
        self.assertEqual(s["wrong_answers"]["judge_accepted"], 0)
        self.assertEqual(s["correct_paraphrases"]["judge_false_reject_rate"], 1.0)
        self.assertEqual({d["kind"] for d in s["disagreements"]}, {"paraphrase"})
        self.assertEqual(len(s["disagreements"]), 10)

    def test_unparseable_output_is_counted_not_hidden(self):
        class Junk:
            def complete(self, prompt, label=""):
                return "maybe"
        out = Path(tempfile.mkdtemp())
        s = J.grade(Junk(), out, dry=True)
        self.assertEqual(s["unparseable_judge_output"], 40)
        self.assertEqual(s["disagreements"], [])


class TestLivePreflight(unittest.TestCase):
    def test_live_refuses_above_max_calls(self):
        args = argparse.Namespace(max_calls=10, out=str(Path(tempfile.mkdtemp())))
        with self.assertRaises(SystemExit):
            J.cmd_live(args)


if __name__ == "__main__":
    unittest.main()
