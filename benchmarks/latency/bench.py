"""Recall and write latency of a real grimoire server, over HTTP.

For each corpus size N the harness:
  1. generates a seeded corpus (corpus.py) into a fresh vault,
  2. starts the grimoire binary on it and times until /api/health answers
     (cold start: the server does a full index of the vault before listening),
  3. records resident memory,
  4. runs every recall mode: 20 warm-up queries (discarded), then 200 measured
     queries, each a sequential GET over one keep-alive connection,
  5. runs writes: 20 warm-up, then 200 measured POST /api/memory calls, which
     go through the server's reconcile path (supersede / challenge / add),
  6. stops the server.

Reported latency is client-observed wall time, so it includes the HTTP stack
and JSON encoding on this machine. Tokens are the project's estimator (ported
in tokens.py) summed over the text of the hits at the server's default limit.

Usage: bench.py --binary PATH --work DIR --out DIR [--sizes 1000,10000,50000]
"""
from __future__ import annotations

import argparse
import http.client
import json
import math
import os
import platform
import shutil
import signal
import statistics
import subprocess
import sys
import time
import urllib.parse
from pathlib import Path

HERE = Path(__file__).resolve().parent
sys.path.insert(0, str(HERE))
import corpus  # noqa: E402
from tokens import count_tokens  # noqa: E402

SEED = 20261010
N_QUERIES = 200
N_WARMUP = 20
DEFAULT_SIZES = (1_000, 10_000, 50_000)
GOLD_MODES = ("plain", "expand", "hops1")
MODES = {
    "plain": "",
    "expand": "&expand=true",
    "hops1": "&hops=1",
    "valid_at": "&valid_at=2026-06-01",
    "as_of": "&as_of={as_of}",
}


def pct(xs: list[float], p: float) -> float:
    s = sorted(xs)
    return s[max(0, math.ceil(p / 100 * len(s)) - 1)]


def stats(xs: list[float]) -> dict:
    return {
        "n": len(xs), "p50_ms": round(pct(xs, 50), 3), "p95_ms": round(pct(xs, 95), 3),
        "p99_ms": round(pct(xs, 99), 3), "mean_ms": round(statistics.fmean(xs), 3),
        "min_ms": round(min(xs), 3), "max_ms": round(max(xs), 3),
    }


def machine() -> dict:
    model = ""
    try:
        for line in Path("/proc/cpuinfo").read_text().splitlines():
            if line.startswith("model name"):
                model = line.split(":", 1)[1].strip()
                break
    except OSError:
        pass
    mem_kb = 0
    for line in Path("/proc/meminfo").read_text().splitlines():
        if line.startswith("MemTotal:"):
            mem_kb = int(line.split()[1])
    go = subprocess.run(["go", "version"], capture_output=True, text=True,
                        env={**os.environ, "PATH": os.environ["PATH"] + ":/usr/local/go/bin"})
    return {
        "loadavg_1m_at_start": os.getloadavg()[0],
        "cpu_model": model, "cpu_count": os.cpu_count(),
        "mem_total_gb": round(mem_kb / 1024 / 1024, 1),
        "kernel": platform.release(), "python": platform.python_version(),
        "go": go.stdout.strip() or "unknown",
    }


def server_env(vault: Path, port: int) -> dict:
    env = {k: v for k, v in os.environ.items() if not k.startswith("GRIMOIRE_")}
    env.update({
        "GRIMOIRE_VAULT": str(vault), "GRIMOIRE_PORT": str(port),
        "GRIMOIRE_HOST": "127.0.0.1", "GRIMOIRE_NO_WATCHER": "1",
        "GRIMOIRE_LOCAL_EMBED": "auto",  # the local model2vec embedder, loaded from the HF cache
        "HF_HUB_OFFLINE": "1",
    })
    return env


def start(binary: Path, vault: Path, port: int, log: Path):
    out = open(log, "w")  # noqa: SIM115 — closed in stop()
    proc = subprocess.Popen([str(binary)], env=server_env(vault, port), stdout=out, stderr=out)
    t0 = time.perf_counter()
    deadline = time.monotonic() + 900
    while True:
        if proc.poll() is not None:
            raise RuntimeError(f"server exited early, see {log}")
        try:
            c = http.client.HTTPConnection("127.0.0.1", port, timeout=5)
            c.request("GET", "/api/health")
            r = c.getresponse()
            body = json.loads(r.read() or b"{}")
            c.close()
            if r.status == 200 and body.get("vault") and body.get("ok"):
                return proc, out, time.perf_counter() - t0, body
        except (OSError, ValueError):
            pass
        if time.monotonic() > deadline:
            raise TimeoutError("server did not answer health within 900s")
        time.sleep(0.05)


def stop(proc, out) -> None:
    proc.send_signal(signal.SIGTERM)
    try:
        proc.wait(timeout=30)
    except subprocess.TimeoutExpired:
        proc.kill()
    out.close()


def rss_mb(pid: int) -> float:
    for line in Path(f"/proc/{pid}/status").read_text().splitlines():
        if line.startswith("VmRSS:"):
            return round(int(line.split()[1]) / 1024, 1)
    return 0.0


class Client:
    def __init__(self, port: int):
        self.conn = http.client.HTTPConnection("127.0.0.1", port, timeout=120)

    def request(self, method: str, path: str, body: dict | None = None):
        payload = None if body is None else json.dumps(body).encode()
        headers = {"Content-Type": "application/json"} if body is not None else {}
        t0 = time.perf_counter()
        self.conn.request(method, path, body=payload, headers=headers)
        r = self.conn.getresponse()
        raw = r.read()
        ms = (time.perf_counter() - t0) * 1000
        if r.status >= 400:
            raise RuntimeError(f"{method} {path[:120]} -> {r.status}: {raw[:200]!r}")
        return ms, json.loads(raw or b"null")


def recall_path(mode: str, q: str, as_of: str) -> str:
    return "/api/memory?q=" + urllib.parse.quote(q) + MODES[mode].format(
        as_of=urllib.parse.quote(as_of))


def run_size(binary: Path, work: Path, n: int, port: int) -> dict:
    vault = work / f"vault-{n}"
    if vault.exists():
        shutil.rmtree(vault)
    t_gen = time.perf_counter()
    facts, entities = corpus.build(SEED, n)
    corpus.write_vault(vault, facts)
    gen_s = time.perf_counter() - t_gen
    superseded = sum(1 for f in facts if f.superseded_by)
    human = sum(1 for f in facts if f.human)
    with_valid = sum(1 for f in facts if f.valid_from)

    log = work / f"server-{n}.log"
    proc, out, cold_s, health = start(binary, vault, port, log)
    index_line = next((ln.split(" ", 2)[-1] for ln in log.read_text().splitlines()
                       if " indexed " in ln), "")
    cell: dict = {
        "facts": n, "entities": len(entities), "superseded": superseded,
        "human_authored": human, "with_valid_from": with_valid,
        "corpus_gen_s": round(gen_s, 2), "cold_start_s": round(cold_s, 3),
        "index_log": index_line, "embedder": health.get("embedder"),
        "rss_mb_after_index": rss_mb(proc.pid),
    }
    try:
        cli = Client(port)
        targets = corpus.queries(facts, SEED, N_QUERIES + N_WARMUP)
        mid = facts[len(facts) // 2].stamp.strftime("%Y-%m-%dT%H:%M:%SZ")
        recall: dict = {}
        for mode in MODES:
            lat, toks, hits_n, at10 = [], [], [], 0
            for k, f in enumerate(targets):
                path = recall_path(mode, f.question, mid)
                if k < N_WARMUP:
                    cli.request("GET", path)
                    continue
                ms, hits = cli.request("GET", path)
                lat.append(ms)
                if hits is None:
                    hits = []
                toks.append(sum(count_tokens(h.get("text", "")) for h in hits[:20]))
                hits_n.append(min(len(hits), 20))
                if any(h.get("id") == f.id for h in hits[:10]):
                    at10 += 1
            # Gold is only meaningful when the mode does not filter the target out:
            # valid_at and as_of can legitimately exclude it.
            gold = round(at10 / N_QUERIES, 3) if mode in GOLD_MODES else None
            recall[mode] = {
                **stats(lat),
                "tokens_mean": round(statistics.fmean(toks), 1),
                "tokens_p50": round(pct(toks, 50), 1),
                "hits_mean": round(statistics.fmean(hits_n), 2),
                "gold_in_top10": gold,
            }
            print(f"  N={n} recall {mode:9s} p50={recall[mode]['p50_ms']}ms "
                  f"p95={recall[mode]['p95_ms']}ms tok={recall[mode]['tokens_mean']} "
                  f"gold@10={recall[mode]['gold_in_top10']}", flush=True)
        cell["recall"] = recall
        cell["rss_mb_after_reads"] = rss_mb(proc.pid)

        current = [(f.entity, f.attr) for f in facts if not f.superseded_by]
        payloads = corpus.write_stream(SEED, n, N_QUERIES + N_WARMUP, entities, current)
        wlat, wsup = [], 0
        for k, body in enumerate(payloads):
            ms, res = cli.request("POST", "/api/memory", body)
            if k < N_WARMUP:
                continue
            wlat.append(ms)
            ops = {r.get("op") for r in (res or {}).get("results", [])}
            wsup += int(bool(ops & {"UPDATE", "SUPERSEDE", "DELETE"}))
        cell["write"] = {**stats(wlat), "reconcile_updates": wsup}
        print(f"  N={n} write   p50={cell['write']['p50_ms']}ms p95={cell['write']['p95_ms']}ms "
              f"updates={wsup}/{N_QUERIES}", flush=True)
        cell["rss_mb_after_writes"] = rss_mb(proc.pid)
    finally:
        stop(proc, out)
    return cell


def findings(results: dict) -> str:
    """Observations the numbers support, computed from results.json."""
    big = [c for c in results["cells"] if c["facts"] > 20_000]
    lines = []
    for c in big:
        plain, asof = c["recall"]["plain"], c["recall"]["as_of"]
        lines.append(
            f"- **Candidate window at {c['facts']:,} facts.** The recall SQL orders by "
            f"`stamp DESC` and keeps `DefaultScanLimit` = 20,000 rows (`go/internal/index/memory.go`); "
            f"only those are scored. Plain recall returned gold in top-10 for "
            f"{plain['gold_in_top10']:.0%} of targets here, against "
            f"{results['cells'][0]['recall']['plain']['gold_in_top10']:.0%} at "
            f"{results['cells'][0]['facts']:,} facts (where the whole vault fits the window).")
        lines.append(
            f"- **`as_of` at {c['facts']:,} facts returned {asof['hits_mean']} hits on average.** "
            f"The window is newest-first and `as_of` is applied after it, so when the newest 20,000 "
            f"facts all postdate the instant, the answer is empty. Its latency is an empty-result "
            f"path and is not comparable to the other modes.")
    return "\n".join(lines) if lines else "- none at this size"


def render(results: dict) -> str:
    m = results["machine"]
    rows = []
    rows.append("| facts | mode | p50 ms | p95 ms | p99 ms | tokens/recall (mean) | hits | gold in top-10 |")
    rows.append("|---:|---|---:|---:|---:|---:|---:|---:|")
    for cell in results["cells"]:
        for mode, r in cell["recall"].items():
            rows.append(f"| {cell['facts']:,} | {mode} | {r['p50_ms']} | {r['p95_ms']} | {r['p99_ms']} "
                        f"| {r['tokens_mean']} | {r['hits_mean']} | {r['gold_in_top10'] if r['gold_in_top10'] is not None else 'n/a'} |")
    wrows = ["| facts | cold start (s) | RSS after index (MB) | write p50 ms | write p95 ms | write p99 ms | writes that reconciled |",
             "|---:|---:|---:|---:|---:|---:|---:|"]
    for cell in results["cells"]:
        w = cell["write"]
        wrows.append(f"| {cell['facts']:,} | {cell['cold_start_s']} | {cell['rss_mb_after_index']} "
                     f"| {w['p50_ms']} | {w['p95_ms']} | {w['p99_ms']} | {w['reconcile_updates']}/{w['n']} |")
    return f"""# Recall and write latency: synthetic corpus, offline embedder

Generated by `benchmarks/latency/run.sh` from `results.json`. Numbers below are
copied from that file; re-run to regenerate.

## Setup

- Machine: {m['cpu_count']} logical CPUs ({m['cpu_model']}), {m['mem_total_gb']} GB RAM, kernel {m['kernel']}.
- {m['go']}; Python {m['python']}.
- Server: the grimoire binary built from this worktree (`go build ./cmd/grimoire`), started
  locally on 127.0.0.1 against a fresh vault per corpus size.
- Embedder: `{results['cells'][0]['embedder']}` (local model2vec, loaded from the Hugging Face
  cache with `HF_HUB_OFFLINE=1`; no network, no Ollama).
- Corpus: seed {results['seed']}, synthetic, generated by `corpus.py`. Supersession ~10%, human
  authorship ~25%, validity ranges ~20%, importance unrated for ~20%.
- Run time: {results['total_wall_s']:.0f} s for all sizes (load average {m['loadavg_1m_at_start']:.1f} at
  start, {results.get('loadavg_1m_at_end', 0):.1f} at end). Above the ~10 min target: recall cost grows with
  N, and the 50k cells dominate (cold start alone is {results['cells'][-1]['cold_start_s']:.0f} s).
- Each recall cell: 20 warm-up queries discarded, then {results['queries']} measured sequential
  requests over one keep-alive connection. Writes: 20 warm-up, then {results['queries']} measured.
- Tokens: the project's estimator (`bank.CountTokens`, ported to Python and checked against the
  Go source) summed over the hit texts at the server's default limit (20).
  `gold in top-10` (plain/expand/hops only) is a sanity check that the query finds its target
  belief; it is not a quality score. It is n/a for valid_at/as_of, which may filter the target out.

## Recall latency (client-observed)

{chr(10).join(rows)}

Modes: `plain` = `GET /api/memory?q=`; `expand` adds `expand=true`; `hops1` adds `hops=1`;
`valid_at` adds `valid_at=2026-06-01`; `as_of` adds `as_of=<timestamp of the middle fact>`.

## Write latency and start-up

`POST /api/memory`, which runs the reconcile path. "Writes that reconciled" counts responses
whose op was UPDATE/SUPERSEDE/DELETE rather than a plain ADD.

{chr(10).join(wrows)}

## Findings

{findings(results)}

Both are properties of the server as it stands, not of the harness. They were not changed here,
because this benchmark is scoped to `benchmarks/latency/`.

## Caveats

- **Synthetic corpus.** Fact text comes from a small set of templates and vocabularies, so the
  lexical overlap between a question and its target is higher than in real notes. Latency
  depends on the candidate set size and vector work, which this roughly reproduces, but the
  absolute numbers will not transfer to a real vault.
- **Writes.** Each write goes to one of 50 topic notes. A single note that keeps growing makes each
  write slower (about 35 ms into a fresh note, about 1.3 s at ~350 entries in one note, from an
  ad-hoc probe), so note growth is a cost the benchmark deliberately does not measure here.
- **Offline embedder.** model2vec is a static embedding model. A neural embedder (or Ollama)
  would change query-embedding cost and the retrieval set. Only the offline path was measured.
- **Single machine, single client, sequential requests.** No concurrency. The machine is shared
  with other workloads that are not controlled: load average was 3 to 36 across the session
  (recorded at start and end in `results.json`), and read and write latencies moved with it.
  Re-run on an idle box before comparing against another build.
  Percentiles at n=200 are noisy at p99: treat p99 as indicative, and re-run before comparing
  two builds by a few percent.
- **Client-observed time** includes Python's HTTP client and JSON parsing, so it sits above the
  server-side cost.
- **Tokens use a port of the estimator** (`tokens.py`) rather than the Go function itself. The
  port is checked against the Go source on a fixed sample by `run.sh`; it is not an exact BPE
  count, and the project documents it as an estimate.
- **RSS** is the server process's VmRSS at the named point, not a peak.
- **No quality claim.** `gold in top-10` only confirms the path works. Retrieval quality is
  measured by the LoCoMo and LongMemEval benchmarks, not here.
"""


def main() -> int:
    ap = argparse.ArgumentParser()
    ap.add_argument("--binary", required=True, type=Path)
    ap.add_argument("--work", required=True, type=Path)
    ap.add_argument("--out", required=True, type=Path)
    ap.add_argument("--sizes", default=",".join(map(str, DEFAULT_SIZES)))
    ap.add_argument("--port", type=int, default=19311)
    args = ap.parse_args()
    args.work.mkdir(parents=True, exist_ok=True)
    sizes = [int(x) for x in args.sizes.split(",")]
    t_all = time.perf_counter()
    cells = []
    for n in sizes:
        print(f"== N={n:,}", flush=True)
        cells.append(run_size(args.binary, args.work, n, args.port))
    results = {
        "seed": SEED, "queries": N_QUERIES, "warmup": N_WARMUP,
        "corpus_version": corpus.CORPUS_VERSION, "machine": machine(),
        "total_wall_s": round(time.perf_counter() - t_all, 1),
        "loadavg_1m_at_end": os.getloadavg()[0], "cells": cells,
    }
    args.out.mkdir(parents=True, exist_ok=True)
    (args.out / "results.json").write_text(json.dumps(results, indent=2) + "\n")
    (args.out / "REPORT.md").write_text(render(results))
    print(f"wrote {args.out / 'results.json'} and REPORT.md in {results['total_wall_s']}s")
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
