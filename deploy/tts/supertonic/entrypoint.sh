#!/bin/sh
# The onnxruntime-gpu CUDA provider loads its shared libraries from the nvidia-*
# wheels, whose lib directories are not on the default loader path and whose
# exact location varies with the Python minor version. Resolving them at start
# rather than hardcoding a path means a base-image bump does not silently drop
# us back to CPU (which costs ~40x -- see server.py).
set -e

NVIDIA_LIBS=$(python3 -c "
import site, os
roots = site.getsitepackages() + [site.getusersitepackages()]
out = set()
for r in roots:
    n = os.path.join(r, 'nvidia')
    if not os.path.isdir(n):
        continue
    for dirpath, _, files in os.walk(n):
        if any(f.startswith('lib') and '.so' in f for f in files):
            out.add(dirpath)
print(':'.join(sorted(out)))
")

if [ -n "$NVIDIA_LIBS" ]; then
    export LD_LIBRARY_PATH="$NVIDIA_LIBS${LD_LIBRARY_PATH:+:$LD_LIBRARY_PATH}"
fi

exec "$@"
