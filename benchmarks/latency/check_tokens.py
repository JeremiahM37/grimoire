"""Cross-check tokens.py against the Go CountTokens it ports.

Copies go/internal/bank/tokens.go from this worktree into a scratch module,
runs it over a fixed sample (corpus text plus hand-picked edge cases), and
exits non-zero on any disagreement. Called by run.sh before the benchmark.
"""
from __future__ import annotations

import json
import subprocess
import sys
from pathlib import Path

HERE = Path(__file__).resolve().parent
sys.path.insert(0, str(HERE))
import corpus  # noqa: E402
from tokens import count_tokens  # noqa: E402

MAIN = '''package main

import (
	"encoding/json"
	"os"
)

func main() {
	var in []string
	if err := json.NewDecoder(os.Stdin).Decode(&in); err != nil {
		panic(err)
	}
	out := make([]int, len(in))
	for i, s := range in {
		out[i] = CountTokens(s)
	}
	json.NewEncoder(os.Stdout).Encode(out)
}
'''


def sample() -> list[str]:
    facts, _ = corpus.build(20261010, 2000)
    texts = [corpus.bullet(f) for f in facts[:1500]]
    texts += ["", " ", "a", "ab" * 40, "naïve café 東京 Привет 42", "x1234567890y",
              "__init__.py; a->b", "https://example.com/a?b=c&d=e", "日本語のテキスト",
              "Ünïcödé ÀÉÎ ǅ", "tab\there\nnewline", "12 3 45 6789"]
    return texts


def main() -> int:
    repo = Path(subprocess.run(["git", "-C", str(HERE), "rev-parse", "--show-toplevel"],
                               capture_output=True, text=True, check=True).stdout.strip())
    work = Path(sys.argv[1]) if len(sys.argv) > 1 else HERE / ".work"
    d = work / "tokcheck"
    d.mkdir(parents=True, exist_ok=True)
    # the file is package bank; rename so it builds as a main package beside main.go
    src = (repo / "go/internal/bank/tokens.go").read_text()
    (d / "tokens.go").write_text(src.replace("package bank", "package main", 1))
    (d / "main.go").write_text(MAIN)
    (d / "go.mod").write_text("module tokcheck\n\ngo 1.22\n")
    subprocess.run(["go", "build", "-o", str(d / "tokcheck"), "."], cwd=d, check=True)
    texts = sample()
    got = json.loads(subprocess.run([str(d / "tokcheck")], input=json.dumps(texts),
                                    capture_output=True, text=True, check=True).stdout)
    bad = [(t, g, count_tokens(t)) for t, g in zip(texts, got, strict=True) if count_tokens(t) != g]
    if bad:
        for t, g, p in bad[:5]:
            print(f"MISMATCH go={g} py={p} text={t[:80]!r}")
        print(f"tokens.py disagrees with CountTokens on {len(bad)}/{len(texts)} samples")
        return 1
    print(f"tokens.py matches CountTokens on {len(texts)} samples")
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
