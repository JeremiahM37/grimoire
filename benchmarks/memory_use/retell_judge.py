"""Can a typed-decision model tell a re-tell from a prompt that merely shares a topic?
Positives: verified real re-tells (user text + the memory it restated).
Negatives: the broad detector's candidates (prompt + the memory it matched)."""
import json
import urllib.request

import yaml

cfg = yaml.safe_load(open('/home/admin/.ai-secrets/jev.yaml'))['jev']
mem = {i['id']: i['text'] for i in json.load(open('../data/memory_items.json'))}
Q = {"retell": {"type": "noul",
     "instructions": "Is the user's message restating, reminding the agent of, or correcting it with something this stored memory already says (so the agent should have known it)? A message that only shares the memory's topic, asks about it, or gives a new instruction is not.",
     "criteria": {"true": "the message restates or re-asserts what the memory says", "false": "the message is new information, a question, or only on the same topic"}}}
def ask(memory, message):
    state = f"Stored memory:\n{memory[:1200]}\n\nUser's message to the agent:\n{message[:1200]}"
    body = json.dumps({"state": state, "model": "jev-latest", "questions": Q}).encode()
    req = urllib.request.Request(cfg['base_url'].rstrip('/') + '/v1/systemone', body,
                                 {"Content-Type": "application/json", "Authorization": "Bearer " + cfg['api_key']})
    return json.load(urllib.request.urlopen(req, timeout=30))['answers']['retell']['noul']
pos = []
for line in open('../data/eval_cases.jsonl'):
    c = json.loads(line)
    if c['failure_class'] == 'F1' and c['label_quality'] == 'verified':
        g = c['gold_memory_ids'][0]
        if g in mem: pos.append((mem[g], c['failure_signal']))
vault_mem = {}
neg = []
for line in json.load(open('../bench/replay_learned_broad.json')):
    t = line['target']
    if t.startswith('note:'):
        try: text = open('/mnt/bulk/memory-use-research/vault/' + t[5:]).read()
        except OSError: continue
    else:
        text = mem.get('gm:' + t[5:], '')
    if text: neg.append((text, line['correction']))
out = {'pos': [ask(m, u) for m, u in pos], 'neg': [ask(m, u) for m, u in neg]}
json.dump(out, open('jev_retell.json', 'w'))
P, N = out['pos'], out['neg']
auc = sum((p > n) + 0.5 * (p == n) for p in P for n in N) / (len(P) * len(N))
print(f"positives {len(P)} negatives {len(N)} AUC {auc:.3f}")
for th in (0.5, 0.6, 0.7, 0.8, 0.9):
    print(f"  threshold {th}: catches {sum(p >= th for p in P)}/{len(P)}, false {sum(n >= th for n in N)}/{len(N)}")
print('pos scores', [round(p, 2) for p in P])
