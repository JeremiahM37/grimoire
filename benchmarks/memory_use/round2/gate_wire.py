import json,time,urllib.request,yaml
import os
# JEV_CONFIG names a YAML file with jev.base_url and jev.api_key (kept outside the repo).
_cfg=yaml.safe_load(open(os.path.expanduser(os.environ.get('JEV_CONFIG','~/.ai-secrets/jev.yaml'))))['jev']
JEV=_cfg['base_url'].rstrip('/')+'/v1/systemone'
LAYA='http://127.0.0.1:8765/v1/systemone'
def call(url,state,questions,model,timeout=30):
    h={"Content-Type":"application/json"}
    if url==JEV: h["Authorization"]="Bearer "+_cfg['api_key']
    body=json.dumps({"state":state,"model":model,"questions":questions}).encode()
    t=time.time()
    try:
        r=json.load(urllib.request.urlopen(urllib.request.Request(url,body,h),timeout=timeout))
        return r,(time.time()-t)*1000
    except Exception as e:
        return {'error':str(e).replace(_cfg['api_key'],'[k]')[:200]},(time.time()-t)*1000
def clip(s,n): 
    s=' '.join(s.split()); return s if len(s)<=n else s[:n]
Q0={"applies":{"type":"noul","instructions":"Would this memory change what the agent does for this request?"}}
