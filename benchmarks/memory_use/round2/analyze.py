"""Grade marker_runs.jsonl deterministically; rates + exact McNemar vs A; qwen-vs-Haiku on A."""
import json, re
from math import comb
from common import *
chk = json.load(open(BENCH + '/checks_final.json'))
R = [json.loads(l) for l in open(BENCH + '/marker_runs.jsonl')]
def cite(r):
    tags = set(re.findall(r'm:([0-9a-f]{4})', r['answer']))
    tags.discard('3e99')
    if not tags: return 'none'
    return 'right' if r['tag'] in tags else 'wrong'
for r in R:
    r['cite'] = cite(r); ck = chk[r['mem_id']]
    r['grade'] = judge(ck, r['answer']) if ck['gradeable'] else None
def mcnemar(a, b):  # lists of bools, paired; exact two-sided
    n10 = sum(x and not y for x, y in zip(a, b)); n01 = sum(y and not x for x, y in zip(a, b)); n = n10 + n01
    if n == 0: return 1.0
    k = min(n10, n01); return min(1.0, 2 * sum(comb(n, i) for i in range(k + 1)) / 2 ** n)
idx = {(r['mem_id'], r['rep'], r['variant']): r for r in R}
keys = sorted({(r['mem_id'], r['rep']) for r in R if r['variant'] == 'A'})
gk = [k for k in keys if chk[k[0]]['gradeable']]
def vec(v, ks, f): return [f(idx[(m, p, v)]) for m, p in ks if (m, p, v) in idx]
print(f'gradeable cases: {len({k[0] for k in gk})}; pairs: follow/violate n={len(gk)}, cite n={len(keys)}')
print('variant | follow | violate | right-cite | wrong-cite | no-cite | p(follow) | p(violate) | p(right-cite)  (p = exact McNemar vs A)')
for v in 'NABCD':
    ks = [k for k in gk if (k[0], k[1], v) in idx]; kc = [k for k in keys if (k[0], k[1], v) in idx]
    if not ks: continue
    f = vec(v, ks, lambda r: r['grade'] == 'FOLLOWS'); vi = vec(v, ks, lambda r: r['grade'] == 'VIOLATES')
    rc = vec(v, kc, lambda r: r['cite'] == 'right'); wc = vec(v, kc, lambda r: r['cite'] == 'wrong'); nc = vec(v, kc, lambda r: r['cite'] == 'none')
    s = lambda x: f'{100*sum(x)/len(x):.1f}%'
    if v in 'BCD':
        fa = vec('A', ks, lambda r: r['grade'] == 'FOLLOWS'); va = vec('A', ks, lambda r: r['grade'] == 'VIOLATES'); ra = vec('A', kc, lambda r: r['cite'] == 'right')
        p = (f'{mcnemar(fa, f):.3f}', f'{mcnemar(va, vi):.3f}', f'{mcnemar(ra, rc):.3f}')
    else: p = ('-', '-', '-')
    print(f'{v} | {s(f)} | {s(vi)} | {s(rc)} | {s(wc)} | {s(nc)} | {p[0]} | {p[1]} | {p[2]}  (n={len(f)}/{len(rc)})')
# qwen A vs Haiku directive (same preamble, no tags, 3 distractors)
H = {r['mem_id']: r for r in json.load(open(HAIKU))}
gc = sorted({k[0] for k in gk})
qa = {m: sum(idx[(m, p, 'A')]['grade'] == 'FOLLOWS' for p in range(3) if (m, p, 'A') in idx) / 3 for m in gc}
hd = {m: judge(chk[m], H[m]['directive']) == 'FOLLOWS' for m in gc}
hl = {m: H[m]['grade_directive'] == 'FOLLOWS' for m in gc}
print('\nQwen A vs Haiku directive on gradeable cases (n=%d)' % len(gc))
print(f'follow rate qwen {100*sum(qa.values())/len(gc):.1f}%, haiku(det) {100*sum(hd.values())/len(gc):.1f}%, haiku(LLM-graded) {100*sum(hl.values())/len(gc):.1f}%')
agree = sum((qa[m] >= .5) == hd[m] for m in gc); print(f'per-case majority agreement qwen/haiku follow: {agree}/{len(gc)}')
xs = [qa[m] for m in gc]; ys = [float(hd[m]) for m in gc]; mx, my = sum(xs)/len(xs), sum(ys)/len(ys)
cov = sum((x-mx)*(y-my) for x, y in zip(xs, ys)); den = (sum((x-mx)**2 for x in xs)*sum((y-my)**2 for y in ys))**.5
print(f'pearson r (qwen follow rate vs haiku follow) = {cov/den if den else float("nan"):.2f}')
# all-101 LLM-graded haiku A vs qwen A with det can't; report haiku none vs qwen N
if any(r['variant'] == 'N' for r in R):
    qn = sum(idx[(m, p, 'N')]['grade'] == 'FOLLOWS' for m in gc for p in range(3) if (m, p, 'N') in idx) / (3*len(gc))
    hn = sum(judge(chk[m], H[m]['none']) == 'FOLLOWS' for m in gc) / len(gc)
    print(f'no-memory follow: qwen {100*qn:.1f}% vs haiku(det) {100*hn:.1f}%')
