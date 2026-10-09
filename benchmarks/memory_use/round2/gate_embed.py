"""Embed every round 1/2 prompt with the same model the server uses (nomic-embed-text via Ollama) -> bench/gate/emb.json"""
import sys, json, urllib.request, concurrent.futures as cf
sys.path.insert(0,'/mnt/bulk/grimoire-am/benchmarks/memory_use/round2')
from gate_common import *
d=json.load(open(OUT+'cands.json')); ps=sorted({x['prompt'] for x in d})
def e(p):
    r=urllib.request.Request('http://100.127.85.58:11434/api/embeddings',json.dumps({'model':'nomic-embed-text','prompt':p}).encode(),{'Content-Type':'application/json'})
    return json.load(urllib.request.urlopen(r,timeout=60))['embedding']
with cf.ThreadPoolExecutor(6) as ex: v=list(ex.map(e,ps))
json.dump(dict(zip(ps,v)),open(OUT+'emb.json','w')); print(len(ps))
