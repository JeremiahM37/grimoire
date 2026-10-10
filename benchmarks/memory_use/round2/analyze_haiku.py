"""Grade marker_runs_haiku.jsonl (deterministic checks + tag extraction); McNemar A vs B; compare with qwen on same cases."""
import json, re
from math import comb
from common import *
chk = json.load(open(BENCH + '/checks_final.json'))
H = [json.loads(l) for l in open(BENCH + '/marker_runs_haiku.jsonl')]
Q = [json.loads(l) for l in open(BENCH + '/marker_runs.jsonl')]
def cite(r):
    t = set(re.findall(r'm:([0-9a-f]{4})', r['answer'])); t.discard('3e99')
    return 'none' if not t else ('right' if r['tag'] in t else 'wrong')
def mcn(a, b):
    n10 = sum(x and not y for x, y in zip(a, b)); n01 = sum(y and not x for x, y in zip(a, b)); n = n10 + n01
    return 1.0 if n == 0 else min(1.0, 2 * sum(comb(n, i) for i in range(min(n10, n01) + 1)) / 2 ** n)
for r in H + Q:
    r['cite'] = cite(r); r['grade'] = judge(chk[r['mem_id']], r['answer'])
ids = sorted({r['mem_id'] for r in H}); idx = {(r['mem_id'], r['variant']): r for r in H}
s = lambda x: f'{100*sum(x)/len(x):.1f}%'
def rates(get, v):
    rs = get(v)
    return dict(f=[r['grade'] == 'FOLLOWS' for r in rs], v=[r['grade'] == 'VIOLATES' for r in rs],
                rc=[r['cite'] == 'right' for r in rs], wc=[r['cite'] == 'wrong' for r in rs], nc=[r['cite'] == 'none' for r in rs])
print(f'Haiku ({HMODEL if (HMODEL:="claude-haiku-5-5") else ""}) confirmatory sample: {len(ids)} gradeable cases x A,B x 1 rep')
print('variant | follow | violate | right-cite | wrong-cite | no-cite')
R = {}
for v in 'AB':
    R[v] = rates(lambda v: [idx[(m, v)] for m in ids], v)
    x = R[v]; print(f"Haiku {v} | {s(x['f'])} | {s(x['v'])} | {s(x['rc'])} | {s(x['wc'])} | {s(x['nc'])}")
print('exact McNemar A vs B: follow p=%.3f, violate p=%.3f, right-cite p=%.3f' % tuple(mcn(R['A'][k], R['B'][k]) for k in ('f', 'v', 'rc')))
print('discordant right-cite (A only / B only): %d / %d' % (sum(a and not b for a, b in zip(R['A']['rc'], R['B']['rc'])), sum(b and not a for a, b in zip(R['A']['rc'], R['B']['rc']))))
for v in 'AB':
    x = rates(lambda v: [r for r in Q if r['variant'] == v and r['mem_id'] in ids], v)
    print(f"qwen {v} (same {len(ids)} cases, 3 reps) | {s(x['f'])} | {s(x['v'])} | {s(x['rc'])} | {s(x['wc'])} | {s(x['nc'])}")
