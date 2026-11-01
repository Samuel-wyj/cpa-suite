#!/usr/bin/env bash
# Real-CPA check of 0.7 self-service usage: /usage in cc-switch format,
# shared upstream quota filtered by routing rules, no data leaks, rate limit,
# shared_quota module switch, and the short cc-switch script run in node.
# No upstream is contacted: fake auth files, forecast passive, proxy blackholed.
#   CPA_HOST=/tmp/cpa-v8.0.30 scripts/e2e_selfusage.sh [cpa-suite.so]
set -euo pipefail
cd "$(dirname "$0")/.."
: "${CPA_HOST:?}"
SO=$(realpath "${1:-dist/linux/amd64/cpa-suite.so}")
R=$(mktemp -d); PORT=${PORT:-28521}; B=http://127.0.0.1:$PORT; M="Authorization: Bearer mgmt"; P=$B/v0/management/plugins/cpa-suite
U=$B/v0/resource/plugins/cpa-suite/usage
trap 'kill $(cat $R/pid 2>/dev/null) 2>/dev/null || true; [ -n "${KEEP:-}" ] && echo "kept $R" || rm -rf $R' EXIT
mkdir -p $R/plugins $R/auth
cp "$SO" $R/plugins/cpa-suite.so
echo '{"type":"codex","email":"alice@example.com","access_token":"fake-a","account_id":"acc-a","expired":"2099-01-01T00:00:00Z"}' > $R/auth/codex-alice.json
echo '{"type":"xai","email":"bob@example.com","access_token":"fake-b","expired":"2099-01-01T00:00:00Z"}' > $R/auth/xai-bob.json
cat > $R/config.yaml <<Y
host: "127.0.0.1"
port: $PORT
auth-dir: "$R/auth"
proxy-url: "http://127.0.0.1:9"
api-keys: ["sk-codex-only-0001","sk-norule-0002","sk-grokmodels-0003","sk-deny-codex-0004"]
remote-management: { allow-remote: false, secret-key: "mgmt", disable-control-panel: true }
plugins:
  enabled: true
  dir: "$R/plugins"
  configs:
    cpa-suite: { enabled: true, state_file: "$R/plugins/cpa-suite.db", import_from: none, plugins_dir: "$R/plugins", modules: { forecast: passive } }
Y
start() { (cd $R; "$CPA_HOST" -config $R/config.yaml >> $R/host.log 2>&1 & echo $! > $R/pid)
  for i in $(seq 60); do curl -fs -H "$M" $B/v0/management/plugins > $R/p.json 2>/dev/null && grep -q '"registered":true' $R/p.json && return; sleep 0.5; done; echo "host did not start"; tail -30 $R/host.log; exit 1; }
stop() { kill $(cat $R/pid); while kill -0 $(cat $R/pid) 2>/dev/null; do sleep 0.2; done; }
ok() { echo "ok   $*"; }
usage() { curl -s -H "Authorization: Bearer $1" ${2:+-H "X-Forwarded-For: $2"} "$U"; }
start
curl -fsS -X POST -H "$M" -H "Content-Type: application/json" -d '{"keys":["sk-codex-only-0001","sk-norule-0002","sk-grokmodels-0003","sk-deny-codex-0004"]}' $P/keys/sync >/dev/null
curl -fs -H "$M" $P/keys > $R/keys.json
scope() { python3 -c "import json,sys;v=json.load(open('$R/keys.json'));v=v.get('keys',v);print([k['scope'] for k in v if k.get('preview','').endswith('$1') or '$1' in k.get('preview','')][0])"; }
S1=$(scope 0001); S2=$(scope 0002); S3=$(scope 0003); S4=$(scope 0004)
curl -fsS -X POST -H "$M" -H "Content-Type: application/json" -d "{\"name\":\"标准套餐\",\"windows\":[{\"name\":\"5h\",\"period_seconds\":18000,\"amount_usd\":5}],\"scopes\":[\"$S1\",\"$S2\"]}" $P/plans >/dev/null
bind() { curl -fsS -X PUT -H "$M" -H "Content-Type: application/json" -d "{\"scope\":\"$1\",\"bindings\":$2}" $P/keys/routes >/dev/null; }
bind $S1 '{"route_ids":[],"credential_providers":[{"source":"auth-files","provider":"codex"}]}'
bind $S3 '{"route_ids":[],"models":["grok-4"]}'
bind $S4 '{"route_ids":[],"denied_credential_providers":[{"source":"auth-files","provider":"codex"}]}'
curl -fs -H "$M" $P/auth-files > $R/af.json
read AC AX < <(python3 -c "
import json;v=json.load(open('$R/af.json'));v=v.get('files',v.get('auth_files',v))
m={f['category']:f['auth_index'] for f in v}
print(m['codex'],m['xai'])")
stop
python3 - $R/plugins/cpa-suite.suite-ext.db "$AC" "$AX" <<'PY'
import sqlite3,sys,time; db,ac,ax=sys.argv[1:4]; c=sqlite3.connect(db); n=time.time_ns(); s=int(time.time())
rows=[(ac,'codex','5h',18000,n-120*10**9,82.0,s+3*3600,'api'),(ac,'codex','weekly',604800,n-120*10**9,13.0,s+6*86400,'api'),
      (ax,'xai','weekly',604800,n-300*10**9,40.0,s+3*86400,'api')]
c.executemany("insert into quota_samples(auth_index,provider,window,window_seconds,at,used_percent,reset_at,source) values(?,?,?,?,?,?,?,?)",rows); c.commit()
PY
start
usage sk-norule-0002 > $R/u2.json; usage sk-codex-only-0001 > $R/u1.json; usage sk-grokmodels-0003 > $R/u3.json; usage sk-deny-codex-0004 > $R/u4.json
python3 - $R <<'PY'
import json,sys; r=sys.argv[1]
L=lambda n: json.load(open(f"{r}/u{n}.json"))
names=lambda d:[x["planName"] for x in d["data"]]
u1,u2,u3,u4=L(1),L(2),L(3),L(4)
for d in (u1,u2,u3,u4): assert d["success"] and d["shared_quota"], d
assert any("标准套餐 · 5h 额度" in n for n in names(u2)), names(u2)
up=lambda d:[n for n in names(d) if n.startswith("共享")]
assert any("Codex" in n for n in up(u2)) and any("Grok" in n for n in up(u2)), up(u2); print("ok   no rule: own plan + Codex 5h/weekly + Grok weekly:", up(u2))
assert up(u1) and all("Codex" in n for n in up(u1)), up(u1); print("ok   codex-only key: Grok hidden:", up(u1))
assert up(u3) and all("Grok" in n for n in up(u3)), up(u3); print("ok   grok-models key (no account rule): only Grok:", up(u3))
assert up(u4) and all("Grok" in n for n in up(u4)), up(u4); print("ok   key denying codex accounts: only Grok:", up(u4))
c5=[x for x in u2["data"] if "Codex" in x["planName"] and "5 小时" in x["planName"]][0]
assert c5["remaining"]==18 and c5["unit"]=="%" and "重置" in c5["extra"], c5; print("ok   Codex 5h row: remaining 18 %,", c5["extra"])
raw=open(f"{r}/u2.json").read()+open(f"{r}/u1.json").read()
assert "alice" not in raw and "example.com" not in raw and "fake-" not in raw and "sk-" not in raw; print("ok   no e-mail, token or key in responses")
PY
[ "$(curl -s -o /dev/null -w '%{http_code}' -H 'Authorization: Bearer mgmt' $U)" = 401 ] && ok "management key is not a client key (401)"
[ "$(curl -s -o /dev/null -w '%{http_code}' $U)" = 401 ] && ok "no key -> 401"
codes=""; for i in $(seq 11); do codes="$codes $(curl -s -o /dev/null -w '%{http_code}' -H 'Authorization: Bearer sk-wrong' -H 'X-Forwarded-For: 10.9.9.9' $U)"; done
[[ "$codes" == *" 429" ]] && [[ "$codes" == " 401"* ]] && ok "10 bad keys from one address -> 401, then 429:$codes"
[ "$(curl -s -o /dev/null -w '%{http_code}' -H 'Authorization: Bearer sk-norule-0002' -H 'X-Forwarded-For: 10.9.9.8' $U)" = 200 ] && ok "other address unaffected"
A=$(curl -s -D - -o /dev/null -H 'Authorization: Bearer sk-norule-0002' $U | tr -d '\r' | grep -i '^cache-control'); [[ "$A" == *max-age=30* ]] && ok "cached for 30 s ($A)"
curl -fsS -X PUT -H "$M" -H "Content-Type: application/json" -d '{"id":"shared_quota","mode":"off"}' $P/suite/modules >/dev/null
usage sk-grokmodels-0003 | python3 -c 'import json,sys;d=json.load(sys.stdin);assert not d["shared_quota"] and not [x for x in d["data"] if x["kind"]=="upstream"],d' && ok "shared_quota off: only own plan"
curl -fsS -X PUT -H "$M" -H "Content-Type: application/json" -d '{"id":"shared_quota","mode":"on"}' $P/suite/modules >/dev/null
# node: the exact short script the page copies, run against the live response
cat > $R/run.mjs <<JS
import fs from "node:fs";
const page = fs.readFileSync("internal/plugin/web/account.js", "utf8");
const fn = page.slice(page.indexOf("function ccSwitchUsageScript"), page.indexOf("\n}\n", page.indexOf("function ccSwitchUsageScript")) + 2);
const ACCOUNT_BASE = "/v0/resource/plugins/cpa-suite"; const m = () => "查询失败";
const script = eval("(" + fn + ")")("$B");
const cfg = eval(script.replaceAll("{{apiKey}}", "sk-norule-0002"));
const res = await fetch(cfg.request.url, { method: cfg.request.method, headers: cfg.request.headers });
const out = cfg.extractor(await res.json());
for (const k of ["planName","total","used","remaining","unit","isValid","invalidMessage","extra"]) for (const r of out) if (r[k] != null && typeof r[k] !== ({planName:"string",unit:"string",invalidMessage:"string",extra:"string",isValid:"boolean"}[k] || "number")) throw new Error(k);
if (!Array.isArray(out) || !out.some(r => r.planName.startsWith("共享"))) throw new Error(JSON.stringify(out));
const bad = cfg.extractor({ error: { message: "Invalid API key" } });
if (bad.isValid !== false) throw new Error("bad");
console.log("ok   node: copied cc-switch script ->", out.length, "rows, cc-switch field types valid; error ->", bad.invalidMessage);
console.log(script);
JS
node $R/run.mjs
! grep -qi panic $R/host.log && ok "no panics"
echo "SELFUSAGE E2E OK"
