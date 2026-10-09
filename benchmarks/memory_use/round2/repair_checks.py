"""Second qwen pass: show it Haiku-labelled answers from the TUNE arms (none, current) only, ask for revised regexes.
Held-out arms (directive, recheck) are never shown. Writes checks_v2.json."""
import json, re
from common import *
cases = {c['mem_id']: c for c in json.load(open(CASES))}
H = {r['mem_id']: r for r in json.load(open(HAIKU))}
chk = json.load(open(BENCH + '/checks_v1.json'))
P = """You wrote regex checks to grade whether an agent's one-line answer follows a memory rule.
Rule: {check}
FOLLOWING looks like: {right}
VIOLATING looks like: {wrong}
Your current checks (follow = ANY regex matches; violate = ANY matches; both or neither = UNCLEAR): {cur}

A careful reviewer labelled these real answers:
{ex}

Revise the checks so that they reproduce the reviewer's FOLLOWS and VIOLATES labels, and give UNCLEAR for UNCLEAR ones, while staying general (do not copy whole sentences; use short tokens/stems that capture the decisive action, 2-6 regexes each). Output JSON only: {{"follow": [...], "violate": [...]}}"""
out = {}
for mid, c in cases.items():
    h = H[mid]; ck = chk[mid]
    ex = ''
    for a in ('none', 'current'):
        ex += f"[{h['grade_'+a]}] {h[a][:500]!r}\n"
    r = chat(P.format(check=c['check'], right=c['right'], wrong=c['wrong'], cur=json.dumps({k: ck[k] for k in ('follow', 'violate')}), ex=ex),
             system='You write precise regexes. JSON only.', temperature=0, num_predict=700)
    m = re.search(r'\{.*\}', r, re.S)
    try:
        j = json.loads(m.group(0)); re.compile('|'.join(j['follow'] + j['violate']))
        out[mid] = {'follow': j['follow'], 'violate': j['violate']}
    except Exception as e:
        out[mid] = {'follow': ck['follow'], 'violate': ck['violate'], 'error': str(e)}
    print(mid, flush=True)
json.dump(out, open(BENCH + '/checks_v2.json', 'w'), indent=1)
