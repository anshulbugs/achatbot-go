#!/bin/bash
# Supervisor: keep the avatar service alive. It exits non-zero when its Daily
# transport dies, because a fresh process is the only reliable way to rebuild it.
cd /workspace/SoulX-FlashHead
while true; do
  CC=cc python avatar_daily.py >> avatar_daily.log 2>&1
  echo "[sup] avatar_daily exited ($?), restarting in 5s" >> avatar_daily.log
  sleep 5
done
