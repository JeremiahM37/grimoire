"""Write generated cues into the replay vault's cue store (.grimoire/cues.jsonl).
usage: load_cues.py cues.jsonl [learned.jsonl]"""
import datetime
import json
import sys

V = '/mnt/bulk/memory-use-research/vault/.grimoire/cues.jsonl'
def target(mid):
    store, key = mid.split(':', 1)
    if store == 'am': return 'note:Agent Memory/' + key.split('#')[0] + '.md'
    if store == 'gm': return 'fact:' + key
    if store == 'cm': return 'note:Instructions/cm_' + key + '.md'
now = datetime.datetime.now(datetime.UTC).isoformat()
n = 0
with open(V, 'w') as out:
    for f in sys.argv[1:]:
        for line in open(f):
            d = json.loads(line)
            for c in d.get('cues', []):
                out.write(json.dumps({'target': target(d['id']), 'kind': c['kind'], 'text': c['text'],
                                      'source': c.get('source', 'generated'), 'added': now}) + '\n'); n += 1
print('cues written', n)
