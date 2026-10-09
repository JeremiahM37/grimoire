"""Agreement of deterministic checks with the Haiku-graded answers (4 arms x 101). Per-case table."""
import json, sys
from common import *
f = sys.argv[1]
ARMS = sys.argv[2].split(',') if len(sys.argv) > 2 else ['none','current','directive','recheck']
chk = json.load(open(f)); H = json.load(open(HAIKU)); cases = {c['mem_id']: c for c in json.load(open(CASES))}
tot = agr = dec = 0; per = {}
conf = {}
for r in H:
    ck = chk[r['mem_id']]
    for a in ARMS:
        h = r['grade_' + a]; d = judge(ck, r[a]) if ck['follow'] else 'UNCLEAR'
        conf[(h, d)] = conf.get((h, d), 0) + 1
        if h != 'UNCLEAR':   # Haiku decided
            p = per.setdefault(r['mem_id'], [0, 0]); p[1] += 1
            p[0] += (d == h)
            dec += 1; agr += (d == h)
print('confusion (haiku, det):'); [print(' ', k, v) for k, v in sorted(conf.items())]
print(f'agreement where Haiku decided: {agr}/{dec} = {agr/dec:.3f}')
bad = {k: v for k, v in per.items() if v[0] / v[1] < 0.75}
print('cases below 75%:', len(bad)); json.dump(per, open(BENCH + '/per_case_agreement.json', 'w'))
for k, v in bad.items(): print(' ', k, v)
