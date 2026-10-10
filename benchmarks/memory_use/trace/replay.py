"""Shadow replay (AttriGuard-style) on sampled with-memory runs.

For each decision point of a with-run (an assistant message that called tools), rebuild the
exact conversation up to that point, remove the relevant memory from the system prompt, and
sample the next message N times. An action's survival is the fraction of samples that contain
the same normalised action signature. influenced = survival <= 1/3 (N=3).

usage: replay.py --out DIR [--n 3] [--workers 3] [--per-family 2] [--controls 4]
"""
import argparse
import json
import os
import sys
from concurrent.futures import ThreadPoolExecutor

sys.path.insert(0, os.path.dirname(os.path.abspath(__file__)))
import agent
import run as R
import tasks as T


def pick(tasks, per_family, controls):
    seen, chosen = {}, []
    for t in tasks:
        if t['applies']:
            k = t['family'].split('-')[0]
            if seen.get(k, 0) < per_family:
                seen[k] = seen.get(k, 0) + 1
                chosen.append(t['id'])
    chosen += [t['id'] for t in tasks if not t['applies']][:controls]
    return chosen


def replay_run(task, run, n, out):
    path = os.path.join(out, 'replay', run['run_id'] + '.json')
    if os.path.exists(path):
        return
    mems = [m for m in run['memories'] if not m['relevant']]
    messages = [{'role': 'system', 'content': agent.system_prompt(mems)}, {'role': 'user', 'content': run['prompt']}]
    points = []
    for si, st in enumerate(run['steps']):
        if not st['calls']:
            break
        if si >= 8:
            break
        samples = []
        for k in range(n):
            m = agent.chat(messages, 7000 + si * 17 + k)
            samples.append([agent.sig(c['name'], c['args']) for c in agent.clean_calls(m)])
        for ci, c in enumerate(st['calls']):
            s = agent.sig(c['name'], c['args'])
            surv = sum(1 for smp in samples if s in smp) / n
            points.append(dict(step=si, call=ci, name=c['name'], args=c['args'], sig=s, survival=surv, samples=samples, err=c['err']))
        am = {'role': 'assistant', 'content': st['content'], 'tool_calls': [{'function': {'name': c['name'], 'arguments': c['args']}} for c in st['calls']]}
        messages.append(am)
        for c in st['calls']:
            messages.append({'role': 'tool', 'tool_name': c['name'], 'content': c['out'][:3000] or '(no output)'})
    os.makedirs(os.path.dirname(path), exist_ok=True)
    json.dump(dict(run_id=run['run_id'], task_id=run['task_id'], n=n, points=points), open(path, 'w'))
    print('replayed', run['run_id'], len(points), 'actions', flush=True)


if __name__ == '__main__':
    ap = argparse.ArgumentParser()
    ap.add_argument('--out', required=True)
    ap.add_argument('--n', type=int, default=3)
    ap.add_argument('--workers', type=int, default=3)
    ap.add_argument('--per-family', type=int, default=2)
    ap.add_argument('--controls', type=int, default=4)
    a = ap.parse_args()
    tasks = {t['id']: t for t in T.build()}
    ids = pick(list(tasks.values()), a.per_family, a.controls)
    jobs = []
    for i in ids:
        p = os.path.join(a.out, 'runs', f'{i}.with.0.json')
        if os.path.exists(p):
            jobs.append((tasks[i], json.load(open(p))))
    print(len(jobs), 'runs to replay', flush=True)
    with ThreadPoolExecutor(a.workers) as ex:
        for f in [ex.submit(replay_run, t, r, a.n, a.out) for t, r in jobs]:
            f.result()
