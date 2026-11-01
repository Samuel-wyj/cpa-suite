#!/usr/bin/env bash
# Real-CPA migration rehearsal: old plugin -> conflict (watch-only) -> migrate -> rollback.
# CPA_HOST=/path/cli-proxy-api OLD_SO=cpa-key-billing-v1.3.20.so GUARD_SO=client-guard.so scripts/e2e_migration.sh
set -euo pipefail
: "${CPA_HOST:?CPA host binary}" "${OLD_SO:?cpa-key-billing .so}" "${GUARD_SO:?client-guard .so}" "${SUITE_SO:=dist/linux/amd64/cpa-suite.so}"
SUITE_SO=$(realpath "$SUITE_SO")
R=$(mktemp -d)/rt; rm -rf $R; mkdir -p $R/plugins/linux/amd64 $R/auth
PORT=28411
cfg() { # $1 = extra plugin config
cat > $R/config.yaml <<Y
host: "127.0.0.1"
port: $PORT
auth-dir: "$R/auth"
api-keys:
  - "sk-mig-key-one-0001"
  - "sk-mig-key-two-0002"
logging-to-file: false
remote-management:
  allow-remote: false
  secret-key: "mgmt"
  disable-control-panel: true
plugins:
  enabled: true
  dir: "plugins"
  configs:
    cpa-key-billing:
      enabled: true
    client-guard:
      enabled: true
$1
Y
}
start() { (cd $R; $CPA_HOST -config $R/config.yaml > $R/host.$1.log 2>&1 & echo $! > $R/pid); for i in $(seq 60); do if [ "$(curl -fs -H "Authorization: Bearer mgmt" http://127.0.0.1:$PORT/v0/management/plugins | jq "[.plugins[]|select(.registered)]|length" 2>/dev/null)" = "$(ls $R/plugins/linux/amd64 | wc -l)" ]; then sleep 1; return; fi; sleep 0.5; done; echo "host failed"; tail -30 $R/host.$1.log; exit 1; }
stop() { kill $(cat $R/pid); while kill -0 $(cat $R/pid) 2>/dev/null; do sleep 0.2; done; }
m() { local meth=$1 path=$2; shift 2; curl -sS -X $meth -H 'Authorization: Bearer mgmt' -H 'Content-Type: application/json' "http://127.0.0.1:$PORT/v0/management$path" "$@"; }
plugins() { m GET /plugins | jq -c '[.plugins[] | {id, registered, effective_enabled, error: (.error // .last_error // null)}]'; }

echo "### phase 1: today's server — cpa-key-billing + client-guard"
cp $OLD_SO $R/plugins/linux/amd64/
cp $GUARD_SO $R/plugins/linux/amd64/
cfg ""
start 1; plugins
m POST /plugins/cpa-key-billing/keys/sync -d '{"keys":["sk-mig-key-one-0001","sk-mig-key-two-0002"]}'; echo
S1=$(m GET /plugins/cpa-key-billing/keys | jq -r '.keys[] | select(.preview|endswith("0001")) | .scope')
m POST /plugins/cpa-key-billing/keys/label -d "{\"scope\":\"$S1\",\"label\":\"王康\"}" >/dev/null
m POST /plugins/cpa-key-billing/plans -d "{\"id\":\"p1\",\"name\":\"P1\",\"scopes\":[\"$S1\"],\"windows\":[{\"name\":\"day\",\"amount_usd\":3,\"period_seconds\":86400}]}" | jq -c '.plan.id'
m GET /plugins/cpa-key-billing/keys | jq -S '[.keys[] | {preview,label,plan_id}]' > $R/keys.before.json
m GET /plugins/cpa-key-billing/plans | jq -S '[.plans[] | {id,name,scopes,windows: [.windows[] | {name,amount_usd,period_seconds}]}]' > $R/plans.before.json
stop
OLD=$R/plugins/cpa-key-billing-state-v1.db; ls -la $OLD

echo "### phase 2: suite added while cpa-key-billing still installed => watch-only"
cp $SUITE_SO $R/plugins/linux/amd64/
cfg "    cpa-suite:
      enabled: true
      state_file: \"plugins/cpa-suite.db\"
      import_from: \"plugins/cpa-key-billing-state-v1.db\""
start 2; plugins
m GET /plugins/cpa-suite/suite/status | jq -c '{watch_only, conflicts, coexisting, import: (.import|{source,schema_version,source_unchanged,rows})}'
curl -s http://127.0.0.1:$PORT/v0/resource/plugins/cpa-suite/ui -o $R/ui2.html -w 'ui http %{http_code} ' ; grep -o 'suite-banner[^>]*>[^<]*<[^>]*>[^<]*' $R/ui2.html | head -2; echo
stop
H0=$(sha256sum $OLD | cut -d' ' -f1)

echo "### phase 3: migration — cpa-key-billing.so moved out, client-guard kept"
mkdir -p $R/plugin-backups; mv $R/plugins/linux/amd64/cpa-key-billing-v1.3.20.so $R/plugin-backups/
start 3; plugins
m GET /plugins/cpa-suite/suite/status | jq -c '{watch_only, conflicts, coexisting}'
m GET /plugins/cpa-suite/keys | jq -S '[.keys[] | {preview,label,plan_id}]' > $R/keys.after.json
m GET /plugins/cpa-suite/plans | jq -S '[.plans[] | {id,name,scopes,windows: [.windows[] | {name,amount_usd,period_seconds}]}]' > $R/plans.after.json
diff $R/keys.before.json $R/keys.after.json && echo "keys identical: $(jq length $R/keys.after.json)"
diff $R/plans.before.json $R/plans.after.json && echo "plans identical: $(jq length $R/plans.after.json)"
[ "$(sha256sum $OLD | cut -d' ' -f1)" = "$H0" ] && echo "old DB unchanged (sha256 $H0)"
stop

echo "### phase 4: rollback — suite out, cpa-key-billing back"
mv $R/plugins/linux/amd64/cpa-suite.so $R/plugin-backups/; mv $R/plugin-backups/cpa-key-billing-v1.3.20.so $R/plugins/linux/amd64/
start 4; plugins
m GET /plugins/cpa-key-billing/keys | jq -S '[.keys[] | {preview,label,plan_id}]' | diff $R/keys.before.json - && echo "rollback: old plugin sees original data"
stop
