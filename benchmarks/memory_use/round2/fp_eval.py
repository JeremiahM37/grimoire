"""Offline measurement of tag-free use detection (fingerprints). No model calls.

Selection runs through the production Go code (go/cmd/fpdump); matching runs
through the production hook code (clients/hooks/grimoire_outcome.py), with the
same salted hashes the wire carries. Data: the adherence cases, the qwen
marker runs (arms N = no memory, A-D = memory), the Haiku marker runs, and the
Haiku adherence arms (none/current/directive/recheck, LLM-graded) plus the
long-horizon Haiku answers.

usage: fp_eval.py [--fpdump PATH] [--min-score X] [--max-df N] [--k 1]
"""
import argparse, importlib.util, json, os, re, subprocess, sys
from collections import Counter, defaultdict
from common import BENCH, CASES, HAIKU, judge

ROOT = os.path.abspath(os.path.join(os.path.dirname(__file__), '..', '..', '..'))
spec = importlib.util.spec_from_file_location('outcome_hook', os.path.join(ROOT, 'clients/hooks/grimoire_outcome.py'))
hook = importlib.util.module_from_spec(spec); spec.loader.exec_module(hook)
SALT = 'eval-salt-0123456789abcdef'

ap = argparse.ArgumentParser()
ap.add_argument('--fpdump', default='/tmp/claude-1000/fpdump')
ap.add_argument('--min-score', type=float, default=0)
ap.add_argument('--max-df', type=int, default=0)
ap.add_argument('--k', type=int, default=1, help='fingerprints that must match to count as used')
ap.add_argument('--show', action='store_true')
args = ap.parse_args()

cases = json.load(open(CASES))
by_id = {c['mem_id']: c for c in cases}
chk = json.load(open(BENCH + '/checks_final.json'))
SHOWN = 900  # the benchmark shows each memory truncated to this many characters
corpus = sorted({c['mem_text'][:SHOWN] for c in cases} | {d[:SHOWN] for c in cases for d in c['distractors']})
req = {'corpus': corpus, 'min_score': args.min_score, 'max_df': args.max_df,
       'items': [{'ID': c['mem_id'], 'Text': c['mem_text'][:SHOWN], 'Situation': c['task']} for c in cases]}
out = subprocess.run([args.fpdump], input=json.dumps(req), capture_output=True, text=True, check=True).stdout
fps = json.loads(out)
hashes = {m: {hook.fp_hash(SALT, f['token']) for f in v} for m, v in fps.items()}

def matches(mem, answer):
    if not hashes[mem]:
        return None  # unfingerprintable: no verdict
    have = {hook.fp_hash(SALT, t) for t in hook.candidates(answer or '')}
    return len(hashes[mem] & have)

n_fp = sum(1 for m in hashes if hashes[m])
print(f'corpus {len(corpus)} memory texts; {len(cases)} cases')
cnt = Counter(len(v) for v in fps.values())
print(f'coverage: {n_fp}/{len(cases)} cases fingerprintable ({100*n_fp/len(cases):.1f}%); fingerprints per case {dict(sorted(cnt.items()))}')
print('kinds:', dict(Counter(f['kind'] for v in fps.values() for f in v)))
if args.show:
    for c in cases[:40]:
        print(' ', c['mem_id'], [f['token'] for f in fps[c['mem_id']]])

def pct(a, b): return f'{100*a/b:.1f}%' if b else 'n/a'
def rate(rows):  # rows: (used, label)
    return sum(1 for u, _ in rows if u)

def report(name, rows, label_name):
    """rows: list of (used:bool, positive:bool) over fingerprintable cases."""
    n = len(rows); tp = sum(u and p for u, p in rows); fpn = sum(u and not p for u, p in rows)
    fn = sum((not u) and p for u, p in rows); tn = n - tp - fpn - fn
    prec = tp / (tp + fpn) if tp + fpn else float('nan'); rec = tp / (tp + fn) if tp + fn else float('nan')
    print(f'  {name:<18} vs {label_name:<10} n={n:<4} used={pct(tp+fpn,n):>6} positive={pct(tp+fn,n):>6} '
          f'precision={prec*100:5.1f}% recall={rec*100:5.1f}%  (tp {tp} fp {fpn} fn {fn} tn {tn})')

print(f'\n== used := >= {args.k} fingerprint(s) matched, among fingerprintable cases ==')
# --- qwen marker runs (deterministic follow grade, right-cite)
R = [json.loads(l) for l in open(BENCH + '/marker_runs.jsonl')]
def cite(r):
    tags = set(re.findall(r'm:([0-9a-f]{4})', r['answer'])); tags.discard('3e99')
    return 'right' if r['tag'] in tags else ('wrong' if tags else 'none')
print('\nqwen marker runs (3 reps x 101 cases):')
nomem = []
agg = defaultdict(list)
for r in R:
    m = matches(r['mem_id'], r['answer'])
    if m is None: continue
    used = m >= args.k
    ck = chk.get(r['mem_id'])
    g = judge(ck, r['answer']) if ck and ck['gradeable'] else None
    if r['variant'] == 'N':
        nomem.append(used)
    else:
        agg[('follow', r['variant'])].append((used, g == 'FOLLOWS')) if g else None
        if r['variant'] in 'BCD':
            agg[('cite', r['variant'])].append((used, cite(r) == 'right'))
        agg[('follow', 'A-D')].append((used, g == 'FOLLOWS')) if g else None
for v in ['A', 'B', 'C', 'D', 'A-D']:
    if ('follow', v) in agg: report('arm ' + v, agg[('follow', v)], 'follow')
for v in 'BCD':
    report('arm ' + v, agg[('cite', v)], 'right-cite')
allc = [x for v in 'BCD' for x in agg[('cite', v)]]
report('arms B-D', allc, 'right-cite')
mem_used = [u for (k, v), rows in agg.items() if k == 'follow' and v in 'ABCD' and len(v) == 1 for u, _ in rows]
print(f'  used-rate with memory (A-D) {pct(sum(mem_used), len(mem_used))} vs NO-MEMORY arm N {pct(sum(nomem), len(nomem))} '
      f'(false-positive rate on {len(nomem)} no-memory answers)')
# follow label restricted to gradeable cases in N
nof = [(matches(r['mem_id'], r['answer']) >= args.k, judge(chk[r['mem_id']], r['answer']) == 'FOLLOWS')
       for r in R if r['variant'] == 'N' and matches(r['mem_id'], r['answer']) is not None
       and chk.get(r['mem_id'], {}).get('gradeable')]
print(f'  N arm: of answers that follow the rule anyway (no memory), used fires on {pct(sum(u for u,p in nof if p), sum(p for _,p in nof))}; '
      f'of answers that do not follow, on {pct(sum(u for u,p in nof if not p), sum(not p for _,p in nof))}')

# --- Haiku marker runs
H = [json.loads(l) for l in open(BENCH + '/marker_runs_haiku.jsonl')]
rows, rc = [], []
for r in H:
    m = matches(r['mem_id'], r['answer'])
    if m is None: continue
    ck = chk.get(r['mem_id']); g = judge(ck, r['answer']) if ck and ck['gradeable'] else None
    if g: rows.append((m >= args.k, g == 'FOLLOWS'))
    if r['variant'] == 'B': rc.append((m >= args.k, cite(r) == 'right'))
print('\nHaiku marker runs (A,B):')
report('A+B', rows, 'follow'); report('B', rc, 'right-cite')

# --- Haiku adherence arms (LLM-graded FOLLOWS)
res = json.load(open(HAIKU))
print('\nHaiku adherence arms (LLM-graded; none = no-memory arm):')
for arm in ['none', 'current', 'directive', 'recheck']:
    rows = []
    for r in res:
        m = matches(r['mem_id'], r[arm])
        if m is None: continue
        rows.append((m >= args.k, r['grade_' + arm] == 'FOLLOWS'))
    if arm == 'none':
        print(f'  none (no memory)   used fires on {pct(sum(u for u,_ in rows), len(rows))} of {len(rows)} answers  <- false-positive rate')
        report('none', rows, 'follow')
    else:
        report(arm, rows, 'follow')
mem = [(matches(r['mem_id'], r[a]) >= args.k, r['grade_' + a] == 'FOLLOWS') for r in res for a in ['current', 'directive', 'recheck']
       if matches(r['mem_id'], r[a]) is not None]
report('current+dir+recheck', mem, 'follow')
lr = json.load(open('/mnt/bulk/memory-use-research/adherence/long_results.json'))
rows = []
for r in lr:
    for a in ['start', 'action']:
        m = matches(r['mem_id'], r[a])
        if m is not None: rows.append((m >= args.k, r['grade_' + a] == 'FOLLOWS'))
print('\nHaiku long-horizon answers:'); report('start+action', rows, 'follow')
