"""Pre-render a looping idle video from one avatar frame: breathing, sway, and real blinks."""
import math, numpy as np, cv2

L_EYE=[33,160,158,133,153,144]; R_EYE=[362,385,387,263,373,380]

def _eye_boxes(rgb):
    try:
        import mediapipe as mp
        with mp.solutions.face_mesh.FaceMesh(static_image_mode=True, max_num_faces=1,
                                             refine_landmarks=True, min_detection_confidence=.4) as fm:
            res=fm.process(rgb)
        if not res.multi_face_landmarks: return []
        h,w=rgb.shape[:2]; lm=res.multi_face_landmarks[0].landmark; out=[]
        for idx in (L_EYE,R_EYE):
            xs=[lm[i].x*w for i in idx]; ys=[lm[i].y*h for i in idx]
            x0,x1=min(xs),max(xs); y0,y1=min(ys),max(ys)
            mx=(x1-x0)*.35; my=(y1-y0)*1.15
            X0=max(0,int(x0-mx)); X1=min(w,int(x1+mx))
            Y0=max(0,int(y0-my)); Y1=min(h,int(y1+my))
            if X1-X0>6 and Y1-Y0>5: out.append((X0,Y0,X1-X0,Y1-Y0))
        return out
    except Exception as e:
        print("[idle] landmark detect failed:", e, flush=True); return []

def _blink(frame, boxes, c):
    """Draw the upper lid down over each eye box; c in (0,1], 1 = fully closed."""
    if c<=0.02: return frame
    out=frame
    for (x,y,w,h) in boxes:
        eye=out[y:y+h, x:x+w]
        lid_h=max(2,int(h*0.30)); lid=eye[0:lid_h]
        cover=max(1,int(h*c))
        drop=cv2.resize(lid,(w,cover),interpolation=cv2.INTER_LINEAR)
        a=np.ones((cover,1),np.float32)
        f=max(1,int(cover*0.35)); a[cover-f:,0]=np.linspace(1,0,f)
        a=np.repeat(a[:,:,None],3,axis=2)
        eye[0:cover]=(drop.astype(np.float32)*a+eye[0:cover].astype(np.float32)*(1-a)).astype(np.uint8)
    return out

def make_idle(base_rgb, fps=25, seconds=8.0, seed=11):
    n=int(fps*seconds); h,w=base_rgb.shape[:2]; rng=np.random.default_rng(seed)
    boxes=_eye_boxes(base_rgb)
    print("[idle] eye boxes:", boxes, flush=True)
    # schedule blinks every 2.2-4.5s
    blinks=[]; t=rng.uniform(0.8,1.6)
    while t<seconds-0.4: blinks.append(int(t*fps)); t+=rng.uniform(2.2,4.5)
    prof=[0.35,0.8,1.0,0.75,0.3]
    frames=[]
    for i in range(n):
        p=i/n; ang=0.40*math.sin(2*math.pi*p); sc=1.006+0.005*math.sin(2*math.pi*p*1.3)
        dx=2.6*math.sin(2*math.pi*p+0.7); dy=1.7*math.sin(4*math.pi*p)+1.1*math.sin(2*math.pi*p*0.85)
        M=cv2.getRotationMatrix2D((w/2,h/2),ang,sc); M[0,2]+=dx; M[1,2]+=dy
        f=cv2.warpAffine(base_rgb,M,(w,h),flags=cv2.INTER_LINEAR,borderMode=cv2.BORDER_REPLICATE)
        for b in blinks:
            if b<=i<b+len(prof): f=_blink(f,boxes,prof[i-b]); break
        frames.append(f)
    d=[float(np.abs(frames[i].astype(np.int16)-frames[i-1].astype(np.int16)).mean()) for i in range(1,len(frames))]
    print("[idle] generated %d frames, %d blinks, mean delta %.2f max %.2f"%(n,len(blinks),sum(d)/len(d),max(d)), flush=True)
    return frames
