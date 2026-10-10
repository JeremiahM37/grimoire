"""Fine-tune the MS MARCO MiniLM cross-encoder on Jev strict-gate labels.
usage: gatemodel_train.py   (env GATEMODEL_DIR; run with /mnt/bulk/laya/venv/bin/python)
Target: soft label sigmoid((logit(p_jev) - logit(0.16)) / T) so logit 0 == Jev's calibrated threshold.
Writes $GATEMODEL_DIR/model/ (config, tokenizer, model.safetensors) and val.json."""
import os, sys, json, math, random, time, torch
from transformers import AutoTokenizer, AutoModelForSequenceClassification
D = os.environ.get('GATEMODEL_DIR', '/mnt/bulk/memory-use-research/round2/gatemodel')
BASE = os.environ.get('GATEMODEL_BASE', '/mnt/bulk/grimoire-parity/models/cross-encoder--ms-marco-MiniLM-L-6-v2')
EPOCHS = int(os.environ.get('EPOCHS', 4)); LR = float(os.environ.get('LR', 4e-5)); BS = 32; MAXLEN = 192; T = 1.0
torch.manual_seed(3); random.seed(3); torch.set_num_threads(24)
def clip(s, n): s = ' '.join(s.split()); return s if len(s) <= n else s[:n]
def lg(p): p = min(max(p, 1e-4), 1 - 1e-4); return math.log(p / (1 - p))
rows = [json.loads(l) for l in open(D + '/labels.jsonl')]
prompts = sorted({r['prompt'] for r in rows}); random.shuffle(prompts)
val_p = set(prompts[:len(prompts) // 10])
tr = [r for r in rows if r['prompt'] not in val_p]; va = [r for r in rows if r['prompt'] in val_p]
print('train', len(tr), 'val', len(va), flush=True)
tok = AutoTokenizer.from_pretrained(BASE); model = AutoModelForSequenceClassification.from_pretrained(BASE, num_labels=1)
def enc(b): return tok([clip(r['prompt'], 240) for r in b], [clip(r['mem'], 280) for r in b], truncation='longest_first', max_length=MAXLEN, padding=True, return_tensors='pt')
def target(r): return torch.sigmoid(torch.tensor((lg(r['p']) - lg(0.16)) / T))
opt = torch.optim.AdamW(model.parameters(), lr=LR, weight_decay=0.01)
steps = EPOCHS * math.ceil(len(tr) / BS); sched = torch.optim.lr_scheduler.LambdaLR(opt, lambda s: min(1, (s + 1) / (0.06 * steps)) * max(0.0, (steps - s) / steps))
def evalv():
    model.eval(); out = []
    with torch.no_grad():
        for i in range(0, len(va), 64):
            b = va[i:i+64]; out += model(**enc(b)).logits.squeeze(-1).tolist()
    model.train(); return out
def auc(P, N): return sum((p > n) + .5 * (p == n) for p in P for n in N) / max(1, len(P) * len(N))
t0 = time.time(); step = 0
for ep in range(EPOCHS):
    random.shuffle(tr)
    for i in range(0, len(tr), BS):
        b = tr[i:i+BS]; y = torch.stack([target(r) for r in b])
        loss = torch.nn.functional.binary_cross_entropy_with_logits(model(**enc(b)).logits.squeeze(-1), y)
        loss.backward(); torch.nn.utils.clip_grad_norm_(model.parameters(), 1.0); opt.step(); sched.step(); opt.zero_grad(); step += 1
        if step % 40 == 0: print(f'ep{ep} step{step}/{steps} loss {loss.item():.4f} {time.time()-t0:.0f}s', flush=True)
    s = evalv(); yv = [r['p'] >= 0.16 for r in va]
    print(f'ep{ep} val AUC vs Jev>=0.16: {auc([x for x, y in zip(s, yv) if y], [x for x, y in zip(s, yv) if not y]):.3f}', flush=True)
model.eval(); model.save_pretrained(D + '/model', safe_serialization=True); tok.save_pretrained(D + '/model')
json.dump(dict(val=[dict(k=r['k'], p=r['p'], s=x) for r, x in zip(va, s)]), open(D + '/val.json', 'w'))
print('saved')
