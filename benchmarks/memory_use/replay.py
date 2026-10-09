"""Replay real user messages, per session in order, through the context hook's
query, and record every learned cue the server creates from a re-tell."""
import collections
import json
import urllib.parse
import urllib.request

base = 'http://127.0.0.1:9141'
cue_file = '/mnt/bulk/memory-use-research/vault/.grimoire/cues.jsonl'
msgs = [json.loads(line) for line in open('../data/human_msgs.jsonl')]
msgs.sort(key=lambda m: (m['sid'], m['j']))
log = []
seen = sum(1 for _ in open(cue_file))
for n, m in enumerate(msgs, 1):
    q = m['text'].strip()[:2000]
    if not q:
        continue
    urllib.request.urlopen(base + '/api/memory/context?' + urllib.parse.urlencode(
        {'q': q, 'rank': 'hybrid', 'min_rel': 0.5, 'session': m['sid'], 'format': 'json'}), timeout=120).read()
    lines = open(cue_file).read().splitlines()
    for line in lines[seen:]:
        c = json.loads(line)
        log.append({'sid': m['sid'], 'correction': q, 'cue': c['text'], 'target': c['target']})
    seen = len(lines)
    if n % 200 == 0:
        print(n, len(msgs), 'learned', len(log), flush=True)
json.dump(log, open('replay_learned.json', 'w'), indent=1)
print('messages', len(msgs), 'learned cues', len(log), 'sessions', len({line['sid'] for line in log}))
print(collections.Counter(line['target'] for line in log).most_common(8))
