#!/usr/bin/env python3
"""Adherence vs number of simultaneous rules: how many rules before a model drops them?

    python3 rule_capacity.py --selftest
    OPENAI_COMPAT_BASE_URL=http://127.0.0.1:11434/v1 OPENAI_COMPAT_MODEL=<name> \
        python3 rule_capacity.py run [--grid 1,2,4,8,16] [--trials 20] [--max-calls 120]

Each trial asks the model for one shell command for a task, with N rules
injected into the prompt. Rules are drawn from a fixed pool of 40. Each rule
has a regex checker on the produced command. Forbidden-pattern rules pass when
the pattern is absent. Required-pattern rules pass when it is present.

Output: per-rule adherence at each N (passes / checks), the share of trials in
which every rule held, the worst-holding rules, and the knee, which is the
smallest N whose per-rule adherence falls below `--knee` (default 0.90).

This is a ManyIFEval-style measurement (arXiv 2509.21051) on a task where the
checks are mechanical: no judge, no reader, nothing to argue about. A failure
is a failed regex, so it can be checked by hand.

The pool is jointly satisfiable. The selftest builds one command that passes
all 40 checks, so a low score cannot be blamed on contradictory rules.

Backend: any OpenAI-compatible server, read from the environment. There is NO
default model. Same variables as correction_wording: OPENAI_COMPAT_BASE_URL,
OPENAI_COMPAT_MODEL, optional OPENAI_COMPAT_API_KEY. Hosts whose name contains
"anthropic" or "claude" are refused. `--max-calls` caps a run before it starts.
`--selftest` uses a fake model and makes no network call.
"""
from __future__ import annotations

import argparse
import json
import math
import os
import random
import re
import tempfile
import urllib.parse
import urllib.request
from collections import Counter
from pathlib import Path

HERE = Path(__file__).parent

# (id, instruction shown to the model, kind, regex checked on the command)
# kind "forbid": the regex must NOT match. kind "require": it MUST match.
RULES = [
    # forbidden
    ("f01", "never use sudo", "forbid", r"\bsudo\b"),
    ("f02", "do not use grep (use rg)", "forbid", r"\bgrep\b"),
    ("f03", "do not use xargs", "forbid", r"\bxargs\b"),
    ("f04", "do not use eval", "forbid", r"\beval\b"),
    ("f05", "do not use backticks for command substitution", "forbid", r"`"),
    ("f06", "do not use sed -i", "forbid", r"\bsed\s+-i"),
    ("f07", "do not use awk", "forbid", r"\bawk\b"),
    ("f08", "do not use perl", "forbid", r"\bperl\b"),
    ("f09", "do not use python -c", "forbid", r"\bpython3?\s+-c\b"),
    ("f10", "do not redirect stderr to /dev/null", "forbid", r"2>\s*/dev/null"),
    ("f11", "do not use cd", "forbid", r"(?:^|[\s;&|])cd\s"),
    ("f12", "do not use head", "forbid", r"\bhead\b"),
    ("f13", "do not use tail", "forbid", r"\btail\b"),
    ("f14", "do not use echo", "forbid", r"\becho\b"),
    ("f15", "do not use printf", "forbid", r"\bprintf\b"),
    ("f16", "do not use &&", "forbid", r"&&"),
    ("f17", "do not use find -exec", "forbid", r"-exec\b"),
    ("f18", "do not use wc", "forbid", r"\bwc\b"),
    ("f19", "do not use ls", "forbid", r"\bls\b"),
    ("f20", "never use rm -rf", "forbid", r"\brm\s+-[a-zA-Z]*r[a-zA-Z]*f|\brm\s+-[a-zA-Z]*f[a-zA-Z]*r"),
    ("f21", "do not pipe cat into another command", "forbid", r"\bcat\b[^|\n]*\|"),
    ("f22", "do not use tr", "forbid", r"\btr\b"),
    ("f23", "never pipe curl into a shell", "forbid", r"curl[^\n]*\|\s*(?:ba|z)?sh\b"),
    ("f24", "never use chmod 777", "forbid", r"chmod\s+777"),
    ("f25", "do not write unquoted absolute paths (quote every path)", "forbid", r"(?:^|\s)/"),
    # required
    ("r01", "include --dry-run", "require", r"--dry-run"),
    ("r02", "pipe output through sort", "require", r"\|\s*sort\b"),
    ("r03", "send stderr to stdout (2>&1)", "require", r"2>&1"),
    ("r04", "wrap the command in timeout 30", "require", r"\btimeout\s+30\b"),
    ("r05", "limit find depth with -maxdepth N", "require", r"-maxdepth\s+\d+"),
    ("r06", "restrict find to files with -type f", "require", r"-type\s+f\b"),
    ("r07", "set LC_ALL=C", "require", r"LC_ALL=C"),
    ("r08", "pass --color=never", "require", r"--color=never"),
    ("r09", "start the command with a comment line", "require", r"\A\s*#"),
    ("r10", "use an absolute path (quoted is fine)", "require", r"(?:^|[\s\"'])/"),
    ("r11", "prefix the command with nice", "require", r"\bnice\b"),
    ("r12", "use -print0", "require", r"-print0"),
    ("r13", "log the output with tee", "require", r"\btee\b"),
    ("r14", "use date -u", "require", r"\bdate\s+[^\n|]*-u\b"),
    ("r15", "use --verbose", "require", r"--verbose\b"),
]
TASKS = [
    "list the 5 largest regular files under a directory held in $DIR, largest first",
    "count the *.log files under /var/log that changed in the last 24 hours",
    "show the first 20 lines of the newest file in ~/projects/app/logs",
    "find Python files under ~/work that contain the string TODO and print their paths",
    "report the disk usage of each subdirectory of /srv/data, sorted by size",
    "print the names of all empty directories under /tmp/scratch",
    "find files over 100 MB under /mnt/bulk that were not accessed in 90 days",
    "show the hostname and the uptime of this machine",
    "archive the directory ~/notes/inbox into a tar.gz file in ~/backups",
    "list the git branches in ~/projects/grimoire that are merged into main",
]
# One command that satisfies all 40 checks. Used by the selftest to show that
# the pool is jointly satisfiable.
GOOD = ("# count recent log files\n"
        "LC_ALL=C timeout 30 nice find \"/var/log\" -maxdepth 2 -type f -name \"*.log\" -print0 "
        "2>&1 | tee -a \"/tmp/log-count.txt\" | sort -z --dry-run --color=never --verbose; date -u")


def rule_ok(rule, cmd: str) -> bool:
    _id, _text, kind, pat = rule
    hit = re.search(pat, cmd, re.M) is not None
    return (not hit) if kind == "forbid" else hit


def check_all(rules, cmd: str) -> dict:
    return {r[0]: rule_ok(r, cmd) for r in rules}


def extract_command(reply: str) -> str:
    m = re.search(r"```[a-zA-Z]*\n(.*?)```", reply, re.S)
    return (m.group(1) if m else reply).strip()


def build_prompt(task: str, rules) -> str:
    lines = [f"{i}. {r[1]}" for i, r in enumerate(rules, 1)]
    return (f"Write one bash command that will {task}.\n\n"
            "The command must follow every rule below:\n" + "\n".join(lines) +
            "\n\nReply with the command only, in a single ```bash block.")


def wilson(k: int, n: int, z: float = 1.96):
    if n == 0:
        return 0.0, 0.0
    p = k / n
    den = 1 + z * z / n
    c = (p + z * z / (2 * n)) / den
    h = z * math.sqrt(p * (1 - p) / n + z * z / (4 * n * n)) / den
    return max(0.0, c - h), min(1.0, c + h)


def knee_of(per_rule: dict, threshold: float):
    """Smallest N whose per-rule adherence is below threshold, else None."""
    for n in sorted(per_rule):
        if per_rule[n] < threshold:
            return n
    return None


# ---- backend ----------------------------------------------------------------

def guard_host(base: str) -> None:
    host = (urllib.parse.urlparse(base).hostname or "").lower()
    if "anthropic" in host or "claude" in host:
        raise SystemExit("refusing: this harness does not call hosted Claude endpoints")


def chat(messages: list[dict]) -> str:
    base = os.environ.get("OPENAI_COMPAT_BASE_URL", "")
    model = os.environ.get("OPENAI_COMPAT_MODEL", "")
    if not base or not model:
        raise SystemExit("set OPENAI_COMPAT_BASE_URL and OPENAI_COMPAT_MODEL "
                         "(no default; a local OpenAI-compatible server only)")
    guard_host(base)
    headers = {"Content-Type": "application/json"}
    key = os.environ.get("OPENAI_COMPAT_API_KEY")
    if key:
        headers["Authorization"] = f"Bearer {key}"
    body = json.dumps({"model": model, "messages": messages,
                       "temperature": 0, "max_tokens": 256}).encode()
    req = urllib.request.Request(base.rstrip("/") + "/chat/completions", data=body,
                                 headers=headers, method="POST")
    with urllib.request.urlopen(req, timeout=180) as r:
        out = json.loads(r.read().decode())
    return out["choices"][0]["message"].get("content") or ""


# ---- run --------------------------------------------------------------------

def run(model, grid, trials: int, seed: int, threshold: float, max_calls: int, out_dir: Path) -> dict:
    total = len(grid) * trials
    if total > max_calls:
        raise SystemExit(f"{total} calls needed, --max-calls is {max_calls}; lower --trials/--grid or raise the cap")
    rng = random.Random(seed)
    rows = []
    for n in grid:
        for t in range(trials):
            task = rng.choice(TASKS)
            rules = rng.sample(RULES, n)
            reply = model([{"role": "user", "content": build_prompt(task, rules)}])
            cmd = extract_command(reply)
            res = check_all(rules, cmd)
            rows.append({"n": n, "trial": t, "task": task, "rules": [r[0] for r in rules],
                         "command": cmd[:400], "pass": res})
    per_n = {}
    per_rule_adh = {}
    for n in grid:
        sub = [r for r in rows if r["n"] == n]
        checks = sum(len(r["pass"]) for r in sub)
        passes = sum(sum(r["pass"].values()) for r in sub)
        all_ok = sum(all(r["pass"].values()) for r in sub)
        fails = Counter(rid for r in sub for rid, ok in r["pass"].items() if not ok)
        per_rule_adh[n] = passes / checks if checks else 0.0
        per_n[n] = {"trials": len(sub), "checks": checks, "passes": passes,
                    "per_rule_adherence": per_rule_adh[n],
                    "all_rules_held": all_ok, "worst_rules": fails.most_common(5)}
    knee = knee_of(per_rule_adh, threshold)
    out = {"model": os.environ.get("OPENAI_COMPAT_MODEL", "fake"), "seed": seed,
           "grid": list(grid), "trials": trials, "knee_threshold": threshold,
           "knee": knee, "by_n": per_n, "rows": rows}
    out_dir.mkdir(parents=True, exist_ok=True)
    dest = out_dir / f"rule_capacity-seed{seed}-t{trials}.json"
    dest.write_text(json.dumps(out, indent=1))
    print_table(out)
    print(f"wrote {dest}")
    return out


def print_table(out: dict) -> None:
    print(f"\nmodel: {out['model']}   trials per N: {out['trials']}   knee threshold: {out['knee_threshold']:.0%}\n")
    print("| N rules | trials | per-rule adherence (95% CI) | all rules held | worst rules |")
    print("|---|---|---|---|---|")
    for n in out["grid"]:
        d = out["by_n"][n]
        lo, hi = wilson(d["passes"], d["checks"])
        worst = ", ".join(f"{rid}×{c}" for rid, c in d["worst_rules"]) or "none"
        print(f"| {n} | {d['trials']} | {100*d['per_rule_adherence']:.1f}% ({100*lo:.0f}-{100*hi:.0f}%) | "
              f"{d['all_rules_held']}/{d['trials']} | {worst} |")
    if out["knee"] is None:
        print(f"\nknee: none in grid (per-rule adherence stayed at or above {out['knee_threshold']:.0%})")
    else:
        print(f"\nknee: N={out['knee']} (first N below {out['knee_threshold']:.0%} per-rule adherence)")


# ---- selftest ---------------------------------------------------------------

def compliant_fake(messages):
    return f"```bash\n{GOOD}\n```"


def violating_fake(messages):
    """Complies with everything except f01 (sudo), which it puts in the comment."""
    return "```bash\n" + GOOD.replace("# count recent log files", "# count recent log files via sudo") + "\n```"


def selftest() -> int:
    fails = []

    def check(name, cond):
        print(("PASS " if cond else "FAIL ") + name)
        if not cond:
            fails.append(name)

    ids = [r[0] for r in RULES]
    check("pool has 40 rules", len(RULES) == 40)
    check("rule ids unique", len(set(ids)) == 40)
    check("25 forbid and 15 require", sum(r[2] == "forbid" for r in RULES) == 25
          and sum(r[2] == "require" for r in RULES) == 15)
    check("every regex compiles", all(re.compile(r[3]) for r in RULES))
    check("tasks are 10", len(TASKS) == 10)

    res_good = check_all(RULES, GOOD)
    check("pool is jointly satisfiable: one command passes all 40 checks", all(res_good.values()))

    bad = check_all(RULES, "sudo grep -r TODO /etc | xargs -n1 cat | awk '{print}' `pwd`")
    check("bad command fails sudo rule", bad["f01"] is False)
    check("bad command fails grep rule", bad["f02"] is False)
    check("bad command fails the missing-require rules", bad["r01"] is False)
    check("good command passes the sudo rule", res_good["f01"] is True)

    check("extract: fenced block", extract_command("here\n```bash\nls -l\n```\nthanks") == "ls -l")
    check("extract: bare reply", extract_command("  find . -name x  ") == "find . -name x")

    check("knee: found at 8", knee_of({1: 1.0, 2: 0.98, 4: 0.95, 8: 0.85, 16: 0.6}, 0.9) == 8)
    check("knee: none when flat", knee_of({1: 1.0, 2: 1.0, 4: 1.0, 8: 0.95, 16: 0.92}, 0.9) is None)

    grid = [1, 2, 4, 8, 16]
    tmp = Path(tempfile.mkdtemp(prefix="rc-selftest-"))
    ok = run(compliant_fake, grid, trials=5, seed=3, threshold=0.9, max_calls=10_000, out_dir=tmp)
    check("compliant fake: adherence 100% at every N", all(
        ok["by_n"][n]["per_rule_adherence"] == 1.0 for n in grid))
    check("compliant fake: no knee", ok["knee"] is None)

    bad_run = run(violating_fake, grid, trials=5, seed=3, threshold=0.9, max_calls=10_000, out_dir=tmp)
    worst_names = {rid for n in grid for rid, _ in bad_run["by_n"][n]["worst_rules"]}
    check("violating fake: only the sudo rule ever fails", worst_names <= {"f01"} and "f01" in worst_names)
    check("violating fake: rule-level adherence below 100% where f01 was sampled",
          any(bad_run["by_n"][n]["per_rule_adherence"] < 1.0 for n in grid))

    try:
        run(compliant_fake, grid, trials=30, seed=3, threshold=0.9, max_calls=100, out_dir=tmp)
        check("max-calls cap enforced", False)
    except SystemExit:
        check("max-calls cap enforced", True)

    try:
        guard_host("https://api.anthropic.com/v1")
        check("claude host refused", False)
    except SystemExit:
        check("claude host refused", True)
    check("local host allowed", guard_host("http://127.0.0.1:11434/v1") is None)

    if fails:
        print(f"\n{len(fails)} FAILED")
        return 1
    print("\nall selftest checks passed")
    return 0


def main() -> int:
    ap = argparse.ArgumentParser(description=__doc__.split("\n")[0])
    ap.add_argument("mode", nargs="?", choices=["run"])
    ap.add_argument("--selftest", action="store_true")
    ap.add_argument("--grid", default="1,2,4,8,16")
    ap.add_argument("--trials", type=int, default=20)
    ap.add_argument("--seed", type=int, default=20261014)
    ap.add_argument("--knee", type=float, default=0.90)
    ap.add_argument("--max-calls", type=int, default=120)
    ap.add_argument("--out", default=str(HERE / "results"))
    args = ap.parse_args()
    if args.selftest:
        return selftest()
    if args.mode != "run":
        ap.print_help()
        return 2
    grid = [int(x) for x in args.grid.split(",")]
    if max(grid) > len(RULES):
        raise SystemExit(f"grid exceeds the {len(RULES)}-rule pool")
    run(chat, grid, args.trials, args.seed, args.knee, args.max_calls, Path(args.out))
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
