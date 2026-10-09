"""Free gate: calibrated logistic score on retrieval signals. Fit on round 1, test on round 2.
features: relevance, cosine, term overlap, cue score, margin to best other candidate, query length, instruction-file flag, rank.
usage: gate_score_only.py -> bench/gate/score_only.json"""
import sys; sys.path.insert(0,'/mnt/bulk/grimoire-am/benchmarks/memory_use/round2')
from gate_common import *
import numpy as np
def feats(x):
    g=x['gold']; others=[c['score'] for c in x['cands'] if not c['isgold']]
    best=max(others) if others else 0
    srt=sorted([c['score'] for c in x['cands']],reverse=True)
    rank=srt.index(g['score'])
    return [g['score'],g['cos'],g['ov'],g['cue'],g['score']-best,min(len(x['prompt'].split()),60)/60,1.0 if g['path'].startswith('Instructions/') else 0.0,min(rank,5)/5,sum(1 for s in srt if s>=.5)/10]
NAMES=['score','cos','overlap','cue','margin','qlen','instr','rank','ncand']
def fit(X,y,l2=1.0,it=3000,lr=0.5):
    X=np.array(X);y=np.array(y,float);mu=X.mean(0);sd=X.std(0)+1e-9
    Z=(X-mu)/sd;Z=np.c_[Z,np.ones(len(Z))];w=np.zeros(Z.shape[1])
    wt=np.where(y==1,0.5/y.sum(),0.5/(1-y).sum())*len(y)  # balance classes
    for _ in range(it):
        p=1/(1+np.exp(-Z@w)); g=Z.T@((p-y)*wt)/len(y); g[:-1]+=l2*w[:-1]/len(y); w-=lr*g
    return lambda Xn:1/(1+np.exp(-(np.c_[(np.array(Xn)-mu)/sd,np.ones(len(Xn))]@w))),dict(zip(NAMES+['bias'],w.round(3).tolist()))
if __name__=='__main__':
    d=load()
    sel=lambda r,lo,hi:[x for x in d if x['round']==r and x['gold'] and lo<=x['gold']['score']<hi]
    tr=sel(1,.35,.9); te=sel(2,.5,.75); te_wide=sel(2,.35,.9)
    f,wts=fit([feats(x) for x in tr],[x['kind']=='pos_prompt' for x in tr])
    print('weights',wts)
    for nm,t in(('r2 band',te),('r2 wide',te_wide)):
        p=f([feats(x) for x in t]); P=[a for a,x in zip(p,t) if x['kind']=='pos_prompt']; N=[a for a,x in zip(p,t) if x['kind']!='pos_prompt']
        print(nm,'LR AUC %.3f'%auc(P,N),'| relevance-alone AUC %.3f'%auc([x['gold']['score'] for x in t if x['kind']=='pos_prompt'],[x['gold']['score'] for x in t if x['kind']!='pos_prompt']),
              '| cue-alone %.3f'%auc([x['gold']['cue'] for x in t if x['kind']=='pos_prompt'],[x['gold']['cue'] for x in t if x['kind']!='pos_prompt']),
              '| margin-alone %.3f'%auc([feats(x)[4] for x in t if x['kind']=='pos_prompt'],[feats(x)[4] for x in t if x['kind']!='pos_prompt']),
              '| qlen-alone %.3f'%auc([feats(x)[5] for x in t if x['kind']=='pos_prompt'],[feats(x)[5] for x in t if x['kind']!='pos_prompt']))
    # r1 in-band for reference (train fit)
    t=sel(1,.5,.75); p=f([feats(x) for x in t]); print('r1 band (train) AUC %.3f'%auc([a for a,x in zip(p,t) if x['kind']=='pos_prompt'],[a for a,x in zip(p,t) if x['kind']!='pos_prompt']))
    json.dump({'weights':wts},open(OUT+'score_only.json','w'))
