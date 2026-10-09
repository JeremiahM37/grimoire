import json,collections,math
B='/mnt/bulk/memory-use-research/'
OUT=B+'round2/bench/gate/'
def gm(mid,c):
    store,key=mid.split(':',1)
    if store=='am': return c['path']==f"Agent Memory/{key.split('#')[0]}.md"
    if store=='gm': return c['id']==key
    if store=='cm': return c['path']==f"Instructions/cm_{key}.md"
def load():
    d=json.load(open(OUT+'cands.json'))
    mem={i['id']:i['text'] for i in json.load(open(B+'data/memory_items.json'))}
    for x in d:
        for c in x['cands']: c['isgold']=gm(x['mem_id'],c)
        g=[c for c in x['cands'] if c['isgold']]
        x['gold']=max(g,key=lambda c:c['score']) if g else None
    return d
def auc(P,N):
    if not P or not N: return float('nan')
    return sum((p>n)+0.5*(p==n) for p in P for n in N)/(len(P)*len(N))
