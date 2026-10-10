"""Ground truth and scoring.

usage: report.py --out DIR     (writes DIR/REPORT.md and DIR/report.json)

Ground truth (all from the arms; no model judges anything):
  task level   influence = P(key action | memory) - P(key action | no memory), where the key action is
               the memory-dictated one (`compliant`) for treatment tasks and any touch of the memory's
               surface (`used`) for controls. benefit = mean score with minus without (score = success,
               less 0.05 per failed call, halved by a violation).
  run x memory a with-run's relevant memory is INFLUENCED when the run took the key action and the
               no-memory arm rarely does (P_without <= 0.5); distractors are not influenced by construction.
  action level (shadow replay) an action is INFLUENCED when it survives in <= 1/3 of replays with the
               relevant memory removed from the same conversation prefix; BENEFICIAL when it is influenced
               and the run scored above that task's no-memory mean.
"""
import argparse
import glob
import json
import os
import sys
from collections import defaultdict

sys.path.insert(0, os.path.dirname(os.path.abspath(__file__)))
import signals as S
import tasks as T


def mean(x):
    x = list(x)
    return sum(x) / len(x) if x else float('nan')


def prf(tp, fp, fn):
    p = tp / (tp + fp) if tp + fp else float('nan')
    r = tp / (tp + fn) if tp + fn else float('nan')
    f = 2 * p * r / (p + r) if p == p and r == r and p + r else float('nan')
    return p, r, f


def fmt(x, pct=True):
    if x != x:
        return 'n/a'
    return f'{100 * x:.0f}%' if pct else f'{x:.2f}'


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument('--out', required=True)
    a = ap.parse_args()
    tasks = {t['id']: t for t in T.build()}
    runs = {}
    for p in glob.glob(os.path.join(a.out, 'runs', '*.json')):
        r = json.load(open(p))
        runs[(r['task_id'], r['arm'], r['rep'])] = r
    fps = S.build_fp(list(tasks.values()))

    def key(r):
        g = r['grade']
        return g['compliant'] if r['applies'] else g['used']

    # ---------------- task level
    rows = []
    for tid, t in tasks.items():
        w = [r for (i, arm, _), r in runs.items() if i == tid and arm == 'with']
        o = [r for (i, arm, _), r in runs.items() if i == tid and arm == 'without']
        if not w or not o:
            continue
        pw, po = mean(key(r) for r in w), mean(key(r) for r in o)
        sw, so = mean(r['score'] for r in w), mean(r['score'] for r in o)
        div = []
        for r in w:
            q = runs.get((tid, 'without', r['rep']))
            if q:
                A = {S_ for s in r['steps'] for c in s['calls'] for S_ in [__import__('agent').sig(c['name'], c['args'])]}
                B = {S_ for s in q['steps'] for c in s['calls'] for S_ in [__import__('agent').sig(c['name'], c['args'])]}
                div.append(1 - len(A & B) / len(A | B) if A | B else 0)
        rows.append(dict(task=tid, family=t['family'].split('-')[0], applies=t['applies'], n=len(w), p_with=pw, p_without=po,
                         influence=pw - po, succ_with=mean(r['grade']['success'] for r in w), succ_without=mean(r['grade']['success'] for r in o),
                         score_with=sw, score_without=so, benefit=sw - so, divergence=mean(div),
                         viol_with=mean(r['grade']['violation'] for r in w), viol_without=mean(r['grade']['violation'] for r in o)))
    out = dict(tasks=rows)
    md = ['# Utilization-trace validation: ground truth and signal accuracy', '',
          f'Agent: qwen3.6:35b-a3b (Ollama, think on, num_ctx 16384). {len(runs)} runs over {len(rows)} tasks. '
          'Arms: `with` (relevant memory + 2 distractors in the system prompt) and `without` (the 2 distractors only); same seeds per rep.', '']

    def agg(sel, label):
        if not sel:
            return
        md.append(f"| {label} | {len(sel)} | {fmt(mean(r['p_with'] for r in sel))} | {fmt(mean(r['p_without'] for r in sel))} | "
                  f"{fmt(mean(r['influence'] for r in sel))} | {fmt(mean(r['influence'] > 0 for r in sel))} | "
                  f"{fmt(mean(r['succ_with'] for r in sel))} / {fmt(mean(r['succ_without'] for r in sel))} | "
                  f"{mean(r['benefit'] for r in sel):+.2f} | {fmt(mean(r['benefit'] > 0 for r in sel))} | {fmt(mean(r['benefit'] < 0 for r in sel))} |")
    md += ['## Task level', '', '| group | tasks | key action with | without | influence rate | tasks influenced | success with / without | mean benefit (score) | tasks benefited | tasks harmed |',
           '|---|---|---|---|---|---|---|---|---|---|']
    tr = [r for r in rows if r['applies']]
    ct = [r for r in rows if not r['applies']]
    agg(tr, 'treatment (memory applies)')
    for fam in sorted({r['family'] for r in tr}):
        agg([r for r in tr if r['family'] == fam], f'  {fam}')
    agg(ct, 'control (memory does not apply)')
    md += ['', 'influence rate = P(key action | memory) - P(key action | no memory); controls use any touch of the memory surface as the key action, so a nonzero control value is the memory leaking into an unrelated task.', '',
           '### Per task', '', '| task | key with | key without | influence | success w/wo | benefit | trajectory divergence |', '|---|---|---|---|---|---|---|']
    for r in rows:
        md.append(f"| {r['task']} | {fmt(r['p_with'])} | {fmt(r['p_without'])} | {fmt(r['influence'])} | {fmt(r['succ_with'])}/{fmt(r['succ_without'])} | {r['benefit']:+.2f} | {fmt(r['divergence'])} |")
    out['task_summary'] = dict(treatment=dict(n=len(tr), influence=mean(r['influence'] for r in tr), influenced_share=mean(r['influence'] > 0 for r in tr),
                                              benefit=mean(r['benefit'] for r in tr), benefited_share=mean(r['benefit'] > 0 for r in tr)),
                               control=dict(n=len(ct), influence=mean(r['influence'] for r in ct), benefit=mean(r['benefit'] for r in ct)))

    # ---------------- run x memory: signals vs ground truth
    pw = {r['task']: r['p_without'] for r in rows}
    inst = []
    for (tid, arm, rep), r in sorted(runs.items()):
        if arm != 'with':
            continue
        sig = S.run_signals(r, fps)
        for m in r['memories']:
            if m['relevant']:
                gt = bool(key(r)) and pw[tid] <= 0.5
            else:
                gt = False
            inst.append(dict(task=tid, rep=rep, mem=m['id'], relevant=m['relevant'], applies=r['applies'], gt=gt, **sig[m['id']]))
    out['instances'] = inst

    def score_signal(sel, name, fn):
        tp = sum(1 for i in sel if fn(i) and i['gt'])
        fp = sum(1 for i in sel if fn(i) and not i['gt'])
        fn_ = sum(1 for i in sel if not fn(i) and i['gt'])
        p, r_, f = prf(tp, fp, fn_)
        return name, tp, fp, fn_, p, r_, f

    md += ['', '## Signals vs ground truth (run x memory)', '',
           f"{len(inst)} instances ({sum(1 for i in inst if i['relevant'])} relevant, {sum(1 for i in inst if not i['relevant'])} distractor); "
           f"ground-truth influenced: {sum(1 for i in inst if i['gt'])}. Fingerprintable: relevant memories "
           f"{fmt(mean(i['fingerprintable'] for i in inst if i['relevant']))}, distractors {fmt(mean(i['fingerprintable'] for i in inst if not i['relevant']))}.", '',
           '| signal | scope | TP | FP | FN | precision | recall | F1 |', '|---|---|---|---|---|---|---|---|']
    sigs = [('tag cited', lambda i: i['tag']), ('fingerprint', lambda i: i['fp']),
            ('tag or fingerprint', lambda i: i['tag'] or i['fp']), ('tag and fingerprint', lambda i: i['tag'] and i['fp'])]
    out['signals'] = {}
    for scope, sel in (('all memories', inst), ('relevant only', [i for i in inst if i['relevant']]),
                       ('treatment relevant', [i for i in inst if i['relevant'] and i['applies']])):
        for name, fn in sigs:
            n, tp, fp, fn_, p, r_, f = score_signal(sel, name, fn)
            md.append(f'| {name} | {scope} | {tp} | {fp} | {fn_} | {fmt(p)} | {fmt(r_)} | {fmt(f)} |')
            out['signals'][f'{name}|{scope}'] = dict(tp=tp, fp=fp, fn=fn_, precision=p, recall=r_, f1=f)
    # error anatomy
    rel = [i for i in inst if i['relevant']]
    md += ['', '### Where the signals miss or fire wrongly', '']
    infl = [i for i in rel if i['gt']]
    non = [i for i in rel if not i['gt']]
    dis = [i for i in inst if not i['relevant']]
    md.append(f"* Influenced relevant memories ({len(infl)}): tag cited {fmt(mean(i['tag'] for i in infl))}, fingerprint {fmt(mean(i['fp'] for i in infl))}, "
              f"either {fmt(mean(i['tag'] or i['fp'] for i in infl))}, neither {fmt(mean(not (i['tag'] or i['fp']) for i in infl))}; of the influenced that are unfingerprintable: "
              f"{sum(1 for i in infl if not i['fingerprintable'])}.")
    md.append(f"* Not-influenced relevant memories ({len(non)}: controls and runs that did the right thing anyway): tag {fmt(mean(i['tag'] for i in non))}, fingerprint {fmt(mean(i['fp'] for i in non))}.")
    md.append(f"* Distractors ({len(dis)}): tag {fmt(mean(i['tag'] for i in dis))}, fingerprint {fmt(mean(i['fp'] for i in dis))}.")
    # without-arm leakage of the relevant memory's fingerprints
    leak, nleak = [], 0
    for (tid, arm, rep), r in runs.items():
        if arm != 'without':
            continue
        toks = fps.get((tid, 'rel'), [])
        if not toks:
            continue
        r2 = dict(r)
        r2['memories'] = [dict(id='rel', tag=tasks[tid]['relevant_tag'], relevant=True)]
        leak.append(S.run_signals(r2, fps)['rel']['fp'])
    md.append(f"* Fingerprint of the relevant memory in runs that never saw it ({len(leak)} runs): {fmt(mean(leak))}. This is the base rate of an agent landing on the same tokens by itself.")
    out['anatomy'] = dict(influenced=len(infl), influenced_tag=mean(i['tag'] for i in infl), influenced_fp=mean(i['fp'] for i in infl),
                          distractor_fp=mean(i['fp'] for i in dis), distractor_tag=mean(i['tag'] for i in dis), without_arm_fp=mean(leak))

    # ---------------- action level (shadow replay)
    rp = []
    for p in sorted(glob.glob(os.path.join(a.out, 'replay', '*.json'))):
        d = json.load(open(p))
        r = runs.get((d['task_id'], 'with', int(d['run_id'].split('.')[-1])))
        if r:
            rp.append((d, r))
    if rp:
        toks_for = lambda tid: fps.get((tid, 'rel'), [])
        pts = []
        for d, r in rp:
            tid = d['task_id']
            base = mean(q['score'] for (i, arm, _), q in runs.items() if i == tid and arm == 'without')
            for pt in d['points']:
                step = r['steps'][pt['step']]
                tgt = S.action_target(pt['name'], pt['args'])
                infl_a = (1 - pt['survival']) >= 0.66
                tg = S.matched(toks_for(tid), tgt) if tgt else 0
                pts.append(dict(task=tid, applies=r['applies'], influenced=infl_a, survival=pt['survival'], beneficial=infl_a and r['score'] > base,
                                fp=bool(tg), tag=tasks[tid]['relevant_tag'] in S.TAG.findall(step['content']) or any(
                                    tasks[tid]['relevant_tag'] == x for x in S.TAG.findall(step['content'])),
                                err=pt['err'], name=pt['name']))
        out['actions'] = pts
        md += ['', '## Action level (shadow replay: re-run each decision point without the relevant memory, 3 samples)', '',
               f"{len(rp)} with-runs replayed, {len(pts)} actions ({sum(1 for p in pts if p['applies'])} in treatment tasks). "
               f"Influenced (survival <= 1/3): {sum(p['influenced'] for p in pts)} ({fmt(mean(p['influenced'] for p in pts))}); "
               f"in treatment tasks {fmt(mean(p['influenced'] for p in pts if p['applies']))}, controls {fmt(mean(p['influenced'] for p in pts if not p['applies']))}. "
               f"Beneficial (influenced and run beat the no-memory mean): {sum(p['beneficial'] for p in pts)}; "
               f"{fmt(mean(p['beneficial'] for p in pts if p['influenced']))} of influenced actions.", '',
               '| signal on the action | TP | FP | FN | precision | recall | F1 |', '|---|---|---|---|---|---|---|']
        out['action_signals'] = {}
        for name, fn in (('fingerprint in this action', lambda p: p['fp']), ('tag cited in this message', lambda p: p['tag']),
                         ('either', lambda p: p['fp'] or p['tag'])):
            tp = sum(1 for p in pts if fn(p) and p['influenced'])
            fp = sum(1 for p in pts if fn(p) and not p['influenced'])
            fn_ = sum(1 for p in pts if not fn(p) and p['influenced'])
            pr, rc, f1 = prf(tp, fp, fn_)
            md.append(f'| {name} | {tp} | {fp} | {fn_} | {fmt(pr)} | {fmt(rc)} | {fmt(f1)} |')
            out['action_signals'][name] = dict(tp=tp, fp=fp, fn=fn_, precision=pr, recall=rc, f1=f1)
        # agreement between replay and arm-level ground truth, per replayed run
        agree = tot = 0
        for d, r in rp:
            run_infl = bool(key(r)) and pw[d['task_id']] <= 0.5
            rep_infl = any((1 - p['survival']) >= 0.66 for p in d['points'])
            agree += run_infl == rep_infl
            tot += 1
        md.append('')
        md.append(f"Replay vs arm-level ground truth, per run (does the run contain an influenced action / was the memory's key action taken against a low no-memory base rate): agree on {agree}/{tot}.")
        out['replay_agreement'] = dict(agree=agree, total=tot)
    open(os.path.join(a.out, 'REPORT.md'), 'w').write('\n'.join(md) + '\n')
    json.dump(out, open(os.path.join(a.out, 'report.json'), 'w'), indent=1, default=str)
    print('\n'.join(md))


if __name__ == '__main__':
    main()
