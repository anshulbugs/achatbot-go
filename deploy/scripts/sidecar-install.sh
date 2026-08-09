#!/bin/bash
# Python environment for the Daily room sidecar.
#
#   bash deploy/scripts/sidecar-install.sh
#
# One venv, three packages. daily-python is the reason this is Python at all:
# Daily publishes no Go SDK, and a browser call has to have the agent inside the
# room. The same library already runs the photoreal avatar service
# (deploy/avatar/avatar_daily.py), so it is proven on this hardware.
#
# The alternative it replaces was having Telnyx dial the room's SIP endpoint.
# That works, and it sounds like a phone call because it is one — G.711 at
# 8 kHz. This keeps browser audio wideband and drops the carrier leg.
set -euo pipefail

VENV="${SIDECAR_VENV:-$HOME/sidecar-venv}"

log() { printf '\n\033[1;36m==> %s\033[0m\n' "$*"; }
die() { printf '\n\033[1;31mFAILED: %s\033[0m\n' "$*" >&2; exit 1; }

command -v python3 >/dev/null || die "python3 not installed"

if [ ! -x "$VENV/bin/python" ]; then
  log "Creating $VENV"
  python3 -m venv "$VENV" || die "could not create the venv (apt install python3-venv?)"
fi

log "Installing daily-python, websocket-client, numpy"
"$VENV/bin/pip" install -q --upgrade pip
"$VENV/bin/pip" install -q daily-python websocket-client numpy \
  || die "pip install failed"

"$VENV/bin/python" -c "import daily, websocket, numpy" \
  || die "imports failed after install"

log "Up"
printf '  %-16s %s\n' python "$VENV/bin/python"
printf '  %-16s %s\n' script "deploy/sidecar/room_agent.py"
echo
echo "contract-start.sh picks these up automatically. To point elsewhere:"
echo "  SIDECAR_PYTHON=... SIDECAR_SCRIPT=... bash deploy/scripts/contract-start.sh"
