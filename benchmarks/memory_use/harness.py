"""Replay benchmark: does the memory path surface the gold memory at the right moment?

usage: harness.py <name> <base_url> [extra query params as k=v ...]
Each case is sent to /api/memory/context as the hook would: the user prompt for
pos_prompt/neg_prompt, and the pending tool input for pos_action (stage=action).
A case hits when the gold memory is among the injected items.
"""
import collections
import concurrent.futures as cf
import json
import sys
import time
import urllib.parse
import urllib.request


def gold_match(mem_id, item):
    store, key = mem_id.split(':', 1)
    if store == 'am':
        return item.get('path') == f"Agent Memory/{key.split('#')[0]}.md"
    if store == 'gm':
        return item.get('id') == key
    if store == 'cm':
        return item.get('path') == f"Instructions/cm_{key}.md"

def query(base, params):
    url = base + '/api/memory/context?' + urllib.parse.urlencode(params, doseq=True)
    t = time.time()
    with urllib.request.urlopen(url, timeout=30) as r:
        d = json.load(r)
    items = [json.loads(line) for line in d['context'].splitlines() if line.startswith('{')]
    return items, time.time() - t, d.get('bytes', 0)

def run(case, base, extra):
    if case['kind'] == 'pos_action':
        params = {'q': f"{case['tool']} {case['input']}", 'stage': 'action', 'tool': case['tool'], 'input': case['input'], 'cwd': case.get('cwd', '')}
    else:
        params = {'q': case['prompt'], 'cwd': case.get('cwd', '')}
    params.update(extra)
    params.setdefault('format', 'json')
    items, dt, nbytes = query(base, params)
    hit = any(gold_match(case['mem_id'], i) for i in items)
    return dict(kind=case['kind'], mem_id=case['mem_id'], hit=hit, n=len(items), bytes=nbytes, dt=dt)

def main():
    name, base = sys.argv[1], sys.argv[2]
    extra = dict(a.split('=', 1) for a in sys.argv[3:])
    import os
    cases = json.load(open(os.environ.get('CASES', 'cases.json')))
    with cf.ThreadPoolExecutor(6) as ex:
        res = list(ex.map(lambda c: run(c, base, extra), cases))
    json.dump(res, open(f'res_{name}.json', 'w'))
    report(name, res)

def report(name, res):
    by = collections.defaultdict(list)
    for r in res:
        store = r['mem_id'].split(':')[0]
        by[(r['kind'], 'instr' if store == 'cm' else 'mem')].append(r)
    def rate(rs): return sum(r['hit'] for r in rs) / max(1, len(rs))
    pp, pa, ng = by[('pos_prompt', 'mem')], by[('pos_action', 'mem')], by[('neg_prompt', 'mem')]
    lat = sorted(r['dt'] for r in res)
    print(f"{name:22s} prompt-hit {rate(pp):.3f} (n={len(pp)})  action-hit {rate(pa):.3f} (n={len(pa)})  "
          f"neg-fire {rate(ng):.3f}  instr prompt-hit {rate(by[('pos_prompt','instr')]):.3f}  "
          f"items/inj {sum(r['n'] for r in res)/len(res):.2f}  bytes {sum(r['bytes'] for r in res)/len(res):.0f}  "
          f"p50 {1000*lat[len(lat)//2]:.0f}ms p90 {1000*lat[int(.9*len(lat))]:.0f}ms")

if __name__ == '__main__':
    main()
