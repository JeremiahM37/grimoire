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

The benign set is every markdown file under docs/ plus the repository's root
markdown. Benign false positives are reported per line and per document. The
benchmark's own attack payloads are not part of the benign set.

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
    """Every payload as written, then each obfuscation of it. Ids are stable."""
    base = []
    for a in corpus.ATTACKS:
        base.append((f"corpus:{a.id}", a.payload.replace("{canary}", a.canary())))
    for a in exfil_corpus.ATTACKS:
        base.append((f"exfil:{a.id}",
                     a.payload.replace("{target}", EXFIL_TARGET)))
    out = []
    for pid, text in base:
        out.append({"id": f"{pid}|plain", "text": text, "set": "plain",
                    "transform": "plain", "payload": pid})
        for name, fn in TRANSFORMS.items():
            out.append({"id": f"{pid}|{name}", "text": fn(text), "set": "obfuscated",
                        "transform": name, "payload": pid})
    return out


def benign_set() -> list[dict]:
    paths = sorted((REPO / "docs").glob("*.md")) + sorted(REPO.glob("*.md"))
    out = []
    for p in paths:
        out.append({"path": str(p.relative_to(REPO)),
                    "body": p.read_text(encoding="utf-8", errors="replace")})
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
    rows = {}
    for a in attacks:
        r = by_id[a["id"]]
        key = a["transform"]
        bucket = rows.setdefault(key, {"n": 0, "raw": 0, "normalized": 0, "exposed_by": {}})
        bucket["n"] += 1
        bucket["raw"] += int(r["raw"])
        bucket["normalized"] += int(r["normalized"])
        if r.get("exposed_by") and r["normalized"]:
            k = " > ".join(r["exposed_by"])
            bucket["exposed_by"][k] = bucket["exposed_by"].get(k, 0) + 1

    def agg(pred):
        n = raw = norm = 0
        for a in attacks:
            if not pred(a):
                continue
            r = by_id[a["id"]]
            n += 1
            raw += int(r["raw"])
            norm += int(r["normalized"])
        return {"n": n, "raw": raw, "normalized": norm,
                "recall_raw": _rate(raw, n), "recall_normalized": _rate(norm, n)}

    benign = {
        "docs": scored["benign_docs"],
        "lines": scored["benign_lines"],
        "docs_flagged_raw": scored["benign_docs_flagged_raw"],
        "docs_flagged_normalized": scored["benign_docs_flagged_normalized"],
        "lines_flagged_raw": scored["benign_lines_flagged_raw"],
        "lines_flagged_normalized": scored["benign_lines_flagged_normalized"],
        "doc_fp_rate_raw": _rate(scored["benign_docs_flagged_raw"], scored["benign_docs"]),
        "doc_fp_rate_normalized": _rate(scored["benign_docs_flagged_normalized"], scored["benign_docs"]),
        "line_fp_rate_raw": _rate(scored["benign_lines_flagged_raw"], scored["benign_lines"]),
        "line_fp_rate_normalized": _rate(scored["benign_lines_flagged_normalized"], scored["benign_lines"]),
        "flagged_docs_normalized": scored.get("benign_flagged_normalized", []),
    }
    return {
        "plain": agg(lambda a: a["set"] == "plain"),
        "obfuscated": agg(lambda a: a["set"] == "obfuscated"),
        "by_transform": {k: {"n": v["n"], "raw": v["raw"], "normalized": v["normalized"],
                             "exposed_by": v["exposed_by"]}
                         for k, v in rows.items()},
        "benign": benign,
    }


def print_table(s: dict) -> None:
    def pct(x: float) -> str:
        return f"{100 * x:5.1f}%"

    p, o, b = s["plain"], s["obfuscated"], s["benign"]
    print("Recall of the injection check (flagged / attacks)")
    print(f"  plain as written   n={p['n']:<4} raw {p['raw']:>3} {pct(p['recall_raw'])}"
          f"   normalized {p['normalized']:>3} {pct(p['recall_normalized'])}")
    print(f"  obfuscated subset  n={o['n']:<4} raw {o['raw']:>3} {pct(o['recall_raw'])}"
          f"   normalized {o['normalized']:>3} {pct(o['recall_normalized'])}")
    print()
    print(f"  {'transform':<14}{'n':>5}{'raw':>6}{'normalized':>12}   exposed by")
    for name, v in s["by_transform"].items():
        if name == "plain":
            continue
        ex = ", ".join(f"{k} x{c}" for k, c in sorted(v["exposed_by"].items(),
                                                      key=lambda kv: -kv[1])[:2])
        print(f"  {name:<14}{v['n']:>5}{v['raw']:>6}{v['normalized']:>12}   {ex}")
    print()
    print("Benign false positives (docs/*.md and root *.md)")
    print(f"  documents {b['docs']}, lines {b['lines']}")
    print(f"  per document   raw {b['docs_flagged_raw']} ({pct(b['doc_fp_rate_raw'])})"
          f"   normalized {b['docs_flagged_normalized']} ({pct(b['doc_fp_rate_normalized'])})")
    print(f"  per line       raw {b['lines_flagged_raw']} ({pct(b['line_fp_rate_raw'])})"
          f"   normalized {b['lines_flagged_normalized']} ({pct(b['line_fp_rate_normalized'])})")
    for path in b["flagged_docs_normalized"]:
        print(f"    flagged: {path}")


def main() -> None:
    ap = argparse.ArgumentParser()
    ap.add_argument("--out", default=str(HERE / "results" / "inject-measure.json"))
    args = ap.parse_args()

    attacks = attack_set()
    benign = benign_set()
    scored = run_scorer(attacks, benign)
    summary = summarise(attacks, scored)
    Path(args.out).parent.mkdir(parents=True, exist_ok=True)
    Path(args.out).write_text(json.dumps(summary, indent=2, ensure_ascii=False) + "\n")
    print_table(summary)
    print(f"\nwrote {args.out}")


if __name__ == "__main__":
    main()
