"""Run every task with and without the relevant memory and write runs + hook-shaped events.

usage: run.py --out DIR [--reps 2] [--workers 2] [--tasks id,id] [--limit N]
Resumable: a finished run file is skipped. Events for all runs are rebuilt into
DIR/events.jsonl by build_events() (also called at the end).
"""
import argparse
import json
import os
import sys
import time
from concurrent.futures import ThreadPoolExecutor

sys.path.insert(0, os.path.dirname(os.path.abspath(__file__)))
import agent
import tasks as T

ARMS = ('with', 'without')


def seed_for(task_id, arm, rep):
    import hashlib
    return (int(hashlib.sha1(f'{task_id}/{rep}'.encode()).hexdigest()[:6], 16) % 100000) * 100  # same seed for both arms: paired by construction


def score(grade):
    if not grade['success']:
        return 0.0
    s = 1.0 - 0.05 * min(grade['wasted'], 6)
    return s * (0.5 if grade['violation'] else 1.0)


def run_one(task, arm, rep, out):
    rid = f"{task['id']}.{arm}.{rep}"
    path = os.path.join(out, 'runs', rid + '.json')
    if os.path.exists(path):
        return rid
    mems = [m for m in task['memories'] if arm == 'with' or not m['relevant']]
    env = T.make_env(task)
    t0 = time.time()
    r = agent.run_agent(task, env, mems, seed_for(task['id'], arm, rep))
    g = env.h.grade(env)
    rec = dict(run_id=rid, task_id=task['id'], family=task['family'], applies=task['applies'], arm=arm, rep=rep,
               prompt=task['prompt'], memories=[dict(id=m['id'], tag=m['tag'], text=m['text'], relevant=m['relevant']) for m in mems],
               steps=r['steps'], final=r['final'], grade=g, score=score(g), seconds=round(time.time() - t0, 1))
    os.makedirs(os.path.dirname(path), exist_ok=True)
    json.dump(rec, open(path + '.tmp', 'w'))
    os.replace(path + '.tmp', path)
    print(f"{rid:28} success={g['success']} comp={g['compliant']} used={g['used']} viol={g['violation']} wasted={g['wasted']} {rec['seconds']}s", flush=True)
    return rid


def build_events(out):
    """Hook-shaped events (what the context/outcome hooks receive) for every run."""
    rows = []
    for fn in sorted(os.listdir(os.path.join(out, 'runs'))):
        if not fn.endswith('.json'):
            continue
        r = json.load(open(os.path.join(out, 'runs', fn)))
        sid = r['run_id']
        seq = 0

        def emit(ev):
            nonlocal seq
            seq += 1
            rows.append(dict(run=r['run_id'], task=r['task_id'], arm=r['arm'], seq=seq, event=dict(session_id=sid, **ev)))
        emit(dict(hook_event_name='UserPromptSubmit', prompt=r['prompt']))
        texts = []
        n = 0
        for st in r['steps']:
            if st['content'].strip():
                texts.append(st['content'])
            for c in st['calls']:
                n += 1
                tid = f"toolu_{n:03d}"
                tin = _claude_input(c['name'], c['args'])
                emit(dict(hook_event_name='PreToolUse', tool_name=agent.CLAUDE_NAME[c['name']] if c['name'] in agent.CLAUDE_NAME else c['name'], tool_input=tin, tool_use_id=tid))
                emit(dict(hook_event_name='PostToolUse', tool_name=agent.CLAUDE_NAME.get(c['name'], c['name']), tool_input=tin, tool_use_id=tid,
                          tool_response=dict(output=c['out'][:2000], is_error=c['err'])))
        emit(dict(hook_event_name='Stop', stop_hook_active=False, last_assistant_message=r['final'], assistant_text='\n'.join(texts)))
    with open(os.path.join(out, 'events.jsonl'), 'w') as f:
        for row in rows:
            f.write(json.dumps(row) + '\n')
    return len(rows)


def _claude_input(name, a):
    if name == 'bash':
        return dict(command=a.get('command', ''))
    if name == 'edit_file':
        return dict(file_path=a.get('path', ''), old_string=a.get('old_string', ''), new_string=a.get('new_string', ''))
    if name == 'write_file':
        return dict(file_path=a.get('path', ''), content=a.get('content', ''))
    if name == 'read_file':
        return dict(file_path=a.get('path', ''))
    return dict(path=a.get('path', ''))


if __name__ == '__main__':
    ap = argparse.ArgumentParser()
    ap.add_argument('--out', required=True)
    ap.add_argument('--reps', type=int, default=2)
    ap.add_argument('--workers', type=int, default=2)
    ap.add_argument('--tasks', default='')
    ap.add_argument('--limit', type=int, default=0)
    a = ap.parse_args()
    ts = T.build()
    if a.tasks:
        keep = set(a.tasks.split(','))
        ts = [t for t in ts if t['id'] in keep]
    if a.limit:
        ts = ts[:a.limit]
    jobs = [(t, arm, rep) for rep in range(a.reps) for t in ts for arm in ARMS]
    print(f'{len(ts)} tasks, {len(jobs)} runs, workers={a.workers}', flush=True)
    with ThreadPoolExecutor(a.workers) as ex:
        futs = [ex.submit(run_one, t, arm, rep, a.out) for t, arm, rep in jobs]
        for f in futs:
            try:
                f.result()
            except Exception as e:
                print('RUN FAILED', e, flush=True)
    print('events:', build_events(a.out))
