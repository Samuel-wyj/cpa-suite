package plugin

import (
	"cpa-key-billing/internal/suite"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"cpa-key-billing/internal/billing"
)

const suitePlanKey = "sk-suite-import-000001"

func registerWithYAML(t *testing.T, app *App, yaml string) error {
	t.Helper()
	_, err := app.HandleMethod(MethodPluginRegister, mustMarshal(t, LifecycleRequest{ConfigYAML: []byte(yaml)}))
	return err
}

func sha256File(t *testing.T, path string) string {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:])
}

// legacyDatabase builds a real cpa-key-billing database through the upstream
// code paths: synced key, two-window plan, routing rule, custom price and a
// billed request so cycles_json carries spend.
func legacyDatabase(t *testing.T, path string) {
	t.Helper()
	app := newTestApp(t)
	if err := registerWithYAML(t, app, "enabled: true\nstate_file: \""+path+"\"\nimport_from: none\n"); err != nil {
		t.Fatal(err)
	}
	scope := billing.CallerScope(suitePlanKey)
	callOK(t, app, http.MethodPut, routePrices, nil, map[string]any{"model_id": flowModel, "input_per_1m": 2, "output_per_1m": 8}, http.StatusOK, nil)
	callOK(t, app, http.MethodPost, routeKeysSync, nil, map[string]any{"keys": []string{suitePlanKey}}, http.StatusOK, nil)
	callOK(t, app, http.MethodPost, routeKeysLabel, nil, map[string]any{"scope": scope, "label": "王康"}, http.StatusOK, nil)
	callOK(t, app, http.MethodPost, routeKeysConcurrency, nil, map[string]any{"scope": scope, "concurrency_limit": 3}, http.StatusOK, nil)
	callOK(t, app, http.MethodPost, routePlans, nil, map[string]any{
		"id": "team", "name": "Team", "scopes": []string{scope},
		"windows": []billing.QuotaWindow{{Name: "5h", AmountUSD: 10, PeriodSeconds: 18000}, {Name: "week", RequestLimit: 100, PeriodSeconds: 604800}},
	}, http.StatusCreated, nil)
	callOK(t, app, http.MethodPost, routeRoutes, nil, map[string]any{
		"name": "Only gpt", "rule": map[string]any{"models": []string{flowModel}}, "scopes": []string{scope},
	}, http.StatusCreated, nil)
	raw, err := app.HandleMethod(MethodRequestInterceptBefore, mustMarshal(t, RequestInterceptRequest{
		RequestID: "r1", SourceFormat: "openai", Model: flowModel, RequestedModel: flowModel,
		Metadata: map[string]any{MetadataCallerScope: scope},
	}))
	if err != nil {
		t.Fatal(err)
	}
	var admitted RequestInterceptResponse
	decodeResult(t, raw, &admitted)
	if admitted.Terminate {
		t.Fatalf("legacy admission refused: %s", admitted.ResponseBody)
	}
	publishUsageRecord(t, app, UsageRecord{Provider: "openai", Model: flowModel, Alias: flowModel, APIKey: suitePlanKey, Generate: true,
		RequestedAt: app.store.Now(), Detail: UsageDetail{InputTokens: 100000, OutputTokens: 50000, TotalTokens: 150000}})
	app.Shutdown()
}

func TestSuiteImportPreservesLegacyDataReadOnly(t *testing.T) {
	dir := t.TempDir()
	legacy := filepath.Join(dir, "cpa-key-billing-state-v1.db")
	legacyDatabase(t, legacy)
	before := sha256File(t, legacy)
	legacyKeys := dbRows(t, legacy, "SELECT scope, preview, label, plan_id, concurrency_limit, cycles_json, route_bindings_json FROM api_keys ORDER BY scope")
	legacyPlans := dbRows(t, legacy, "SELECT * FROM plans ORDER BY position")
	legacyRoutes := dbRows(t, legacy, "SELECT * FROM routes ORDER BY position")
	legacyPrices := dbRows(t, legacy, "SELECT * FROM prices ORDER BY position")
	legacyEvents := dbRows(t, legacy, "SELECT * FROM request_events ORDER BY id")
	if len(legacyKeys) != 1 || !strings.Contains(legacyKeys[0], "spent_usd") || len(legacyEvents) != 1 {
		t.Fatalf("legacy fixture incomplete: keys=%v events=%d", legacyKeys, len(legacyEvents))
	}

	target := filepath.Join(dir, "cpa-suite.db")
	app := newTestApp(t)
	t.Cleanup(app.Shutdown)
	cfg := "enabled: true\nstate_file: \"" + target + "\"\nimport_from: \"" + legacy + "\"\nplugins_dir: \"" + filepath.Join(dir, "plugins") + "\"\n"
	if err := registerWithYAML(t, app, cfg); err != nil {
		t.Fatal(err)
	}
	if got := sha256File(t, legacy); got != before {
		t.Fatal("legacy database was modified by the import")
	}
	for name, pair := range map[string][2][]string{
		"api_keys": {legacyKeys, dbRows(t, target, "SELECT scope, preview, label, plan_id, concurrency_limit, cycles_json, route_bindings_json FROM api_keys ORDER BY scope")},
		"plans":    {legacyPlans, dbRows(t, target, "SELECT * FROM plans ORDER BY position")},
		"routes":   {legacyRoutes, dbRows(t, target, "SELECT * FROM routes ORDER BY position")},
		"prices":   {legacyPrices, dbRows(t, target, "SELECT * FROM prices ORDER BY position")},
		"events":   {legacyEvents, dbRows(t, target, "SELECT * FROM request_events ORDER BY id")},
	} {
		if strings.Join(pair[0], "\n") != strings.Join(pair[1], "\n") {
			t.Fatalf("%s differs after import:\nlegacy=%v\nsuite=%v", name, pair[0], pair[1])
		}
	}
	if v := dbRows(t, target, "PRAGMA user_version"); len(v) != 1 || v[0] != "[17]" {
		t.Fatalf("schema version = %v", v)
	}
	// The credential fingerprint salt is unchanged, so imported routing refs stay valid.
	if !strings.HasPrefix(billing.CredentialFingerprint("x"), "sha256:") ||
		billing.CredentialFingerprint("auth-1") != "sha256:"+hexSHA("cpa-key-billing:credential:v1\x00auth-1") {
		t.Fatal("credential fingerprint salt changed")
	}
	key := keysByScope(t, app)[billing.CallerScope(suitePlanKey)]
	if key.Label != "王康" || key.PlanID != "team" || key.ConcurrencyLimit != 3 {
		t.Fatalf("imported key view = %+v", key)
	}
	var status struct {
		WatchOnly bool `json:"watch_only"`
		Import    struct {
			SourceUnchanged bool             `json:"source_unchanged"`
			Rows            map[string]int64 `json:"rows"`
		} `json:"import"`
	}
	callOK(t, app, http.MethodGet, routeSuiteStatus, nil, nil, http.StatusOK, &status)
	if status.WatchOnly || !status.Import.SourceUnchanged || status.Import.Rows["api_keys"] != 1 || status.Import.Rows["plans"] != 1 {
		t.Fatalf("status = %+v", status)
	}

	// Reconfigure and restart must never import again or touch the legacy file.
	if err := os.WriteFile(legacy, []byte("not a database any more"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := registerWithYAML(t, app, cfg+"debug: true\n"); err != nil {
		t.Fatalf("reconfigure re-imported: %v", err)
	}
	restarted := newTestApp(t)
	t.Cleanup(restarted.Shutdown)
	if err := registerWithYAML(t, restarted, cfg); err != nil {
		t.Fatalf("restart re-imported: %v", err)
	}
	if keysByScope(t, restarted)[billing.CallerScope(suitePlanKey)].PlanID != "team" {
		t.Fatal("restart lost imported data")
	}
}

func hexSHA(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])
}

func TestSuiteImportFailureCreatesNothing(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "cpa-suite.db")
	app := newTestApp(t)
	t.Cleanup(app.Shutdown)
	err := registerWithYAML(t, app, "enabled: true\nstate_file: \""+target+"\"\nimport_from: \""+filepath.Join(dir, "typo.db")+"\"\n")
	if err == nil || !strings.Contains(err.Error(), "nothing was created") {
		t.Fatalf("err = %v", err)
	}
	if _, errStat := os.Stat(target); !os.IsNotExist(errStat) {
		t.Fatal("a failed import must not leave a database behind")
	}
}

func TestSuiteRefusesSilentFreshStartNextToLegacyDB(t *testing.T) {
	dir := t.TempDir()
	legacyDatabase(t, filepath.Join(dir, "plugins", "cpa-key-billing-state-v1.db"))
	app := newTestApp(t)
	t.Chdir(dir)
	t.Cleanup(app.Shutdown)
	err := registerWithYAML(t, app, "enabled: true\n")
	if err == nil || !strings.Contains(err.Error(), "import_from is not set") {
		t.Fatalf("err = %v", err)
	}
	if _, errStat := os.Stat(filepath.Join(dir, "plugins", "cpa-suite.db")); !os.IsNotExist(errStat) {
		t.Fatal("must not create an empty database next to the legacy one")
	}
	if err := registerWithYAML(t, app, "enabled: true\nimport_from: plugins/cpa-key-billing-state-v1.db\n"); err != nil {
		t.Fatalf("default paths import: %v", err)
	}
	if keysByScope(t, app)[billing.CallerScope(suitePlanKey)].PlanID != "team" {
		t.Fatal("default-path import lost data")
	}
}

// watchOnlyApp returns an app whose only key has an exhausted quota, a
// restricting route and a concurrency limit of 1, plus its plugins dir.
func watchOnlyApp(t *testing.T, extra string) (*App, string, string, string) {
	t.Helper()
	dir := t.TempDir()
	plugins := filepath.Join(dir, "plugins")
	app := newTestApp(t)
	t.Cleanup(app.Shutdown)
	if err := registerWithYAML(t, app, "enabled: true\nstate_file: \""+filepath.Join(dir, "s.db")+"\"\nimport_from: none\nplugins_dir: \""+plugins+"\"\n"+extra); err != nil {
		t.Fatal(err)
	}
	scope := billing.CallerScope(suitePlanKey)
	callOK(t, app, http.MethodPut, routePrices, nil, map[string]any{"model_id": flowModel, "input_per_1m": 2, "output_per_1m": 8}, http.StatusOK, nil)
	callOK(t, app, http.MethodPost, routeKeysSync, nil, map[string]any{"keys": []string{suitePlanKey}}, http.StatusOK, nil)
	callOK(t, app, http.MethodPost, routePlans, nil, map[string]any{
		"id": "tiny", "scopes": []string{scope}, "windows": []billing.QuotaWindow{{Name: "day", RequestLimit: 1, PeriodSeconds: 86400}},
	}, http.StatusCreated, nil)
	if _, err := app.store.CreateRoute(billing.Route{ID: "one-cred", Name: "One cred", Rule: billing.RouteRule{
		CredentialIDs: []string{billing.CredentialFingerprint("auth-a")}}}, []string{scope}); err != nil {
		t.Fatal(err)
	}
	return app, plugins, scope, filepath.Join(dir, "s.db")
}

func interceptOnce(t *testing.T, app *App, scope, id string) RequestInterceptResponse {
	t.Helper()
	raw, err := app.HandleMethod(MethodRequestInterceptBefore, mustMarshal(t, RequestInterceptRequest{
		RequestID: id, SourceFormat: "openai", Model: flowModel, RequestedModel: flowModel,
		Metadata: map[string]any{MetadataCallerScope: scope},
	}))
	if err != nil {
		t.Fatal(err)
	}
	var resp RequestInterceptResponse
	decodeResult(t, raw, &resp)
	return resp
}

func pickOnce(t *testing.T, app *App, scope string) SchedulerPickResponse {
	t.Helper()
	req := SchedulerPickRequest{Model: flowModel, Candidates: []SchedulerAuthCandidate{{ID: "auth-a", Provider: "openai"}, {ID: "auth-b", Provider: "openai"}}}
	req.Options.Metadata = map[string]any{MetadataCallerScope: scope}
	raw, err := app.HandleMethod(MethodSchedulerPick, mustMarshal(t, req))
	if err != nil {
		t.Fatal(err)
	}
	var resp SchedulerPickResponse
	decodeResult(t, raw, &resp)
	return resp
}

func TestSuiteConflictSwitchesToWatchOnly(t *testing.T) {
	app, plugins, scope, state := watchOnlyApp(t, "")
	// Enforcing: the first request is admitted and charged, the second hits the quota.
	if resp := interceptOnce(t, app, scope, "a1"); resp.Terminate {
		t.Fatalf("first request refused: %s", resp.ResponseBody)
	}
	if pick := pickOnce(t, app, scope); !pick.Handled || pick.AuthID != "auth-a" {
		t.Fatalf("enforcing pick = %+v", pick)
	}
	publishUsageRecord(t, app, UsageRecord{Provider: "openai", Model: flowModel, Alias: flowModel, APIKey: suitePlanKey, Generate: true,
		RequestedAt: app.store.Now(), Detail: UsageDetail{InputTokens: 10, OutputTokens: 10, TotalTokens: 20}})
	completeOnce(t, app, "a1")
	if resp := interceptOnce(t, app, scope, "a2"); !resp.Terminate || resp.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("quota not enforced: %+v", resp)
	}
	events := len(requestEventEntries(t, app))

	// A cpa-key-billing library appears in the CPA discovery directory (store-installed name).
	touchFile(t, filepath.Join(plugins, runtime.GOOS, runtime.GOARCH, "cpa-key-billing-v1.3.20.so"))
	if err := registerWithYAML(t, app, "enabled: true\nstate_file: \""+state+"\"\nplugins_dir: \""+plugins+"\"\n"); err != nil {
		t.Fatal(err)
	}
	if resp := interceptOnce(t, app, scope, "w1"); resp.Terminate {
		t.Fatalf("watch-only must not block: %+v", resp)
	}
	if pick := pickOnce(t, app, scope); pick.Handled {
		t.Fatalf("watch-only must not schedule: %+v", pick)
	}
	publishUsageRecord(t, app, UsageRecord{Provider: "openai", Model: flowModel, Alias: flowModel, APIKey: suitePlanKey, Generate: true,
		RequestedAt: app.store.Now(), Detail: UsageDetail{InputTokens: 10, OutputTokens: 10, TotalTokens: 20}})
	if got := len(requestEventEntries(t, app)); got != events {
		t.Fatalf("watch-only must not charge or record usage: events %d -> %d", events, got)
	}
	completeOnce(t, app, "w1")
	page := callManagementResource(t, app)
	if !strings.Contains(page, "CPA Suite 只观察模式") || !strings.Contains(page, "cpa-key-billing") {
		t.Fatal("UI must show the red watch-only banner")
	}
	var status struct {
		WatchOnly bool `json:"watch_only"`
		Conflicts []struct {
			PluginID string `json:"plugin_id"`
		} `json:"conflicts"`
	}
	callOK(t, app, http.MethodGet, routeSuiteStatus, nil, nil, http.StatusOK, &status)
	if !status.WatchOnly || len(status.Conflicts) != 1 || status.Conflicts[0].PluginID != "cpa-key-billing" {
		t.Fatalf("status = %+v", status)
	}
}

func TestSuiteClientGuardCoexists(t *testing.T) {
	app, plugins, scope, state := watchOnlyApp(t, "")
	touchFile(t, filepath.Join(plugins, runtime.GOOS, runtime.GOARCH, "client-guard.so"))
	if err := registerWithYAML(t, app, "enabled: true\nstate_file: \""+state+"\"\nplugins_dir: \""+plugins+"\"\n"); err != nil {
		t.Fatal(err)
	}
	if pick := pickOnce(t, app, scope); !pick.Handled {
		t.Fatal("client-guard must not trigger watch-only")
	}
	page := callManagementResource(t, app)
	if strings.Contains(page, "CPA Suite 只观察模式") || !strings.Contains(page, "client-guard.so 仍在加载") {
		t.Fatal("banner must explain the client module is paused while client-guard runs")
	}
}

func TestSuiteModuleSwitches(t *testing.T) {
	app, _, scope, state := watchOnlyApp(t, "modules:\n  core: observe\n")
	if pick := pickOnce(t, app, scope); pick.Handled {
		t.Fatal("core: observe must not schedule")
	}
	var modules struct {
		Modules []struct {
			ID, Mode string
		} `json:"modules"`
	}
	callOK(t, app, http.MethodGet, routeSuiteModules, nil, nil, http.StatusOK, &modules)
	if len(modules.Modules) != 9 || modules.Modules[0].ID != "core" || modules.Modules[0].Mode != "observe" {
		t.Fatalf("modules = %+v", modules)
	}
	// Runtime switches: config.yaml locks core; candy_test needs confirm; others switch live.
	callOK(t, app, http.MethodPut, routeSuiteModules, nil, map[string]any{"id": "core", "mode": "enforce"}, http.StatusConflict, nil)
	callOK(t, app, http.MethodPut, routeSuiteModules, nil, map[string]any{"id": "candy_test", "mode": "on"}, http.StatusBadRequest, nil)
	callOK(t, app, http.MethodPut, routeSuiteModules, nil, map[string]any{"id": "candy_test", "mode": "on", "confirm": true}, http.StatusOK, nil)
	callOK(t, app, http.MethodPut, routeSuiteModules, nil, map[string]any{"id": "ranking", "mode": "off"}, http.StatusOK, nil)
	callOK(t, app, http.MethodGet, routeSuiteModules, nil, nil, http.StatusOK, &modules)
	got := map[string]string{}
	for _, m := range modules.Modules {
		got[m.ID] = m.Mode
	}
	if got["candy_test"] != "on" || got["ranking"] != "off" || got["core"] != "observe" {
		t.Fatalf("after switches %+v", got)
	}
	for _, bad := range []string{"modules:\n  forecast: enforce\n", "modules:\n  gemini_quota: off\n", "modules:\n  typo: on\n"} {
		if err := registerWithYAML(t, app, "enabled: true\nstate_file: \""+state+"\"\n"+bad); err == nil {
			t.Fatalf("%q must be rejected", bad)
		}
	}
}

func TestSuiteRegistrationIdentity(t *testing.T) {
	reg := registration()
	if reg.Metadata.Name != "cpa-suite" || reg.Metadata.Version != suite.Version || suite.Version != "1.0.0" || MenuLabel != "CPA Suite 控制台" {
		t.Fatalf("identity = %+v %s", reg.Metadata, MenuLabel)
	}
	if !strings.HasSuffix(managementBase, "/cpa-suite") {
		t.Fatal(managementBase)
	}
}

func completeOnce(t *testing.T, app *App, id string) {
	t.Helper()
	if _, err := app.HandleMethod(MethodRequestComplete, mustMarshal(t, RequestCompletion{RequestID: id, Outcome: "succeeded", StatusCode: 200})); err != nil {
		t.Fatal(err)
	}
}

func touchFile(t *testing.T, path string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("elf"), 0o644); err != nil {
		t.Fatal(err)
	}
}

func callManagementResource(t *testing.T, app *App) string {
	t.Helper()
	raw, err := app.HandleMethod(MethodManagementHandle, mustMarshal(t, ManagementRequest{Method: http.MethodGet, Path: resourceBase + resourceUIPath}))
	if err != nil {
		t.Fatal(err)
	}
	var resp ManagementResponse
	decodeResult(t, raw, &resp)
	return string(resp.Body)
}

// TestImportRealUpstreamDatabase imports a database written by the pristine
// upstream build (produced by scripts/parity.sh) and checks every row survives.
func TestImportRealUpstreamDatabase(t *testing.T) {
	source := os.Getenv("SUITE_UPSTREAM_DB")
	if source == "" {
		t.Skip("set SUITE_UPSTREAM_DB (scripts/parity.sh)")
	}
	before := sha256File(t, source)
	target := filepath.Join(t.TempDir(), "cpa-suite.db")
	app := newTestApp(t)
	t.Cleanup(app.Shutdown)
	if err := registerWithYAML(t, app, "enabled: true\nstate_file: \""+target+"\"\nimport_from: \""+source+"\"\n"); err != nil {
		t.Fatal(err)
	}
	if sha256File(t, source) != before {
		t.Fatal("upstream database modified")
	}
	for _, table := range []string{"api_keys", "plans", "routes", "prices", "config_credentials", "request_events", "request_errors", "plugin_logs"} {
		q := "SELECT * FROM " + table + " ORDER BY rowid"
		a, b := dbRows(t, source, q), dbRows(t, target, q)
		if table == "plugin_logs" && len(b) >= len(a) {
			b = b[:len(a)] // the suite appends its own startup log lines
		}
		if strings.Join(a, "\n") != strings.Join(b, "\n") {
			t.Fatalf("%s differs after import (%d vs %d rows)", table, len(a), len(b))
		}
	}
	if len(readKeys(t, app)) != 3 {
		t.Fatal("imported upstream keys not visible")
	}
}

// A config upstream rejects must not import, create a database or an ext file.
func TestSuiteRejectedConfigLeavesNoFiles(t *testing.T) {
	dir := t.TempDir()
	app := newTestApp(t)
	t.Cleanup(app.Shutdown)
	t.Chdir(dir)
	if err := registerWithYAML(t, app, "enabled: [not, a, boolean]\n"); err == nil {
		t.Fatal("malformed config accepted")
	}
	if entries, _ := os.ReadDir(dir); len(entries) != 0 {
		t.Fatalf("rejected config created files: %v", entries)
	}
}
