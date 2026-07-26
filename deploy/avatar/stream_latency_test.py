import sys, time, numpy as np, librosa, torch, statistics
from collections import deque
from flash_head.inference import get_pipeline, get_base_data, get_infer_params, get_audio_embedding, run_pipeline

ckpt="models/SoulX-FlashHead-1_3B"; wav2vec="models/wav2vec2-base-960h"
model_type=sys.argv[1] if len(sys.argv)>1 else "lite"
cond=sys.argv[2] if len(sys.argv)>2 else "portrait_hq.png"
audio=sys.argv[3] if len(sys.argv)>3 else "taj_16k.wav"

t0=time.time()
pipeline=get_pipeline(world_size=1, ckpt_dir=ckpt, model_type=model_type, wav2vec_dir=wav2vec)
print("[load] model load (one-time): %.2fs"%(time.time()-t0), flush=True)
t=time.time()
get_base_data(pipeline, cond_image_path_or_dir=cond, base_seed=9999, use_face_crop=True)
print("[prep] avatar prep (once per photo): %.2fs"%(time.time()-t), flush=True)
p=get_infer_params(); sr=p["sample_rate"]; fps=p["tgt_fps"]; cad=p["cached_audio_duration"]; fn=p["frame_num"]; mfn=p["motion_frames_num"]
sl=fn-mfn; cvd=sl/fps
print("[params] %s: slice_len=%df chunk_video=%.3fs cached_audio=%ds"%(model_type,sl,cvd,cad), flush=True)
a,_=librosa.load(audio, sr=sr, mono=True); step=sl*sr//fps
rem=len(a)%step
if rem>0: a=np.concatenate([a,np.zeros(step-rem,dtype=a.dtype)])
sls=a.reshape(-1,step); cal=sr*cad; aei=cad*fps; asi=aei-fn; dq=deque([0.0]*cal,maxlen=cal); costs=[]; t_stream=time.time()
for i,sp in enumerate(sls):
    dq.extend(sp.tolist()); arr=np.array(dq)
    emb=get_audio_embedding(pipeline, arr, asi, aei); torch.cuda.synchronize(); ts=time.time()
    v=run_pipeline(pipeline, emb); v=v[mfn:]; torch.cuda.synchronize(); c=time.time()-ts; costs.append(c)
    if i==0: print("[first-frame] startup lag to first video: %.2fs"%(time.time()-t_stream), flush=True)
    rt = "REALTIME" if c<cvd else "LAGS"
    print("[chunk %d] gen=%.3fs per %.3fs video -> %s (%.2fx)"%(i,c,cvd,rt,cvd/c), flush=True)
avg=statistics.mean(costs[1:]) if len(costs)>1 else costs[0]
print("[SUMMARY] %s: avg gen=%.3fs / %.3fs video => %.2fx realtime; ~%d concurrent streams/GPU"%(model_type,avg,cvd,cvd/avg,int(cvd/avg)), flush=True)
