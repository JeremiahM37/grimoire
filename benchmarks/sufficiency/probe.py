#!/usr/bin/env python3
"""Feasibility probe: can retrieval tell answerable from unanswerable?

LoCoMo ships 446 category-5 questions — adversarial, unanswerable, and
EXCLUDED from every published memory-system evaluation (including ours)
because they have no gold answer to score against. They are not nonsense:
they name real people and real topics from the conversation and ask for a
fact that was never stated. That makes them the sharpest available test of
whether a retrieval layer can tell RELEVANT context from SUFFICIENT context.

This probe scores nothing with a reader or a judge. It asks one question of
the retrieval layer directly, per query, and the answer is deterministic —
which is the point: the LoCoMo/LongMemEval harnesses cannot resolve small
differences any more (8-12% of answers flip on byte-identical input), and no
reader is involved here at all.

Usage:
    python probe.py --binary ../../go/grimoire --n 200
"""
from __future__ import annotations

import argparse
import collections
import json
import math
import random
import re
import sys
import urllib.parse
import urllib.request
from pathlib import Path

HERE = Path(__file__).resolve().parent
sys.path.insert(0, str(HERE.parent))
sys.path.insert(0, str(HERE.parent / "locomo"))

import goserver  # noqa: E402
from run_locomo import load_data, session_docs  # noqa: E402

SEED = 42  # same seed the LoCoMo harness freezes its sample with

WORD = re.compile(r"[a-z0-9']+")
# Stopwords carry no evidence, so they must not count toward "is this question
# supported": every question contains them and they would wash the signal out.
STOP = set("""a an the and or but if then than that this these those of to in on at
for with from by as is are was were be been being it its it's he she they them his her
their we you i me my your our do does did done have has had what when where who whom
which why how much many about after before during over under again further once here
there all any both each few more most other some such no nor not only own same so too
very can will just should now what's who's did didn't does doesn't""".split())


def terms(text: str) -> list[str]:
    return [w for w in WORD.findall(text.lower()) if w not in STOP and len(w) > 1]


def api(base: str, path: str):
    with urllib.request.urlopen(base + path, timeout=300) as r:
        raw = r.read().decode()
    return json.loads(raw) if raw.strip() else {}


def build_vault(vault: Path, conv) -> None:
    """One note per session, matching the LoCoMo harness. The sessions live
    under conv["conversation"], not at the top level — the top level is qa,
    summaries and the sample id."""
    vault.mkdir(parents=True, exist_ok=True)
    for p in vault.glob("*.md"):
        p.unlink()
    for n, when, text in session_docs(conv["conversation"]):
        (vault / f"session-{n:02d}.md").write_text(
            f"---\ntitle: Conversation session {n} ({when})\n---\n"
            f"Date: {when}\n\n{text}\n", encoding="utf-8")


def corpus_idf(vault: Path) -> tuple[dict[str, float], int]:
    """IDF over the session notes, so a term's weight reflects how rare it is
    in THIS conversation rather than in English at large."""
    df: collections.Counter = collections.Counter()
    docs = list(vault.glob("*.md"))
    for p in docs:
        for t in set(terms(p.read_text(encoding="utf-8"))):
            df[t] += 1
    n = max(len(docs), 1)
    return {t: math.log(1 + n / (1 + c)) for t, c in df.items()}, n


def signals(base: str, question: str, idf: dict[str, float]) -> dict:
    """Everything a caller could compute today, per query, for free."""
    q = urllib.parse.quote(question)
    hits = api(base, f"/api/retrieve?q={q}&k=10")
    scores = [h.get("score", 0.0) for h in hits]
    cos = [h.get("cosine", 0.0) for h in hits]
    lex = [h.get("lexical", 0.0) for h in hits]
    text = " ".join(h.get("chunk", "") for h in hits).lower()
    have = set(terms(text))

    qt = terms(question)
    seen_w = sum(idf.get(t, math.log(2)) for t in qt if t in have)
    total_w = sum(idf.get(t, math.log(2)) for t in qt) or 1.0
    # Rare terms only: a question's discriminative words are the ones that say
    # what it is actually asking for. "What did Caroline realize" shares
    # "Caroline" with the whole conversation; "realize" is the ask.
    rare = sorted(qt, key=lambda t: -idf.get(t, math.log(2)))[:3]
    rare_hit = sum(1 for t in rare if t in have) / (len(rare) or 1)

    def gap(v):
        return (v[0] - v[1]) if len(v) > 1 else 0.0

    return {
        "n_hits": len(hits),
        # The standard production signal the literature calls a "fixed
        # similarity threshold": the best chunk's cosine.
        "top_cosine": cos[0] if cos else 0.0,
        "max_cosine": max(cos) if cos else 0.0,
        "mean_cosine": (sum(cos) / len(cos)) if cos else 0.0,
        # Concentration: does one chunk stand out, or is the field flat?
        "cosine_gap": gap(sorted(cos, reverse=True)),
        "top_lexical": lex[0] if lex else 0.0,
        "max_lexical": max(lex) if lex else 0.0,
        "lexical_gap": gap(sorted(lex, reverse=True)),
        # Agreement: do the two legs pick the same chunk as best? When they
        # disagree, neither has strong evidence.
        "legs_agree": float(bool(cos and lex and
                                 cos.index(max(cos)) == lex.index(max(lex)))),
        "top_score": scores[0] if scores else 0.0,
        "score_margin": (scores[0] - scores[-1]) if len(scores) > 1 else 0.0,
        "score_spread": (max(scores) - min(scores)) if scores else 0.0,
        "term_coverage": seen_w / total_w,
        "rare_term_coverage": rare_hit,
        "n_query_terms": len(qt),
    }


def main() -> int:
    ap = argparse.ArgumentParser()
    ap.add_argument("--binary", default=str(HERE.parent.parent / "go" / "grimoire"))
    ap.add_argument("--port", type=int, default=9131)
    ap.add_argument("--vault", default="/tmp/sufficiency-vault")
    ap.add_argument("--embed", default="auto")
    ap.add_argument("--n", type=int, default=100, help="questions per class per conversation cap")
    ap.add_argument("--out", default=str(HERE / "signals.jsonl"))
    args = ap.parse_args()

    data = load_data()
    out = Path(args.out).open("w")
    written = collections.Counter()

    for ci, conv in enumerate(data):
        qa = conv.get("qa", [])
        answerable = [q for q in qa if int(q.get("category", 0)) in (1, 2, 3, 4)]
        unanswerable = [q for q in qa if int(q.get("category", 0)) == 5]
        if not unanswerable:
            continue
        # Balanced per conversation: the class prior is a property of the
        # dataset, not of the signal, and an unbalanced probe would let a
        # constant predictor look good.
        #
        # The answerable side is sampled STRATIFIED by category, not taken in
        # file order. Taking the first N caught 4 single-hop questions out of
        # 249 — of the category that is 55% of LoCoMo — because the file is
        # not ordered randomly. The finding survived that, but a sample that
        # under-represents the dominant category by two orders of magnitude is
        # not one to report from.
        take = min(len(unanswerable), args.n)
        by_cat = collections.defaultdict(list)
        for q in answerable:
            by_cat[int(q["category"])].append(q)
        total = sum(len(v) for v in by_cat.values()) or 1
        rng = random.Random(SEED)
        sampled: list = []
        for cat in sorted(by_cat):
            want = round(take * len(by_cat[cat]) / total)
            sampled += rng.sample(by_cat[cat], min(want, len(by_cat[cat])))
        # Rounding can leave the two classes uneven; top up or trim to match.
        pool = [q for q in answerable if q not in sampled]
        while len(sampled) < take and pool:
            sampled.append(pool.pop(rng.randrange(len(pool))))
        answerable = sampled[:take]
        unanswerable = rng.sample(unanswerable, take)

        vault = Path(args.vault)
        build_vault(vault, conv)
        idf, ndocs = corpus_idf(vault)
        base = f"http://127.0.0.1:{args.port}"
        with goserver.launch(args.binary, vault, args.port, embed=args.embed):
            for label, qs in (("answerable", answerable), ("unanswerable", unanswerable)):
                for qi, q in enumerate(qs):
                    rec = signals(base, q["question"], idf)
                    rec.update(conv=ci, qid=f"c{ci}q{label[:3]}{qi}", label=label,
                               category=int(q.get("category", 0)),
                               question=q["question"], ndocs=ndocs)
                    out.write(json.dumps(rec) + "\n")
                    written[label] += 1
        print(f"conversation {ci}: {written['answerable']} answerable / "
              f"{written['unanswerable']} unanswerable so far", flush=True)
    out.close()
    print(f"wrote {sum(written.values())} records to {args.out}")
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
