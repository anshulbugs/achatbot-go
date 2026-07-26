import json,time,threading,urllib.request,statistics,sys
PORT=sys.argv[1]; CONC=int(sys.argv[2]); N=int(sys.argv[3])
SYS=open("/tmp/sysprompt.txt").read()
MSGS=[{"role":"system","content":SYS},
      {"role":"assistant","content":"Hey there, thanks so much for taking my call! How is your day going?"},
      {"role":"user","content":"It is going pretty well thanks. Can you tell me more about the role and what the team looks like?"}]
lat=[]; ttft=[]; errs=[0]; lock=threading.Lock()
def one():
    body=json.dumps({"model":"Qwen/Qwen2.5-3B-Instruct","messages":MSGS,"max_tokens":80,"temperature":0.7,"stream":True}).encode()
    req=urllib.request.Request("http://127.0.0.1:%s/v1/chat/completions"%PORT,data=body,headers={"Content-Type":"application/json"})
    t0=time.time(); first=None
    try:
        with urllib.request.urlopen(req,timeout=120) as r:
            for line in r:
                if line.startswith(b"data:") and b"content" in line and first is None:
                    first=time.time()-t0
        tot=time.time()-t0
        with lock:
            if first is not None: ttft.append(first*1000)
            lat.append(tot*1000)
    except Exception as e:
        with lock: errs[0]+=1
sem=threading.Semaphore(CONC); ths=[]
def run():
    with sem: one()
t0=time.time()
for _ in range(N):
    t=threading.Thread(target=run); t.start(); ths.append(t)
for t in ths: t.join()
wall=time.time()-t0
def p(a,q):
    a=sorted(a); return a[min(len(a)-1,int(len(a)*q))] if a else 0
print("port=%s conc=%d n=%d | TTFT p50=%4.0fms p95=%4.0fms | total p50=%4.0fms p95=%5.0fms | %5.1f req/s | errs=%d"%(
   PORT,CONC,N,p(ttft,.5),p(ttft,.95),p(lat,.5),p(lat,.95),len(lat)/wall,errs[0]))
