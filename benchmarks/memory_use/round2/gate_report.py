"""End result table: per gate option -> hits kept %, near-miss false fires removed %, added latency.
Calibrated on round 1 (threshold giving >=90% of in-band hits kept), tested on round 2.
usage: gate_report.py -> bench/gate/report.json + printed table"""
import sys, random
sys.path.insert(0,'/mnt/bulk/grimoire-am/benchmarks/memory_use/round2')
from gate_common import *; from gate_score_only import feats, fit
import numpy as np, statistics
KEEP=float(sys.argv[1]) if len(sys.argv)>1 else 0.90
d=load(); emb={k:np.array(v)/np.linalg.norm(v) for k,v in json.load(open(OUT+'emb.json')).items()}
key=lambda x:(x['round'],x['prompt'],x['mem_id'])
D={key(x):x for x in d if x['gold']}
def pairs(who):
    o=json.load(open(OUT+f'pairs_{who}.json')); r={}
    for a in o:
        if a['p'] is not None: r.setdefault(a['ph'],{})[(a['round'],a['prompt'],a['mem_id'])]=a['p']
    return r
def inband(rnd): return [x for x in d if x['round']==rnd and x['gold'] and .5<=x['gold']['score']<.75]
def above(rnd): return [x for x in d if x['round']==rnd and x['gold'] and x['gold']['score']>=.75]
def calib(scores_pos,scores_neg):
    # largest threshold keeping >=KEEP of positives
    ps=sorted(scores_pos); i=int((1-KEEP)*len(ps)); return ps[i]
def evaluate(name,score,lat,thr=None):
    """score: dict key->p (higher = keep); thr chosen on round 1 unless given."""
    r1=inband(1); r2=inband(2)
    sp=[score[key(x)] for x in r1 if x['kind']=='pos_prompt' and key(x) in score]; sn=[score[key(x)] for x in r1 if x['kind']!='pos_prompt' and key(x) in score]
    t=thr if thr is not None else calib(sp,sn)
    P=[score[key(x)] for x in r2 if x['kind']=='pos_prompt' and key(x) in score]; N=[score[key(x)] for x in r2 if x['kind']!='pos_prompt' and key(x) in score]
    kp=sum(p>=t for p in P)/len(P); rn=sum(n<t for n in N)/len(N)
    # end-to-end on all injected (score>=.5): above-band always kept
    ap=sum(1 for x in above(2) if x['kind']=='pos_prompt'); an=sum(1 for x in above(2) if x['kind']!='pos_prompt')
    allp=len(P)+ap; alln=len(N)+an
    return dict(name=name,thr=round(float(t),3),auc=round(auc(P,N),3),hits_kept=round(100*kp,1),fp_removed=round(100*rn,1),
                e2e_hits_kept=round(100*(sum(p>=t for p in P)+ap)/allp,1),e2e_fp_removed=round(100*sum(n<t for n in N)/alln,1),
                n_pos=len(P),n_neg=len(N),**lat)
rows=[]
LAT={}
for w in('jev','laya'):
    try: l=json.load(open(OUT+f'latency_{w}.json')); LAT[w]={k:(round(statistics.median(l[k])),round(sorted(l[k])[int(.95*len(l[k]))-1])) for k in('s1','par3','batch3')}
    except Exception as e: LAT[w]=None
def lat(w,k): 
    if not LAT.get(w): return dict(lat_p50=None,lat_p95=None)
    a,b=LAT[w][k]; return dict(lat_p50=a,lat_p95=b)
rows.append(dict(name='no gate',thr=None,auc=None,hits_kept=100.0,fp_removed=0.0,e2e_hits_kept=100.0,e2e_fp_removed=0.0,lat_p50=0,lat_p95=0))
for w in('jev','laya'):
    try: P=pairs(w)
    except Exception: continue
    for ph in('p0','p1'):
        rows.append(evaluate(f'{w} {ph} thr 0.5 (shipped default)',P[ph],lat(w,'par3'),thr=0.5))
        rows.append(evaluate(f'{w} {ph} calibrated, 3 parallel',P[ph],lat(w,'par3')))
        if ph=='p1': rows.append(evaluate(f'{w} {ph} calibrated, 1 batched call',P[ph],lat(w,'batch3')))
# score-only
tr=[x for x in d if x['round']==1 and x['gold'] and .35<=x['gold']['score']<.9]
for nm,cols in(('score-only LR (all features)',list(range(9))),('score-only LR (no query length)',[0,1,2,3,4,6,7,8])):
    f,w=fit([[feats(x)[c] for c in cols] for x in tr],[x['kind']=='pos_prompt' for x in tr])
    sc={key(x):float(f([[feats(x)[c] for c in cols]])[0]) for x in d if x['gold']}
    rows.append(evaluate(nm,sc,dict(lat_p50=0,lat_p95=0)))
# combos: LR (no query length) + Jev p1 averaged in logit space, and a cascade that only calls Jev when LR is unsure
def lg(p): p=min(max(p,1e-3),1-1e-3); return float(np.log(p/(1-p)))
cols=[0,1,2,3,4,6,7,8]; f2,_=fit([[feats(x)[c] for c in cols] for x in tr],[x['kind']=='pos_prompt' for x in tr])
lr2={key(x):float(f2([[feats(x)[c] for c in cols]])[0]) for x in d if x['gold']}
Pj1=pairs('jev')['p1']
mix={k:(lg(lr2[k])+lg(Pj1[k]))/2 for k in Pj1 if k in lr2}
rows.append(evaluate('LR(no qlen) + Jev p1 averaged, 3 parallel',mix,lat('jev','par3')))
r1s=sorted(lr2[key(x)] for x in inband(1))
lo,hi=r1s[int(.25*len(r1s))],r1s[int(.75*len(r1s))]
casc={k:(mix[k] if lo<=lr2[k]<=hi else lg(lr2[k])*3) for k in mix}
ev=evaluate('cascade: LR decides outer half, Jev p1 asked on the middle half',casc,lat('jev','par3'))
ev['jev_call_share']=round(float(np.mean([lo<=lr2[key(x)]<=hi for x in inband(2)])),2); rows.append(ev)
# async learned suppression: Jev p1 verdicts arrive off the critical path; per-memory nearest-exemplar suppression
def async_sim(verd,theta,seed=0,margin=0.0):
    rnd=random.Random(seed); mem={}  # mem_id -> [(emb,yes)]
    out={}
    for r in(1,2):
        xs=[x for x in d if x['round']==r and x['gold'] and .5<=x['gold']['score']<.75]; rnd.shuffle(xs)
        for x in xs:
            k=key(x); e=emb[x['prompt']]; ex=mem.get(x['mem_id'],[])
            sN=max([float(e@v) for v,y in ex if not y],default=0); sY=max([float(e@v) for v,y in ex if y],default=0)
            out[k]=0.0 if (sN>=theta and sN>sY+margin) else 1.0   # score 0 = suppressed before injection
            if k in verd: mem.setdefault(x['mem_id'],[]).append((e,verd[k]>=0.5))   # judged afterwards, async
    return out
Pj=pairs('jev')['p1']
# pick theta on round 1 only
best=None
for th in(0.5,0.55,0.6,0.65,0.7,0.75,0.8):
    sc=async_sim(Pj,th)
    r1=inband(1); hk=np.mean([sc[key(x)] for x in r1 if x['kind']=='pos_prompt']); fr=np.mean([1-sc[key(x)] for x in r1 if x['kind']!='pos_prompt'])
    print('async theta',th,'r1 hits kept %.3f fp removed %.3f'%(hk,fr))
    if hk>=KEEP and (best is None or fr>best[1]): best=(th,fr)
th=best[0] if best else 0.9
res=[evaluate(f'async learned suppression (theta {th}, seed {s})',async_sim(Pj,th,s),dict(lat_p50=0,lat_p95=0),thr=0.5) for s in range(5)]
agg=dict(res[0]); 
for k in('hits_kept','fp_removed','e2e_hits_kept','e2e_fp_removed'): agg[k]=round(float(np.mean([r[k] for r in res])),1)
agg['name']=f'async learned suppression (Jev p1 judge off-path, theta {th}, mean of 5 orders)'; agg['auc']=None; rows.append(agg)
print(f"\ncalibration: threshold keeps >={int(KEEP*100)}% of round-1 in-band hits; test = round 2 in-band (hits {res[0]['n_pos']}, near-misses {res[0]['n_neg']})")
print(f"{'option':66s} thr   AUC  hits  fpRem  e2eHit e2eFP  p50  p95")
for r in rows: print(f"{r['name']:66s} {str(r['thr']):5s} {str(r['auc']):5s} {r['hits_kept']:5} {r['fp_removed']:5} {r['e2e_hits_kept']:6} {r['e2e_fp_removed']:5} {r['lat_p50']} {r['lat_p95']}")
json.dump(rows,open(OUT+'report.json','w'),indent=1)
