"""Build and Jev-label (request, memory) training pairs from ROUND-1 cases only.
usage: gatemodel_labels.py [budget]  -> $GATEMODEL_DIR/labels.jsonl (cached; resumable)
Pairs per case: the gold memory, the HARD (default 5) top-scoring non-gold hybrid candidates (hard negatives)
and RAND (default 2) random non-gold candidates. Round-1 cases sharing any word 5-gram with a round-2 prompt
are dropped so round 2 stays held out. Jev key comes from ~/.ai-secrets/jev.yaml (never printed)."""
import sys, os, json, random, hashlib, concurrent.futures as cf
sys.path.insert(0, os.path.dirname(__file__))
from gate_common import load
from gate_wire import call, clip, JEV
OUT = os.environ.get('GATEMODEL_DIR', '/mnt/bulk/memory-use-research/round2/gatemodel')
STRICT = {"applies": {"type": "noul", "instructions": "Is this stored memory directly about the task the user is asking for right now, so that the agent would act differently without it? Sharing a topic or a project name is not enough.",
          "criteria": {"true": "the memory states a rule, fact or preference that governs this exact request", "false": "the memory is only on the same topic, or the request does not touch what it says"}}}
budget = int(sys.argv[1]) if len(sys.argv) > 1 else 6000
HARD, RAND = int(os.environ.get('HARD', 5)), int(os.environ.get('RAND', 2))
def grams(s, n=5):
    w = s.lower().split(); return {' '.join(w[i:i+n]) for i in range(len(w)-n+1)}
def state(prompt, mem): return f"Memory: {clip(mem,280)}\nRequest: {clip(prompt,240)}"
def key(prompt, mem): return hashlib.sha1((clip(prompt,240)+'\x00'+clip(mem,280)).encode()).hexdigest()
if __name__ == '__main__':
    d = load(); random.seed(11)
    r2 = [x for x in d if str(x['round']) == '2']; r1 = [x for x in d if str(x['round']) == '1']
    g2 = set().union(*[grams(x['prompt']) for x in r2]) if r2 else set()
    dropped = 0; pairs = []; seen = set()
    for x in r1:
        if grams(x['prompt']) & g2: dropped += 1; continue
        c = sorted(x['cands'], key=lambda c: -c['score'])
        non = [a for a in c if not a['isgold']]
        pick = ([x['gold']] if x['gold'] else []) + non[:HARD] + random.sample(non[HARD:], min(RAND, len(non[HARD:])))
        for a in pick:
            k = key(x['prompt'], a['text'])
            if k in seen: continue
            seen.add(k)
            pairs.append(dict(k=k, prompt=x['prompt'], mem=a['text'], mem_path=a['path'], score=a['score'], kind=x['kind'], isgold=bool(a['isgold'])))
    random.shuffle(pairs)
    fn = OUT + '/labels.jsonl'
    cached = {json.loads(l)['k'] for l in open(fn)} if os.path.exists(fn) else set()
    pairs.sort(key=lambda p: p['k'] not in cached); pairs = pairs[:budget]  # keep what is already labelled first
    print('round1 cases', len(r1), 'dropped for overlap', dropped, 'pairs', len(pairs))
    os.makedirs(OUT, exist_ok=True); fn = OUT + '/labels.jsonl'
    done = {}
    if os.path.exists(fn):
        for l in open(fn): j = json.loads(l); done[j['k']] = j
    todo = [p for p in pairs if p['k'] not in done]
    print('cached', len(done), 'to label', len(todo))
    def one(p):
        r, ms = call(JEV, state(p['prompt'], p['mem']), STRICT, 'jev-latest')
        try: p['p'] = r['answers']['applies']['noul']
        except Exception: p['p'] = None
        return p
    with cf.ThreadPoolExecutor(8) as ex, open(fn, 'a') as f:
        for i, p in enumerate(ex.map(one, todo)):
            if p['p'] is not None: f.write(json.dumps(p) + '\n'); f.flush()
            if i % 500 == 0: print(i, flush=True)
