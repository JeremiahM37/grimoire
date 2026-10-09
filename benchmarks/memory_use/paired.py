"""Paired comparison of two harness result files on the same cases."""
import json
import sys
from math import comb

a, b = json.load(open(sys.argv[1])), json.load(open(sys.argv[2]))
for kind in ('pos_prompt', 'pos_action', 'neg_prompt'):
    pairs = [(x['hit'], y['hit']) for x, y in zip(a, b, strict=False) if x['kind'] == kind and not x['mem_id'].startswith('cm:')]
    if not pairs: continue
    up = sum(1 for x, y in pairs if y and not x); down = sum(1 for x, y in pairs if x and not y)
    n = up + down; k = min(up, down)
    p = min(1, 2 * sum(comb(n, i) for i in range(k + 1)) / 2 ** n) if n else 1
    print(f"  {kind:10s} {sum(x for x,_ in pairs)}->{sum(y for _,y in pairs)} of {len(pairs)}  (gained {up}, lost {down}, p={p:.3f})")
