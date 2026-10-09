"""Long-context adherence: memory shown at session start, ~20k tokens of work
in between, then the task. Arms:
  start     the directive memory block only at the start (as CLAUDE.md / a briefing is)
  action    the same, plus the action-time check (the PreToolUse cue firing)
Cases: those where the no-memory agent did not follow the memory (needs it).
usage: long_run.py results.json out.json [n]"""
import concurrent.futures as cf
import glob
import json
import random
import sys

sys.path.insert(0, '.')
from adherence import ASK, claude, fmt_directive, grade


def filler(seed, target_chars=80000):
    files = sorted(glob.glob('/mnt/bulk/grimoire-situated/docs/*.md') + glob.glob('/mnt/bulk/grimoire-situated/go/internal/index/*.go'))
    rnd = random.Random(seed); rnd.shuffle(files)
    out, size = [], 0
    for f in files:
        text = open(f).read()[:12000]
        out.append(f"User: next, look at {f.split('/grimoire-situated/')[1]} and tell me what it does.\n"
                   f"Assistant: Reading it.\n[tool output: Read {f.split('/grimoire-situated/')[1]}]\n{text}\n"
                   f"Assistant: Done; noted the structure.\n")
        size += len(out[-1])
        if size >= target_chars: break
    return '\n'.join(out)

def run_case(c):
    rnd = random.Random(c['mem_id'])
    gold = c['mem_text'][:900]
    mems = c['distractors'] + [gold]; rnd.shuffle(mems)
    head = fmt_directive(mems) + '\n\n--- session so far ---\n' + filler(c['mem_id']) + '\n--- end of session so far ---\n\n'
    out = {'mem_id': c['mem_id']}
    out['start'] = claude(head + 'User: ' + c['task'] + ASK)
    out['action'] = claude(head + 'User: ' + c['task'] +
        f"\n\nYou proposed: {out['start']}\nBefore it runs, this memory matched the action:\n- {gold}\n"
        "If your action goes against it, give the corrected action; otherwise repeat your action. One line only.")
    for arm in ('start', 'action'):
        out['grade_' + arm] = grade(c, out[arm])
    return out

if __name__ == '__main__':
    prev = {r['mem_id']: r for r in json.load(open(sys.argv[1]))}
    n = int(sys.argv[3]) if len(sys.argv) > 3 else 60
    cases = [c for c in json.load(open('cases.json')) if prev[c['mem_id']]['grade_none'] != 'FOLLOWS'][:n]
    with cf.ThreadPoolExecutor(6) as ex:
        res = list(ex.map(run_case, cases))
    json.dump(res, open(sys.argv[2], 'w'), indent=1)
    for arm in ('start', 'action'):
        g = [r['grade_' + arm] for r in res]
        print(f"{arm:8s} follows {g.count('FOLLOWS')}/{len(g)}  violates {g.count('VIOLATES')}  unclear {g.count('UNCLEAR')}")
