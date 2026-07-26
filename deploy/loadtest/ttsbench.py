import json,time,threading,urllib.request,sys
CONC=int(sys.argv[1]); N=int(sys.argv[2])
TXT="Hi there, thanks so much for taking my call today. How is your day going so far?"
lat=[]; lock=threading.Lock(); sem=threading.Semaphore(CONC)
def one():
    with sem:
        b=json.dumps({"input":TXT,"voice":"af_heart","speed":1.1}).encode()
        r=urllib.request.Request("http://127.0.0.1:8880/tts",data=b,headers={"Content-Type":"application/json"})
        t0=time.time()
        try:
            with urllib.request.urlopen(r,timeout=90) as resp: resp.read()
            with lock: lat.append((time.time()-t0)*1000)
        except Exception as e:
            with lock: lat.append(-1)
ths=[threading.Thread(target=one) for _ in range(N)]
t0=time.time()
for t in ths: t.start()
for t in ths: t.join()
ok=sorted(v for v in lat if v>0)
p=lambda q: ok[min(len(ok)-1,int(len(ok)*q))] if ok else 0
print("TTS conc=%3d n=%3d | p50=%5.0fms p95=%5.0fms max=%5.0fms | %.1f req/s | fails=%d"%(
  CONC,N,p(.5),p(.95),max(ok) if ok else 0,len(ok)/(time.time()-t0),sum(1 for v in lat if v<0)))
