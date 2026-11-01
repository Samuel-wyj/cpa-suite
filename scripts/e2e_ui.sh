#!/usr/bin/env bash
# Start a real CPA host with only the suite build, load its UI in headless Chrome
# and verify every API call the UI makes resolves (regression for 0.1 404s),
# then the 0.3 checks (ui_clients.mjs).
#   CPA_HOST=/path/cli-proxy-api scripts/e2e_ui.sh [cpa-suite.so]
set -euo pipefail
cd "$(dirname "$0")/.."
: "${CPA_HOST:?CPA host binary}"
SO=$(realpath "${1:-dist/linux/amd64/cpa-suite.so}")
[ -d scripts/uitest/node_modules ] || (cd scripts/uitest && npm install --silent)
R=$(mktemp -d); PORT=${PORT:-28431}
trap 'kill $(cat $R/pid 2>/dev/null) $(cat $R/upid 2>/dev/null) 2>/dev/null || true; rm -rf $R' EXIT
python3 scripts/dummy_provider.py --port 0 > $R/upstream.log 2>&1 & echo $! > $R/upid
for i in $(seq 50); do UP=$(sed -n 's|^dummy provider: http://127.0.0.1:||p' $R/upstream.log | head -1); [ -n "$UP" ] && break; sleep 0.1; done
mkdir -p $R/plugins $R/auth && cp "$SO" $R/plugins/cpa-suite.so
cat > $R/config.yaml <<Y
host: "0.0.0.0"
port: $PORT
auth-dir: "$R/auth"
api-keys: ["sk-ui-e2e-key-0001"]
remote-management: { allow-remote: true, secret-key: "mgmt", disable-control-panel: true }
plugins:
  enabled: true
  dir: "$R/plugins"
  configs:
    cpa-suite: { enabled: true, state_file: "$R/plugins/cpa-suite.db", import_from: none, plugins_dir: "$R/plugins" }
openai-compatibility:
  - name: "dummy-ui-e2e"
    base-url: "http://127.0.0.1:$UP/v1"
    api-key-entries: [{ api-key: "dummy" }]
    models: [{ name: "gpt-5.5" }]
Y
(cd $R; "$CPA_HOST" -config $R/config.yaml > $R/host.log 2>&1 & echo $! > $R/pid)
for i in $(seq 60); do
  curl -fs -H "Authorization: Bearer mgmt" http://127.0.0.1:$PORT/v0/management/plugins | grep -q '"registered":true' && break; sleep 0.5
done
curl -fsS -X POST -H "Authorization: Bearer mgmt" -H "Content-Type: application/json" \
  -d '{"keys":["sk-ui-e2e-key-0001"]}' http://127.0.0.1:$PORT/v0/management/plugins/cpa-suite/keys/sync >/dev/null
node scripts/uitest/ui_e2e.mjs "http://127.0.0.1:$PORT" cpa-suite mgmt sk-ui-e2e-key-0001
# Plain-http origin (not localhost, so no WebCrypto): login persistence,
# CPAMP enc::v2 key borrowing, client page and a real 403 through CPA.
LAN=${LAN_IP:-$(ip -4 addr show | grep -oP 'inet \K[\d.]+' | grep -v '^127\.' | head -1)}
node scripts/uitest/ui_clients.mjs "http://$LAN:$PORT" "http://127.0.0.1:$PORT" cpa-suite mgmt sk-ui-e2e-key-0001
# layout: every group/sub-tab, upstream actions, CPAMP shell simulation
node scripts/uitest/ui_layout.mjs "http://127.0.0.1:$PORT" cpa-suite mgmt sk-ui-e2e-key-0001
