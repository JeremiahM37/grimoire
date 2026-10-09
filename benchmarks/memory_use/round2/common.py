"""Shared helpers for WS4 round-2 benchmarks (stdlib only)."""
import json, re, time, urllib.request
OLLAMA = 'http://100.127.85.58:11434'
MODEL = 'qwen3.6:35b-a3b'
BENCH = '/mnt/bulk/memory-use-research/round2/bench'
CASES = '/mnt/bulk/memory-use-research/adherence/cases.json'
HAIKU = '/mnt/bulk/memory-use-research/adherence/results.json'
SYS = ("You are a coding and operations agent working on the user's Linux homelab. "
       "Answer with the single next action only.")
ASK = ("\n\nAssume you have already looked around and have everything you need. State the decisive action "
       "you take to complete this request (the command, edit or decision that matters), in one line.")

def chat(prompt, system=SYS, think=False, num_predict=400, temperature=0.7, seed=None):
    opts = {'num_ctx': 16384, 'temperature': temperature, 'num_predict': num_predict}
    if seed is not None: opts['seed'] = seed
    body = {'model': MODEL, 'stream': False, 'think': think, 'options': opts,
            'messages': [{'role': 'system', 'content': system}, {'role': 'user', 'content': prompt}]}
    for i in range(3):
        try:
            req = urllib.request.Request(OLLAMA + '/api/chat', json.dumps(body).encode(), {'Content-Type': 'application/json'})
            d = json.load(urllib.request.urlopen(req, timeout=300))
            return d['message']['content'].strip()
        except Exception as e:
            time.sleep(5)
    return ''

def judge(check, text):
    """Deterministic three-way grade. check = {'follow':[regex..any..], 'violate':[regex..any..]}"""
    t = text or ''
    f = any(re.search(p, t, re.I | re.S) for p in check['follow'])
    v = any(re.search(p, t, re.I | re.S) for p in check['violate'])
    if f and not v: return 'FOLLOWS'
    if v and not f: return 'VIOLATES'
    return 'UNCLEAR'
