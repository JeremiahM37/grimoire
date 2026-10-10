"""The existing use signals, computed the way production computes them.

* tag: the agent cited `m:<tag>` in text a hook can see (assistant text, not thinking).
* fingerprint: the memory's fingerprints (go/cmd/fpdump, the production selector, with the
  trigger prompt as the situation and the whole benchmark plus the adherence corpus as the
  store) hashed with a salt and matched against tool targets and the turn's text with the
  production hook code (clients/hooks/grimoire_outcome.py).
Both are bytes in, boolean out: no model.
"""
import importlib.util
import json
import os
import re
import subprocess

ROOT = os.path.abspath(os.path.join(os.path.dirname(__file__), '..', '..', '..'))
spec = importlib.util.spec_from_file_location('outcome_hook', os.path.join(ROOT, 'clients/hooks/grimoire_outcome.py'))
hook = importlib.util.module_from_spec(spec)
spec.loader.exec_module(hook)
SALT = 'trace-eval-salt-0123456789ab'
FPDUMP = os.environ.get('FPDUMP', '/tmp/claude-1000/fpdump')
CASES = os.environ.get('TRACE_CASES', 'cases.json')  # adherence cases; see README
TAG = re.compile(r'\bm:([0-9a-f]{4,8})\b')


def build_fp(tasks):
    """{(task_id, mem_id): [tokens]} for every memory of every task."""
    corpus = {m['text'] for t in tasks for m in t['memories']}
    if os.path.exists(CASES):
        for c in json.load(open(CASES)):
            corpus.add(c['mem_text'][:900])
    items = [dict(ID=f"{t['id']}|{m['id']}", Text=m['text'], Situation=t['prompt']) for t in tasks for m in t['memories']]
    req = dict(corpus=sorted(corpus), min_score=0, max_df=0, items=items)
    out = subprocess.run([FPDUMP], input=json.dumps(req), capture_output=True, text=True, check=True).stdout
    raw = json.loads(out)
    return {tuple(k.split('|')): [f['token'] for f in v] for k, v in raw.items()}


def hashes(tokens):
    return {hook.fp_hash(SALT, t) for t in tokens}


def matched(tokens, text):
    """Distinct fingerprints of the memory present in text (production matcher)."""
    if not tokens:
        return None  # unfingerprintable: no verdict
    have = {hook.fp_hash(SALT, t) for t in hook.candidates(text or '')}
    return len(hashes(tokens) & have)


def action_target(name, args):
    cn = {'bash': 'Bash', 'edit_file': 'Edit', 'write_file': 'Write'}.get(name)
    if not cn:
        return ''  # read-only tools are not in the hook's ACTION_TOOLS
    key = {'bash': 'command', 'edit_file': 'path', 'write_file': 'path'}[name]
    return str(args.get(key, '') or '')


def run_signals(run, tokens_by_mem):
    """Per memory in the run: dict(tag, fp, fp_n, fingerprintable)."""
    texts = [s['content'] for s in run['steps'] if s['content'].strip()]
    text = '\n'.join(texts)
    cited = set(TAG.findall(text))
    targets = [action_target(c['name'], c['args']) for s in run['steps'] for c in s['calls']]
    out = {}
    for m in run['memories']:
        toks = tokens_by_mem.get((run['task_id'], m['id']), [])
        n = 0 if not toks else max(matched(toks, t) or 0 for t in targets + [text]) if (targets or text) else 0
        # production counts distinct fingerprints matched so far across the turn
        if toks:
            seen = set()
            hs = hashes(toks)
            for t in targets + [text]:
                seen |= {hook.fp_hash(SALT, c) for c in hook.candidates(t)} & hs
            n = len(seen)
        out[m['id']] = dict(tag=m['tag'] in cited, fp=bool(toks) and n >= 1, fp_n=n, fingerprintable=bool(toks))
    return out
