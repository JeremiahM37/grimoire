"""Learned-cue experiment (difference in differences).

1. Split the benchmark memories at random into TREATED and CONTROL halves.
2. Replay each round-1 prompt. When a TREATED memory is missed, teach it the
   prompt as a learned cue (what a re-tell does in production).
3. Score round-2 prompts (different situations, never taught) before and after.
   The change for TREATED minus the change for CONTROL is the effect of
   learning; CONTROL absorbs anything else that moved. False fires on round-2
   negatives are tracked the same way.
usage: learn_exp.py <base_url> [k=v ...]   (server must be fresh: no learned cues yet)"""
import hashlib
import json
import sys
import urllib.parse
import urllib.request

sys.path.insert(0, '.')
from harness import run

base = sys.argv[1]
extra = dict(a.split('=', 1) for a in sys.argv[2:])
extra.setdefault('rank', 'hybrid')
def treated(mid):
    return int(hashlib.sha1(mid.encode()).hexdigest(), 16) % 2 == 0

def target(mid):
    store, key = mid.split(':', 1)
    return {'am': 'note:Agent Memory/' + key.split('#')[0] + '.md', 'gm': 'fact:' + key,
            'cm': 'note:Instructions/cm_' + key + '.md'}[store]

def score(cases):
    res = [run(c, base, extra) for c in cases]
    out = {}
    for grp in ('treated', 'control'):
        for kind in ('pos_prompt', 'neg_prompt'):
            rs = [r for r in res if r['kind'] == kind and treated(r['mem_id']) == (grp == 'treated')
                  and not r['mem_id'].startswith('cm:')]
            out[(grp, kind)] = (sum(r['hit'] for r in rs), len(rs))
    return out

r1 = [c for c in json.load(open('cases.json')) if c['kind'] == 'pos_prompt']
r2 = json.load(open('cases2.json'))
before = score(r2)
taught = 0
for c in r1:
    if c['mem_id'].startswith('cm:') or not treated(c['mem_id']):
        continue
    if run(c, base, extra)['hit']:
        continue
    body = json.dumps({'target': target(c['mem_id']), 'cues': [c['prompt'][:300]], 'source': 'learned'}).encode()
    req = urllib.request.Request(base + '/api/memory/cues', body, {'Content-Type': 'application/json'})
    try:
        urllib.request.urlopen(req, timeout=30); taught += 1
    except Exception as e:
        print('teach failed', c['mem_id'], e)
after = score(r2)
print('learned cues taught:', taught)
for kind in ('pos_prompt', 'neg_prompt'):
    for grp in ('treated', 'control'):
        (b, n), (a, _) = before[(grp, kind)], after[(grp, kind)]
        print(f'{kind:10s} {grp:8s} before {b}/{n} = {b/n:.3f}  after {a}/{n} = {a/n:.3f}  change {(a-b)/n:+.3f}')
