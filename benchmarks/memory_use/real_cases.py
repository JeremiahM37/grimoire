"""Replay real re-tell cases from the transcripts: would the task prompt the
agent had have surfaced the memory the user later restated?
usage: real_cases.py <base_url> <name> [k=v ...]"""
import json
import sys

sys.path.insert(0, '.')
from harness import gold_match, query

base, name = sys.argv[1], sys.argv[2]
extra = dict(a.split('=', 1) for a in sys.argv[3:])
extra.setdefault('format', 'json')
cases = [json.loads(line) for line in open('../data/eval_cases.jsonl')]
cases = [c for c in cases if c['failure_class'] in ('F1', 'F3') and c['label_quality'] == 'verified']
hits = 0
for c in cases:
    q = (c['context_before'].get('task_prompt') or '')[:2000]
    if not q:
        continue
    items, _, _ = query(base, {'q': q, **extra})
    hit = any(gold_match(g, i) for g in c['gold_memory_ids'] for i in items)
    hits += hit
print(f"{name:20s} real verified cases: gold surfaced {hits}/{len(cases)}")
