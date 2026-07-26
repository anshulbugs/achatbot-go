import asyncio, json, time, threading, queue
import numpy as np, torch, requests, daily
from collections import deque
from aiohttp import web, WSMsgType
from flash_head.inference import get_pipeline, get_base_data, get_infer_params, get_audio_embedding, run_pipeline

CKPT="models/SoulX-FlashHead-1_3B"; WAV2VEC="models/wav2vec2-base-960h"; MODEL="lite"
AVATAR="avatar_current.jpg"; PORT=8899
KEY=[l.split("=",1)[1].strip() for l in open("daily.env") if l.startswith("DAILY_API_KEY")][0]
HDR={"Authorization":"Bearer "+KEY,"Content-Type":"application/json"}

print("[daily] loading SoulX...", flush=True)
pl=get_pipeline(world_size=1, ckpt_dir=CKPT, model_type=MODEL, wav2vec_dir=WAV2VEC)
P=get_infer_params(); SR=P["sample_rate"]; FPS=P["tgt_fps"]; CAD=P["cached_audio_duration"]; FN=P["frame_num"]; MFN=P["motion_frames_num"]
SLICE=FN-MFN; STEP=SLICE*SR//FPS; CAL=SR*CAD; AEI=CAD*FPS; ASI=AEI-FN; SUB=SR//FPS
get_base_data(pl, cond_image_path_or_dir=AVATAR, base_seed=9999, use_face_crop=True)
_dq=deque([0.0]*CAL, maxlen=CAL); _emb=get_audio_embedding(pl, np.array(_dq), ASI, AEI); _v=run_pipeline(pl,_emb); _v=_v[MFN:]
IDLE=np.ascontiguousarray(_v.cpu().numpy().astype(np.uint8)[0]); torch.cuda.synchronize()
print("[daily] SoulX READY; STEP=%d SUB=%d"%(STEP,SUB), flush=True)

lock=threading.Lock()
def gen_chunk(w):
    with lock:
        emb=get_audio_embedding(pl, w, ASI, AEI); v=run_pipeline(pl, emb); v=v[MFN:]
        return np.ascontiguousarray(v.cpu().numpy().astype(np.uint8))

# idle loop: prefer a supplied idle video (idle_loop.mp4); else synthesize one
import os as _os, cv2 as _cv2
from idle_gen import make_idle
IDLE_SEQ=None
for _p in ("idle_loop.mp4","sample_results/idle_loop.mp4"):
    if _os.path.exists(_p):
        try:
            import imageio.v2 as _iio
            _fr=[]
            for _f in _iio.get_reader(_p):
                _a=np.asarray(_f)[:,:,:3]
                if _a.shape[0]!=512 or _a.shape[1]!=512: _a=_cv2.resize(_a,(512,512),interpolation=_cv2.INTER_AREA)
                _fr.append(np.ascontiguousarray(_a.astype(np.uint8)))
            if len(_fr)>2:
                IDLE_SEQ=_fr; print("[daily] idle video loaded: %s (%d frames)"%(_p,len(_fr)), flush=True)
        except Exception as _e: print("[daily] idle video load failed:", _e, flush=True)
        break
if IDLE_SEQ is None:
    IDLE_SEQ = make_idle(IDLE, fps=FPS, seconds=8.0)
IDLE_FRAMES = [f.tobytes() for f in IDLE_SEQ]
try:
    import imageio, os as _os
    _os.makedirs("sample_results", exist_ok=True)
    _w=imageio.get_writer("sample_results/idle_preview.mp4", format="mp4", mode="I", fps=FPS, codec="libx264", pixelformat="yuv420p")
    for _f in IDLE_SEQ: _w.append_data(_f)
    _w.close(); print("[daily] idle preview written", flush=True)
except Exception as _e: print("[daily] idle preview failed:", _e, flush=True)
daily.Daily.init()
cam=daily.Daily.create_camera_device("cam", width=512, height=512, color_format="RGB")
mic=daily.Daily.create_microphone_device("mic", sample_rate=SR, channels=1)
client=daily.CallClient()
client.update_inputs({"camera":{"isEnabled":True,"settings":{"deviceId":"cam"}},"microphone":{"isEnabled":True,"settings":{"deviceId":"mic"}}})
ROOM={"url":None,"name":None}
r=requests.post("https://api.daily.co/v1/rooms",headers=HDR,json={"properties":{"exp":int(time.time())+21600,"enable_prejoin_ui":False}}).json()
bot_tok=requests.post("https://api.daily.co/v1/meeting-tokens",headers=HDR,json={"properties":{"room_name":r["name"],"is_owner":True,"user_name":"avatar"}}).json()["token"]
client.join(r["url"], bot_tok); time.sleep(2); ROOM["url"]=r["url"]; ROOM["name"]=r["name"]
print("[daily] bot joined room", r["url"], flush=True)

pair_q=queue.Queue(maxsize=400); audio_in=queue.Queue()
def _emit(frames, sl):
    ai=(np.clip(sl,-1,1)*32767).astype("<i2")
    for i in range(frames.shape[0]):
        pair_q.put((frames[i].tobytes(), ai[i*SUB:(i+1)*SUB].tobytes()))
def gen_worker():
    dq=deque([0.0]*CAL, maxlen=CAL); buf=np.zeros(0,dtype=np.float32)
    while True:
        kind,payload=audio_in.get()
        if kind=="flush":
            print("[t] flush: dropping %d buffered samples + queue"%len(buf), flush=True)
            buf=np.zeros(0,dtype=np.float32); dq=deque([0.0]*CAL, maxlen=CAL)
            try:
                while True: pair_q.get_nowait()
            except queue.Empty: pass
            continue
        if kind=="eot":
            print("[t] eot: tail %d samples (%.0fms)"%(len(buf), 1000.0*len(buf)/SR), flush=True)
            if len(buf)>0:
                sl=np.concatenate([buf, np.zeros(STEP-len(buf), dtype=np.float32)])
                dq.extend(sl.tolist()); _emit(gen_chunk(np.array(dq)), sl)
                buf=np.zeros(0,dtype=np.float32)
            continue
        buf=np.concatenate([buf,payload])
        while len(buf)>=STEP:
            sl=buf[:STEP]; buf=buf[STEP:]; dq.extend(sl.tolist())
            _emit(gen_chunk(np.array(dq)), sl)
def writer_loop():
    sil=np.zeros(SUB,dtype="<i2").tobytes(); per=1.0/FPS; nxt=time.time(); ii=0; nidle=len(IDLE_FRAMES)
    while True:
        try: fb,ab=pair_q.get_nowait()
        except queue.Empty: fb=IDLE_FRAMES[ii%nidle]; ab=sil; ii+=1
        cam.write_frame(fb); mic.write_frames(ab)
        nxt+=per; d=nxt-time.time()
        if d>0: time.sleep(d)
        else: nxt=time.time()
threading.Thread(target=gen_worker,daemon=True).start()
threading.Thread(target=writer_loop,daemon=True).start()

def ctoken(): return requests.post("https://api.daily.co/v1/meeting-tokens",headers=HDR,json={"properties":{"room_name":ROOM["name"],"user_name":"viewer"}}).json()["token"]
async def session(req): return web.json_response({"roomUrl":ROOM["url"],"token":ctoken()}, headers={"Access-Control-Allow-Origin":"*"})
async def opts(req): return web.Response(headers={"Access-Control-Allow-Origin":"*","Access-Control-Allow-Methods":"POST,OPTIONS","Access-Control-Allow-Headers":"*"})
async def ws_handler(req):
    ws=web.WebSocketResponse(max_msg_size=0); await ws.prepare(req); print("[daily] ws connected", flush=True)
    await ws.send_str(json.dumps({"type":"room","url":ROOM["url"],"token":ctoken()}))
    async for m in ws:
        if m.type==WSMsgType.BINARY: audio_in.put(("pcm", np.frombuffer(m.data,dtype=np.int16).astype(np.float32)/32768.0))
        elif m.type==WSMsgType.TEXT:
            t=json.loads(m.data).get("type")
            if t=="flush": audio_in.put(("flush",None))
            elif t=="eot": audio_in.put(("eot",None))
    return ws
app=web.Application(); app.router.add_post("/session",session); app.router.add_options("/session",opts); app.router.add_get("/",ws_handler); app.router.add_get("/ws",ws_handler)
print("[daily] serving on :%d"%PORT, flush=True); web.run_app(app, host="0.0.0.0", port=PORT)
