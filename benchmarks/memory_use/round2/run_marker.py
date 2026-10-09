"""Marker test on qwen: variants N (no memory), A, B, C, D x 101 cases x 3 reps. Resumable JSONL."""
import concurrent.futures as cf, hashlib, json, os, random, sys
from common import *
OUT = BENCH + '/marker_runs.jsonl'
REPS = 3
PRE = ["Memories from your earlier sessions with this user that may apply here (newest first).",
       "Follow each one that applies to this request. If you decide one does not apply or you must go against it, say which and why in your answer."]
CITE = "When an item changes what you do, cite its tag once, e.g. (m:3e99)."
CONF = "If two memories conflict, follow the more specific one and say so."
cases = json.load(open(CASES))
def tag(mid): return hashlib.md5(mid.encode()).hexdigest()[:4]

def block(c, rep, variant):
    rnd = random.Random(f"{c['mem_id']}|{rep}")
    pool = [x for x in cases if x['mem_id'] != c['mem_id']]
    ds = rnd.sample(pool, 2)
    items = [(c['mem_id'], c['mem_text'][:900])] + [(d['mem_id'], d['mem_text'][:900]) for d in ds]
    rnd.shuffle(items)
    pre = list(PRE)
    if variant == 'D':
        items = [i for i in items if i[0] == c['mem_id']] + [i for i in items if i[0] != c['mem_id']]
        pre[0] = "Memories from your earlier sessions with this user that may apply here (most relevant first)."
    lines = []
    for n, (mid, t) in enumerate(items):
        lab = ' [most relevant]' if variant == 'D' and n == 0 else ''
        lines.append(f"- (m:{tag(mid)}){lab} {t}")
    if variant in ('B', 'C', 'D'): pre.append(CITE)
    if variant == 'C': pre.append(CONF)
    return '\n'.join(pre + lines), {tag(m) for m, _ in items}

def one(job):
    c, rep, v = job
    if v == 'N':
        p = c['task'] + ASK
    else:
        b, _ = block(c, rep, v)
        p = b + '\n\nRequest: ' + c['task'] + ASK
    return {'mem_id': c['mem_id'], 'rep': rep, 'variant': v, 'tag': tag(c['mem_id']), 'answer': chat(p, seed=rep)}

if __name__ == '__main__':
    done = set()
    if os.path.exists(OUT):
        for l in open(OUT):
            r = json.loads(l); done.add((r['mem_id'], r['rep'], r['variant']))
    jobs = [(c, r, v) for r in range(REPS) for v in 'NABCD' for c in cases if (c['mem_id'], r, v) not in done]
    print(len(jobs), 'jobs', flush=True)
    with cf.ThreadPoolExecutor(2) as ex, open(OUT, 'a') as f:
        for i, r in enumerate(ex.map(one, jobs)):
            f.write(json.dumps(r) + '\n'); f.flush()
            if i % 50 == 0: print(i, flush=True)
