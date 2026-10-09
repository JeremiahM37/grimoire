"""Generate retrieval cues for each memory with the local qwen model.

A cue is a short situation in which an agent should recall the memory:
a user request phrased differently from the memory, a command or file path
the agent may be about to run or touch, or a keyword set. Cues are built from
the memory text ONLY (never from benchmark prompts)."""
import concurrent.futures as cf
import json
import re
import sys
import time
import urllib.request

OLLAMA = 'http://100.127.85.58:11434/api/chat'
MODEL = 'qwen3.6:35b-a3b'
PROMPT = """You index memories for a coding/ops AI agent working on a Linux homelab.
Memory:
<<<
{text}
>>>
List situations in which the agent must recall this memory. Output ONLY lines in this format:
R: <a short request a user might type, using different words from the memory>  (write 4)
A: <a shell command or file path the agent may be about to run or edit when this memory applies>  (write 3; write "A: none" if no tool call relates)
K: <5 to 8 search keywords, including synonyms not in the memory>  (write 1)"""

def cues_for(text):
    body = {'model': MODEL, 'think': False, 'stream': False, 'options': {'temperature': 0.3, 'num_predict': 300, 'num_ctx': 4096},
            'messages': [{'role': 'user', 'content': PROMPT.format(text=text[:1500])}]}
    req = urllib.request.Request(OLLAMA, json.dumps(body).encode(), {'Content-Type': 'application/json'})
    out = json.load(urllib.request.urlopen(req, timeout=120))['message']['content']
    cues = []
    for line in out.splitlines():
        m = re.match(r'\s*([RAK])\s*:\s*(.+)', line)
        if m and m.group(2).strip().lower() not in ('none', '"none"'):
            cues.append({'kind': {'R': 'request', 'A': 'action', 'K': 'keywords'}[m.group(1)], 'text': m.group(2).strip()[:300]})
    return cues

if __name__ == '__main__':
    items = [json.loads(line) for line in open(sys.argv[1])]
    out = open(sys.argv[2], 'a')
    done = set()
    try:
        done = {json.loads(line)['id'] for line in open(sys.argv[2])}
    except Exception: pass
    todo = [i for i in items if i['id'] not in done]
    t0 = time.time()
    def work(i):
        try: return {'id': i['id'], 'cues': cues_for(i['text'])}
        except Exception as e: return {'id': i['id'], 'error': str(e)[:200]}
    with cf.ThreadPoolExecutor(int(sys.argv[3]) if len(sys.argv) > 3 else 2) as ex:
        for n, r in enumerate(ex.map(work, todo), 1):
            out.write(json.dumps(r) + '\n'); out.flush()
            if n % 25 == 0: print(n, len(todo), f'{time.time()-t0:.0f}s', flush=True)
