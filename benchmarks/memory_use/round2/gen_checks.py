"""Ask qwen once per case for regex checks on the decisive action. Saves checks_raw.json."""
import json, re, sys
from common import *
cases = json.load(open(CASES))
out = {}
P = """Write a deterministic grader (regular expressions) for an agent's one-line answer.
Memory the agent should respect:
{mem}

Task given to the agent: {task}
Rule: {check}
A FOLLOWING answer looks like: {right}
A VIOLATING answer looks like: {wrong}

Output JSON only: {{"follow": [...], "violate": [...]}}
- "follow": 2-5 SHORT Python regexes (case-insensitive); an answer counts as following if ANY ONE matches. Each is a distinct short token or phrase that a following answer would contain (a command, flag, path, tag, or a word stem such as "ask(s|ing)? (which|what)"), covering different ways of phrasing the correct decision. Prefer short tokens over long phrases. Never use `.*` across more than ~30 characters.
- "violate": 1-4 short regexes; if ANY matches, the answer does the wrong thing (the VIOLATING example's decisive tokens). Do not use a violate regex that would also match a correct answer that merely mentions the forbidden thing in a refusal.
Use single backslashes only where needed (JSON-escape them)."""
for c in cases:
    r = chat(P.format(mem=c['mem_text'][:900], task=c['task'], check=c['check'], right=c['right'], wrong=c['wrong']),
             system='You write precise regexes. JSON only.', temperature=0, num_predict=600)
    m = re.search(r'\{.*\}', r, re.S)
    try:
        j = json.loads(m.group(0)); re.compile('|'.join(j['follow'] + j['violate']))
        out[c['mem_id']] = {'follow': j['follow'], 'violate': j['violate']}
    except Exception as e:
        out[c['mem_id']] = {'follow': [], 'violate': [], 'error': str(e), 'raw': r[:300]}
    print(c['mem_id'], 'ok' if 'error' not in out[c['mem_id']] else 'ERR', flush=True)
json.dump(out, open(BENCH + '/checks_raw.json', 'w'), indent=1)
