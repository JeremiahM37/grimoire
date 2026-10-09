import sys; sys.path.insert(0,'/mnt/bulk/grimoire-am/benchmarks/memory_use/round2')
from gate_common import *
import statistics
def pr(who):
    o=json.load(open(OUT+f'pairs_{who}.json'))
    for ph in ('p0','p1'):
        for name,lo,hi in(('band .5-.75',.5,.75),('wide .35-.9',.35,.9)):
            x=[a for a in o if a['ph']==ph and a['p'] is not None and lo<=a['score']<hi]
            P=[a['p'] for a in x if a['kind']=='pos_prompt']; N=[a['p'] for a in x if a['kind']=='neg_prompt']
            print(f"{who} {ph} {name}: nP={len(P)} nN={len(N)} AUC {auc(P,N):.3f} mean yes pos {statistics.mean(P):.3f} neg {statistics.mean(N):.3f}")
for w in sys.argv[1:]: pr(w)
