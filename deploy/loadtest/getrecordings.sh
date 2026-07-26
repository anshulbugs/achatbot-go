#!/bin/bash
# List + download recent Telnyx call recordings.
cd /tmp/achatbot-test
set -a; . ./telnyx.env; set +a
N="${1:-10}"; OUT="${2:-recordings}"
mkdir -p "$OUT"
curl -s -H "Authorization: Bearer $TELNYX_API_KEY" \
  "https://api.telnyx.com/v2/recordings?page%5Bsize%5D=$N" -o /tmp/recs.json
python3 - "$OUT" <<PY
import json,sys,os,subprocess
out=sys.argv[1]
d=json.load(open("/tmp/recs.json"))
rows=d.get("data",[])
print("recordings found:", len(rows))
for r in rows:
    urls=r.get("download_urls") or {}
    u=urls.get("mp3") or urls.get("wav")
    rid=r.get("id","x")[:8]; dur=r.get("duration_millis",0)/1000.0
    print("  %s  %5.1fs  %s"%(rid, dur, r.get("recording_started_at","")))
    if u:
        p=os.path.join(out, "call_%s.mp3"%rid)
        subprocess.run(["curl","-sL",u,"-o",p])
PY
ls -la "$OUT" | tail -12
