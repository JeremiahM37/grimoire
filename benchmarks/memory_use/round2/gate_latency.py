"""Latency of the gate on real in-band requests, and batching.
usage: gate_latency.py {jev|laya}  -> bench/gate/latency_<who>.json
Modes: s1 = one candidate; par3 = three candidates in three parallel calls (wall time);
batch3 = three candidates, one call, three questions; batch3_ctx = same, memories numbered in state."""
import sys, random, concurrent.futures as cf, statistics
sys.path.insert(0,'/mnt/bulk/grimoire-am/benchmarks/memory_use/round2')
from gate_common import *; from gate_wire import *
who=sys.argv[1]; url,model=(JEV,'jev-latest') if who=='jev' else (LAYA,'volatility')
N=int(sys.argv[2]) if len(sys.argv)>2 else 60
random.seed(7)
d=load()
rows=[]
for x in d:
    band=[c for c in x['cands'] if .5<=c['score']<.75]
    if len(band)>=3: rows.append((x,sorted(band,key=lambda c:-c['score'])[:3]))
print('requests with >=3 in-band candidates:',len(rows))
rows=random.sample(rows,min(N,len(rows)))
def st1(q,c): return f"Memory: {clip(c['text'],280)}\nRequest: {clip(q,240)}"
def pv(r,k='applies'):
    try: return r['answers'][k]['noul']
    except Exception: return None
res={'s1':[],'par3':[],'batch3':[],'batch3_p':[],'single_p':[]}
for x,band in rows:
    q=x['prompt']
    r,ms=call(url,st1(q,band[0]),Q0,model); res['s1'].append(ms)
    with cf.ThreadPoolExecutor(3) as ex:
        t=time.time(); out=list(ex.map(lambda c:call(url,st1(q,c),Q0,model),band)); res['par3'].append((time.time()-t)*1000)
    res['single_p'].append([pv(o[0]) for o in out])
    state=f"Request: {clip(q,240)}\n"+"\n".join(f"Memory {i+1}: {clip(c['text'],280)}" for i,c in enumerate(band))
    qs={f"m{i+1}":{"type":"noul","instructions":f"Would Memory {i+1} change what the agent does for this request?"} for i in range(3)}
    r,ms=call(url,state,qs,model); res['batch3'].append(ms); res['batch3_p'].append([pv(r,f"m{i+1}") for i in range(3)])
def pct(a,p): a=sorted(a); return a[min(len(a)-1,int(p*len(a)))]
for k in('s1','par3','batch3'):
    print(who,k,'n=%d p50 %.0f p95 %.0f ms'%(len(res[k]),statistics.median(res[k]),pct(res[k],.95)))
a=[p for r in res['single_p'] for p in r if p is not None]; b=[p for r,s in zip(res['batch3_p'],res['single_p']) for p,q in zip(r,s) if p is not None and q is not None]
s=[q for r,s in zip(res['batch3_p'],res['single_p']) for p,q in zip(r,s) if p is not None and q is not None]
import numpy as np
print('batched answers parsed',len(b),'of',3*len(rows),'corr single vs batched %.3f'%np.corrcoef(b,s)[0,1] if len(b)>3 else 'n/a','batched spread (std) %.3f single std %.3f'%(np.std(b) if b else 0,np.std(s) if s else 0))
json.dump(res,open(OUT+f'latency_{who}.json','w'))
