"""Score round-2 band pairs (gold memory, gold score in [0.35,0.9)) with a cross-encoder and compare to Jev.
usage: gatemodel_eval.py MODEL_DIR [MODEL_DIR...]   (run with /mnt/bulk/laya/venv/bin/python)
Writes $GATEMODEL_DIR/eval_<name>.json (per-pair scores, reused by the Go parity test)."""
import sys, os, json, torch, time
sys.path.insert(0, os.path.dirname(__file__))
from gate_common import load, auc, OUT
from transformers import AutoTokenizer, AutoModelForSequenceClassification
D = os.environ.get('GATEMODEL_DIR', '/mnt/bulk/memory-use-research/round2/gatemodel')
def clip(s, n): s = ' '.join(s.split()); return s if len(s) <= n else s[:n]
d = load(); R = {}
for rnd in ('1', '2'):
    R[rnd] = [x for x in d if str(x['round']) == rnd and x['gold'] and .35 <= x['gold']['score'] < .9]
jev = {(a['prompt'], a['ph']): a['p'] for a in json.load(open(OUT + 'pairs_jev.json'))}
def op(pos, neg, thr): return 100 * sum(p >= thr for p in pos) / len(pos), 100 * sum(n < thr for n in neg) / len(neg)
def fit(pos, keep=.86):  # threshold keeping `keep` of the hits
    s = sorted(pos); return s[int((1 - keep) * len(s))]
def report(name, sc, rows):
    P = [s for s, x in zip(sc, rows) if x['kind'] == 'pos_prompt']; N = [s for s, x in zip(sc, rows) if x['kind'] == 'neg_prompt']
    thr = fit(P); k, r = op(P, N, thr)
    print(f"{name:34s} AUC {auc(P,N):.3f}  n={len(P)}/{len(N)}  thr {thr:.3f}: hits kept {k:.1f}% near-miss removed {r:.1f}%")
    return dict(name=name, auc=auc(P, N), thr=thr, kept=k, removed=r)
res = []
rows = R['2']
res.append(report('jev strict (p1)', [jev[(x['prompt'], 'p1')] for x in rows], rows))
for md in sys.argv[1:]:
    tok = AutoTokenizer.from_pretrained(md); m = AutoModelForSequenceClassification.from_pretrained(md).eval()
    def score(rs):
        out = []
        with torch.no_grad():
            for i in range(0, len(rs), 32):
                b = rs[i:i+32]
                e = tok([clip(x['prompt'], 240) for x in b], [clip(x['gold']['text'], 280) for x in b], truncation='longest_first', max_length=192, padding=True, return_tensors='pt')
                out += m(**e).logits.squeeze(-1).tolist()
        return out
    name = os.path.basename(md.rstrip('/')); name = 'untuned MiniLM' if 'ms-marco' in name else 'distilled ' + name
    s2 = score(rows); res.append(report(name, s2, rows))
    # threshold fitted on round 1 (in sample for the distilled model), applied to round 2
    s1 = score(R['1']); thr1 = fit([s for s, x in zip(s1, R['1']) if x['kind'] == 'pos_prompt'])
    P = [s for s, x in zip(s2, rows) if x['kind'] == 'pos_prompt']; N = [s for s, x in zip(s2, rows) if x['kind'] == 'neg_prompt']
    k, r = op(P, N, thr1); print(f"{'':34s} thr from round 1 ({thr1:.3f}) on round 2: hits kept {k:.1f}% near-miss removed {r:.1f}%")
    json.dump([dict(prompt=clip(x['prompt'], 240), mem=clip(x['gold']['text'], 280), kind=x['kind'], s=s) for x, s in zip(rows, s2)], open(D + f'/eval_{name.replace(" ", "_")}.json', 'w'))
json.dump(res, open(D + '/eval_summary.json', 'w'))
