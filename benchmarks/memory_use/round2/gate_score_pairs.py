"""Ask Jev and Laya about every (request, gold-candidate) pair with gold score in [0.35,0.9):
two phrasings each. usage: gate_score_pairs.py {jev|laya}   -> bench/gate/pairs_<who>.json"""
import sys, json, concurrent.futures as cf
sys.path.insert(0,'/mnt/bulk/grimoire-am/benchmarks/memory_use/round2')
from gate_common import *; from gate_wire import *
who=sys.argv[1]
url,model,thr=(JEV,'jev-latest',8) if who=='jev' else (LAYA,'volatility',1)
QS={'p0':Q0,
    'p1':{"applies":{"type":"noul","instructions":"Is this stored memory directly about the task the user is asking for right now, so that the agent would act differently without it? Sharing a topic or a project name is not enough.",
          "criteria":{"true":"the memory states a rule, fact or preference that governs this exact request","false":"the memory is only on the same topic, or the request does not touch what it says"}}}}
d=load()
pairs=[x for x in d if x['gold'] and .35<=x['gold']['score']<.9]
def st(x,mode):
    m=clip(x['gold']['text'],280); q=clip(x['prompt'],240)
    return f"Memory: {m}\nRequest: {q}"
def one(a):
    x,ph=a
    r,ms=call(url,st(x,ph),QS[ph],model)
    try: p=r['answers']['applies']['noul']
    except Exception: p=None
    return dict(case=x['case_id'] if 'case_id' in x else None,mem_id=x['mem_id'],kind=x['kind'],round=x['round'],prompt=x['prompt'],score=x['gold']['score'],ph=ph,p=p,ms=ms,err=r.get('error'))
jobs=[(x,ph) for x in pairs for ph in QS]
with cf.ThreadPoolExecutor(thr) as ex: out=list(ex.map(one,jobs))
json.dump(out,open(OUT+f'pairs_{who}.json','w'))
print(who,len(out),'errors',sum(1 for o in out if o['p'] is None))
