"""Confirmatory Haiku sample for the marker test: 30 gradeable cases x variants A,B x 1 rep.
Sequential `claude -p`, hard cap of 60 calls total (retries count). Resumable JSONL."""
import json, os, random, subprocess
from common import *
import run_marker as rm
HMODEL = 'claude-haiku-5-5'
CLEAN = '/tmp/claude-1000/clean'
OUT = BENCH + '/marker_runs_haiku.jsonl'
CAP, NCASES, REP = 60, 30, 0
calls = 0

def claude(prompt):
    global calls
    for attempt in range(2):  # one retry on transient error
        if calls >= CAP: return None
        calls += 1
        try:
            p = subprocess.run(['claude', '-p', '--model', HMODEL, '--tools', '', '--strict-mcp-config',
                                '--setting-sources', '', '--system-prompt', SYS],
                               input=prompt, capture_output=True, text=True, timeout=180, cwd=CLEAN)
            if p.returncode == 0 and p.stdout.strip(): return p.stdout.strip()
        except subprocess.TimeoutExpired: pass
    return ''

if __name__ == '__main__':
    os.makedirs(CLEAN, exist_ok=True)
    chk = json.load(open(BENCH + '/checks_final.json'))
    ids = sorted(c['mem_id'] for c in rm.cases if chk.get(c['mem_id'], {}).get('gradeable'))
    ids = set(random.Random(7).sample(ids, NCASES))
    cs = [c for c in rm.cases if c['mem_id'] in ids]
    done = set()
    if os.path.exists(OUT):
        for l in open(OUT):
            r = json.loads(l); done.add((r['mem_id'], r['variant']))
    with open(OUT, 'a') as f:
        for c in cs:
            for v in 'AB':
                if (c['mem_id'], v) in done: continue
                b, _ = rm.block(c, REP, v)
                a = claude(b + '\n\nRequest: ' + c['task'] + ASK)
                if a is None: raise SystemExit(f'call cap {CAP} reached')
                f.write(json.dumps({'mem_id': c['mem_id'], 'rep': REP, 'variant': v, 'tag': rm.tag(c['mem_id']), 'answer': a}) + '\n'); f.flush()
    print('calls used', calls)
