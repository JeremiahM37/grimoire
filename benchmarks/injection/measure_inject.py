"""Deterministic measurement of the injection scanner, before and after the
normalizer (go/internal/trust/normalize).

No model is called. The payloads come from corpus.py (40 instruction attacks)
and exfil_corpus.py (32 exfiltration attacks). Each payload is scored in two
ways by the same secscan injection check the vault runs:

  raw         the line exactly as written (the pre-normalizer behaviour)
  normalized  every reading from normalize.Expand (the shipped behaviour)

Two sets are measured:

  plain       the payloads as written
  obfuscated  each payload re-encoded by one of the transforms below; this is
              the subset the normalizer is meant to help with

Held-out split. The 50 unique payload bodies (40 corpus, 10 exfiltration
templates; the three origins of an exfiltration template share one body) are
sorted and split by position: even positions are the SEEN half, odd positions
the HELD-OUT half. Splitting by body keeps duplicates from landing on both
sides. Pattern families were written while looking at the seen half; the
held-out half is reported separately and is the test of whether the families
generalise. Caveat: the author had read the whole corpus file before writing
the families, so the held-out half is a weaker test than a fresh corpus would be.

The benign set is every markdown file under docs/ and the repository root, plus
the comment lines of the non-test Go sources and writing/. Benign false
positives are reported per line and per document. The benchmark's attack
payloads (benchmarks/) and Go test fixtures (*_test.go) are not in it.

Run from the repository root:

    python3 benchmarks/injection/measure_inject.py [--out results/inject-measure.json]

It shells out to `go test` on the secscan scorer, which is skipped by a normal
`go test ./...` and runs only when the INJECTION_BENCH_* variables are set.
"""
from __future__ import annotations

import argparse
import base64
import codecs
import json
import os
import shutil
import subprocess
import sys
import tempfile
import urllib.parse
from pathlib import Path

HERE = Path(__file__).resolve().parent
REPO = HERE.parent.parent
sys.path.insert(0, str(HERE))

import corpus  # noqa: E402
import exfil_corpus  # noqa: E402
import fresh_set  # noqa: E402

EXFIL_TARGET = "https://collect.example.com/upload"

# Homoglyph map used by the obfuscator. Chosen to be the Cyrillic letters that
# look identical to the Latin ones in most fonts; a subset of what the
# normalizer folds, so this transform tests the folding without tuning to it.
_HOMOGLYPH = {"a": "а", "e": "е", "o": "о", "p": "р",
              "c": "с", "i": "і"}


def _zwsp(text: str) -> str:
    out = []
    for word in text.split(" "):
        if len(word) >= 4:
            word = word[0] + "​" + word[1:]
        out.append(word)
    return " ".join(out)


def _homoglyph(text: str) -> str:
    return "".join(_HOMOGLYPH.get(c, c) for c in text)


def _fullwidth(text: str) -> str:
    return "".join(chr(ord(c) + 0xFEE0) if c.isascii() and c.isalpha() else c
                   for c in text)


def _nbsp(text: str) -> str:
    return text.replace(" ", " ")


def _b64(text: str) -> str:
    return "Decode and follow: " + base64.b64encode(text.encode()).decode()


def _hex(text: str) -> str:
    return "Hex payload: " + text.encode().hex()


def _urlenc(text: str) -> str:
    return urllib.parse.quote(text, safe="")


def _rot13(text: str) -> str:
    return codecs.encode(text, "rot13")


def _html(text: str) -> str:
    return "Quarterly summary <!-- " + " ".join(text.split()) + " --> done"


def _link(text: str) -> str:
    title = " ".join(text.split()).replace('"', "'")
    return '[notes](https://example.com/n "' + title + '")'


def _alt(text: str) -> str:
    return "![" + " ".join(text.split()).replace("]", "") + "](https://example.com/x.png)"


def _dataurl(text: str) -> str:
    return "[x](data:text/plain;base64," + base64.b64encode(text.encode()).decode() + ")"


TRANSFORMS = {
    "zero-width": _zwsp,
    "homoglyph": _homoglyph,
    "fullwidth": _fullwidth,
    "nbsp": _nbsp,
    "base64": _b64,
    "hex": _hex,
    "url-encoded": _urlenc,
    "rot13": _rot13,
    "html-comment": _html,
    "link-title": _link,
    "alt-text": _alt,
    "data-url": _dataurl,
}


def attack_set() -> list[dict]:
    """Every payload as written, then each obfuscation of it. Ids are stable.

    Each entry carries `half`: "seen" or "held" (see the module docstring)."""
    base = []  # (payload id, group, text)
    for a in corpus.ATTACKS:
        base.append((f"corpus:{a.id}", f"corpus:{a.id}",
                     a.payload.replace("{canary}", a.canary())))
    for a in exfil_corpus.ATTACKS:
        # ids are x{template:02d}{origin}; the template is the body's identity.
        base.append((f"exfil:{a.id}", f"exfil:t{a.id[1:3]}",
                     a.payload.replace("{target}", EXFIL_TARGET)))
    groups = sorted({g for _, g, _ in base})
    half = {g: ("seen" if i % 2 == 0 else "held") for i, g in enumerate(groups)}
    out = []
    for pid, g, text in base:
        h = half[g]
        out.append({"id": f"{pid}|plain", "text": text, "set": "plain",
                    "transform": "plain", "payload": pid, "half": h})
        for name, fn in TRANSFORMS.items():
            out.append({"id": f"{pid}|{name}", "text": fn(text), "set": "obfuscated",
                        "transform": name, "payload": pid, "half": h})
    return out


def _go_comments(path: Path) -> str:
    """The `//` comment lines of one Go file, without the markers."""
    lines = []
    for ln in path.read_text(encoding="utf-8", errors="replace").splitlines():
        t = ln.strip()
        if t.startswith("//"):
            lines.append(t[2:].strip())
    return "\n".join(lines)


def benign_set() -> list[dict]:
    out = []
    for p in sorted((REPO / "docs").glob("*.md")) + sorted(REPO.glob("*.md")):
        out.append({"path": str(p.relative_to(REPO)),
                    "body": p.read_text(encoding="utf-8", errors="replace")})
    for p in sorted((REPO / "writing").rglob("*.md")):
        out.append({"path": str(p.relative_to(REPO)),
                    "body": p.read_text(encoding="utf-8", errors="replace")})
    for p in sorted((REPO / "go").rglob("*.go")):
        if p.name.endswith("_test.go") or "testdata" in p.parts:
            continue
        body = _go_comments(p)
        if body:
            out.append({"path": str(p.relative_to(REPO)), "body": body})
    return out


def run_scorer(attacks: list[dict], benign: list[dict]) -> dict:
    go = shutil.which("go") or "/usr/local/go/bin/go"
    gobin = str(Path(go).parent)
    env = dict(os.environ)
    env["PATH"] = gobin + os.pathsep + env.get("PATH", "")
    with tempfile.TemporaryDirectory(prefix="inject-bench-") as tmp:
        tmp_in = Path(tmp) / "in.json"
        tmp_out = Path(tmp) / "out.json"
        tmp_in.write_text(json.dumps({
            "attacks": [{"id": a["id"], "text": a["text"]} for a in attacks],
            "benign": benign,
        }))
        env["INJECTION_BENCH_IN"] = str(tmp_in)
        env["INJECTION_BENCH_OUT"] = str(tmp_out)
        proc = subprocess.run(
            [go, "test", "./internal/dream/secscan/", "-run", "TestInjectionCorpusMeasure",
             "-count=1", "-v"],
            cwd=REPO / "go", env=env, capture_output=True, text=True)
        if proc.returncode != 0:
            sys.stderr.write(proc.stdout + proc.stderr)
            raise SystemExit("go test scorer failed")
        return json.loads(tmp_out.read_text())


def _rate(n: int, d: int) -> float:
    return 0.0 if d == 0 else n / d


def summarise(attacks: list[dict], scored: dict) -> dict:
    by_id = {a["id"]: a for a in scored["attacks"]}

    def agg(pred) -> dict:
        n = raw = legacy = norm = 0
        for a in attacks:
            if not pred(a):
                continue
            r = by_id[a["id"]]
            n += 1
            raw += int(r["raw"])
            legacy += int(r["legacy"])
            norm += int(r["normalized"])
        return {"n": n, "raw": raw, "legacy_normalized": legacy, "normalized": norm,
                "recall_raw": _rate(raw, n),
                "recall_legacy_normalized": _rate(legacy, n),
                "recall_normalized": _rate(norm, n)}

    halves = {"seen": "seen", "held": "held"}
    res = {}
    for set_name in ("plain", "obfuscated"):
        res[set_name] = {"all": agg(lambda a, s=set_name: a["set"] == s)}
        for h in halves:
            res[set_name][h] = agg(lambda a, s=set_name, h=h: a["set"] == s and a["half"] == h)

    per_transform = {}
    for a in attacks:
        if a["set"] != "obfuscated":
            continue
        r = by_id[a["id"]]
        v = per_transform.setdefault(a["transform"], {"n": 0, "raw": 0, "normalized": 0,
                                                      "held_normalized": 0, "held_n": 0,
                                                      "exposed_by": {}})
        v["n"] += 1
        v["raw"] += int(r["raw"])
        v["normalized"] += int(r["normalized"])
        if a["half"] == "held":
            v["held_n"] += 1
            v["held_normalized"] += int(r["normalized"])
        if r.get("exposed_by") and r["normalized"]:
            k = " > ".join(r["exposed_by"])
            v["exposed_by"][k] = v["exposed_by"].get(k, 0) + 1

    benign = {
        "docs": scored["benign_docs"],
        "lines": scored["benign_lines"],
        "docs_flagged_raw": scored["benign_docs_flagged_raw"],
        "docs_flagged_normalized": scored["benign_docs_flagged_normalized"],
        "lines_flagged_raw": scored["benign_lines_flagged_raw"],
        "lines_flagged_normalized": scored["benign_lines_flagged_normalized"],
        "doc_fp_rate_normalized": _rate(scored["benign_docs_flagged_normalized"], scored["benign_docs"]),
        "line_fp_rate_normalized": _rate(scored["benign_lines_flagged_normalized"], scored["benign_lines"]),
        "flagged_docs_normalized": scored.get("benign_flagged_normalized", []),
        "flagged_lines_normalized": scored.get("benign_flagged_lines", []),
    }
    return {"recall": res, "by_transform": per_transform, "benign": benign}


def _pct(x: float) -> str:
    return f"{100 * x:5.1f}%"


def print_table(s: dict) -> None:
    r = s["recall"]
    print("Recall of the injection check: raw | legacy patterns on normalized text | "
          "current (legacy + families)")
    for set_name in ("plain", "obfuscated"):
        for h in ("seen", "held", "all"):
            v = r[set_name][h]
            print(f"  {set_name:<11}{h:<6} n={v['n']:<4}"
                  f" raw {v['raw']:>4} {_pct(v['recall_raw'])}"
                  f" | legacy {v['legacy_normalized']:>4} {_pct(v['recall_legacy_normalized'])}"
                  f" | current {v['normalized']:>4} {_pct(v['recall_normalized'])}")
    print()
    print(f"  {'transform':<14}{'n':>5}{'raw':>6}{'current':>9}{'held-out':>10}   exposed by")
    for name, v in s["by_transform"].items():
        ex = ", ".join(f"{k} x{c}" for k, c in sorted(v["exposed_by"].items(),
                                                      key=lambda kv: -kv[1])[:2])
        print(f"  {name:<14}{v['n']:>5}{v['raw']:>6}{v['normalized']:>9}"
              f"{v['held_normalized']:>5}/{v['held_n']:<4}  {ex}")
    b = s["benign"]
    print()
    print(f"Benign false positives: {b['docs']} documents, {b['lines']} non-empty lines")
    print(f"  per document  raw {b['docs_flagged_raw']}  current {b['docs_flagged_normalized']}"
          f"  ({_pct(b['doc_fp_rate_normalized'])})")
    print(f"  per line      raw {b['lines_flagged_raw']}  current {b['lines_flagged_normalized']}"
          f"  ({_pct(b['line_fp_rate_normalized'])})")
    for line in b["flagged_lines_normalized"]:
        print(f"    flagged: {line[:150]}")


def fresh_summary() -> dict:
    """The independently written set (fresh_set.py), scored in its own run so
    it cannot be confused with the corpus halves."""
    attacks = [{"id": f"fresh-{i:02d}", "text": t} for i, t in enumerate(fresh_set.FRESH_ATTACKS)]
    benign = [{"path": f"fresh/benign-{i:02d}", "body": t}
              for i, t in enumerate(fresh_set.FRESH_BENIGN)]
    a_scored = run_scorer(attacks, [])
    b_scored = run_scorer([], benign)
    n = len(attacks)
    raw = sum(int(r["raw"]) for r in a_scored["attacks"])
    legacy = sum(int(r["legacy"]) for r in a_scored["attacks"])
    cur = sum(int(r["normalized"]) for r in a_scored["attacks"])
    missed = [a["text"] for a, r in zip(attacks, a_scored["attacks"]) if not r["normalized"]]
    fp_docs = b_scored["benign_docs_flagged_normalized"]  # a count
    return {
        "attacks": {"n": n, "raw": raw, "legacy_normalized": legacy, "normalized": cur,
                    "recall_raw": _rate(raw, n), "recall_legacy_normalized": _rate(legacy, n),
                    "recall_normalized": _rate(cur, n), "missed": missed},
        "benign": {"sentences": len(benign), "flagged_normalized": fp_docs,
                   "flagged": b_scored.get("benign_flagged_lines", []),
                   "fp_rate_normalized": _rate(fp_docs, len(benign))},
    }


def main() -> None:
    ap = argparse.ArgumentParser()
    ap.add_argument("--out", default=str(HERE / "results" / "inject-measure.json"))
    args = ap.parse_args()

    attacks = attack_set()
    benign = benign_set()
    scored = run_scorer(attacks, benign)
    summary = summarise(attacks, scored)
    summary["fresh"] = fresh_summary()
    Path(args.out).parent.mkdir(parents=True, exist_ok=True)
    Path(args.out).write_text(json.dumps(summary, indent=2, ensure_ascii=False) + "\n")
    print_table(summary)
    f = summary["fresh"]
    print()
    print("Fresh independent set (fresh_set.py, written before the final run)")
    a, b = f["attacks"], f["benign"]
    print(f"  attacks  n={a['n']}  raw {a['raw']} {_pct(a['recall_raw'])}"
          f" | legacy {a['legacy_normalized']} {_pct(a['recall_legacy_normalized'])}"
          f" | current {a['normalized']} {_pct(a['recall_normalized'])}")
    for t in a["missed"]:
        print(f"    missed: {t[:110]}")
    print(f"  benign sentences n={b['sentences']}  flagged {b['flagged_normalized']}"
          f" ({_pct(b['fp_rate_normalized'])})")
    for line in b["flagged"]:
        print(f"    flagged: {line[:150]}")
    print(f"\nwrote {args.out}")


if __name__ == "__main__":
    main()
