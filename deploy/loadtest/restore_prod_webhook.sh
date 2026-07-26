#!/bin/bash
# Restore the production Telnyx webhook URL after load testing.
cd /tmp/achatbot-test
set -a; . ./telnyx.env; set +a
ORIG=$(cat telnyx_webhook_ORIGINAL.txt)
[ -z "$ORIG" ] && { echo "ERROR: original URL missing, aborting"; exit 1; }
curl -s -X PATCH "https://api.telnyx.com/v2/call_control_applications/$TELNYX_APP_ID" \
  -H "Authorization: Bearer $TELNYX_API_KEY" -H "Content-Type: application/json" \
  -d "{\"webhook_event_url\":\"$ORIG\"}" | python3 -c "
import sys,json; d=json.load(sys.stdin).get(\"data\",{})
print(\"RESTORED webhook_event_url:\", d.get(\"webhook_event_url\"))"
