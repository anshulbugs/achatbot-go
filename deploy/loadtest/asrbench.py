import time,threading,urllib.request,sys
CONC=int(sys.argv[1]); N=int(sys.argv[2])
DATA=open("/tmp/spk16.pcm","rb").read()
lat=[]; lock=threading.Lock(); sem=threading.Semaphore(CONC)
def one():
    with sem:
        r=urllib.request.Request("http://127.0.0.1:8890/asr",data=DATA,headers={"Content-Type":"application/octet-stream"})
        t0=time.time()
        try:
            with urllib.request.urlopen(r,timeout=120) as resp: resp.read()
            with lock: lat.append((time.time()-t0)*1000)
        except Exception as e:
            with lock: lat.append(-1)
ths=[threading.Thread(target=one) for _ in range(N)]
t0=time.time()
for t in ths: t.start()
for t in ths: t.join()
ok=sorted(v for v in lat if v>0); el=time.time()-t0
p=lambda q: ok[min(len(ok)-1,int(len(ok)*q))] if ok else 0
print("ASR conc=%3d n=%3d | p50=%5.0fms p95=%5.0fms | %.1f req/s | fails=%d"%(CONC,N,p(.5),p(.95),len(ok)/el,sum(1 for v in lat if v<0)))
