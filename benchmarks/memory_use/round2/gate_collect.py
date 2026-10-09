"""Collect, for every round 1 and round 2 prompt case, the candidate list the context endpoint
builds (scores before any gate), from the debug-patched test server on :9198.
usage: gate_collect.py   -> bench/gate/cands.json"""
import json, sys, urllib.parse, urllib.request, concurrent.futures as cf
B='/mnt/bulk/memory-use-research/'
base='http://127.0.0.1:9198'
cases=[]
for rnd,f in ((1,'bench/cases.json'),(2,'bench/cases2.json')):
    for c in json.load(open(B+f)):
        if c['kind'] in('pos_prompt','neg_prompt'):
            c['round']=rnd; cases.append(c)
def one(c):
    p={'q':c['prompt'],'cwd':c.get('cwd',''),'rank':'hybrid','min_rel':'0','limit':'50','debug':'1','format':'json'}
    r=json.load(urllib.request.urlopen(base+'/api/memory/context?'+urllib.parse.urlencode(p),timeout=60))
    return dict(c, cands=r.get('cands') or [])
with cf.ThreadPoolExecutor(4) as ex: out=list(ex.map(one,cases))
json.dump(out,open(B+'round2/bench/gate/cands.json','w'))
print(len(out))
