package plugin

import (
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"cpa-key-billing/internal/billing"
)

const codexUA = "codex_cli_rs/0.40.0 (Mac OS 15.0; arm64) iTerm.app"

func clientIntercept(t *testing.T, app *App, scope, id, ua string) RequestInterceptResponse {
	t.Helper()
	req := map[string]any{
		"RequestID": id, "SourceFormat": "openai", "Model": flowModel, "RequestedModel": flowModel,
		"Headers":  http.Header{"User-Agent": []string{ua}},
		"Metadata": map[string]any{MetadataCallerScope: scope, "request_path": "/v1/chat/completions"},
	}
	raw, err := app.HandleMethod(MethodRequestInterceptBefore, mustMarshal(t, req))
	if err != nil {
		t.Fatal(err)
	}
	var resp RequestInterceptResponse
	decodeResult(t, raw, &resp)
	return resp
}

type clientStateView struct {
	Module struct {
		Mode   string `json:"mode"`
		Active bool   `json:"active"`
		Paused bool   `json:"paused"`
		Reason string `json:"reason"`
		Error  string `json:"error"`
		Import *struct {
			KeyRules        int    `json:"key_rules"`
			FoundStats      bool   `json:"found_stats"`
			SourceUnchanged bool   `json:"source_unchanged"`
			ImportedAt      string `json:"imported_at"`
		} `json:"import"`
	} `json:"module"`
	State *struct {
		Rules struct {
			Default struct {
				Mode  string   `json:"mode"`
				Allow []string `json:"allow"`
			} `json:"default"`
			Keys map[string]struct {
				Mode  string   `json:"mode"`
				Allow []string `json:"allow"`
			} `json:"keys"`
		} `json:"rules"`
		Recent []struct {
			Scope  string `json:"scope"`
			Result string `json:"result"`
		} `json:"recent"`
	} `json:"state"`
}

func clientState(t *testing.T, app *App) clientStateView {
	t.Helper()
	var v clientStateView
	callOK(t, app, http.MethodGet, routeSuiteClients, nil, nil, http.StatusOK, &v)
	return v
}

// TestSuiteClientBlocksBeforeAdmission: a 403 from the client module takes no
// slot and no quota and records no usage; an allowed client is then admitted.
func TestSuiteClientBlocksBeforeAdmission(t *testing.T) {
	app, _, scope, _ := watchOnlyApp(t, "")
	callOK(t, app, http.MethodPost, routeKeysConcurrency, nil, map[string]any{"scope": scope, "concurrency_limit": 1}, http.StatusOK, nil)
	callOK(t, app, http.MethodPut, routeSuiteClientRules, nil, map[string]any{
		"default": map[string]any{"mode": "off", "allow": []string{}},
		"keys":    map[string]any{scope: map[string]any{"mode": "enforce", "allow": []string{"re:^codex[_-]"}}},
	}, http.StatusOK, nil)
	events := len(requestEventEntries(t, app))
	for i := 0; i < 3; i++ {
		resp := clientIntercept(t, app, scope, "blocked", "curl/8.4.0")
		if !resp.Terminate || resp.StatusCode != http.StatusForbidden || !strings.Contains(string(resp.ResponseBody), "client_not_allowed") {
			t.Fatalf("blocked = %+v %s", resp, resp.ResponseBody)
		}
		completeOnce(t, app, "blocked") // CPA still fires complete; must be harmless
	}
	if got := keysByScope(t, app)[scope].CurrentConcurrency; got != 0 {
		t.Fatalf("blocked request took a slot: %d", got)
	}
	if got := len(requestEventEntries(t, app)); got != events {
		t.Fatalf("blocked request charged/recorded: %d -> %d", events, got)
	}
	// The plan allows exactly one request: it must still be available.
	if resp := clientIntercept(t, app, scope, "ok1", codexUA); resp.Terminate {
		t.Fatalf("codex must be admitted (quota untouched by 403s): %+v %s", resp, resp.ResponseBody)
	}
	if got := keysByScope(t, app)[scope].CurrentConcurrency; got != 1 {
		t.Fatalf("admitted request slot = %d, want 1", got)
	}
	// A blocked client gets 403 even when billing would refuse (client check first).
	if resp := clientIntercept(t, app, scope, "blocked2", "curl/8.4.0"); resp.StatusCode != http.StatusForbidden {
		t.Fatalf("client check must run before admission: %+v", resp)
	}
	if resp := clientIntercept(t, app, scope, "ok2", codexUA); !resp.Terminate || resp.StatusCode == http.StatusForbidden {
		t.Fatalf("second codex request must be refused by billing (slot held), got %+v", resp)
	}
	completeOnce(t, app, "ok1")
	if got := keysByScope(t, app)[scope].CurrentConcurrency; got != 0 {
		t.Fatalf("slot not released: %d", got)
	}
	st := clientState(t, app)
	if !st.Module.Active || st.State == nil || len(st.State.Recent) < 4 {
		t.Fatalf("state = %+v", st.Module)
	}
	callOK(t, app, http.MethodPost, routeSuiteClientClear, nil, nil, http.StatusOK, nil)
	if st = clientState(t, app); len(st.State.Recent) != 0 {
		t.Fatalf("clear-stats left %d entries", len(st.State.Recent))
	}
	if resp := callManagement(t, app, http.MethodPut, routeSuiteClientRules, nil, map[string]any{
		"default": map[string]any{"mode": "enforce", "allow": []string{"re:("}}}); resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("bad regex must be rejected: %d %s", resp.StatusCode, resp.Body)
	}
}

func TestSuiteClientObserveAndOff(t *testing.T) {
	rules := map[string]any{"default": map[string]any{"mode": "enforce", "allow": []string{"codex"}}, "keys": map[string]any{}}
	for _, mode := range []string{"observe", "off"} {
		app, _, scope, _ := watchOnlyApp(t, "modules:\n  client_guard: "+mode+"\n")
		if mode == "off" {
			if resp := callManagement(t, app, http.MethodPut, routeSuiteClientRules, nil, rules); resp.StatusCode == http.StatusOK {
				t.Fatal("off module must refuse rule edits")
			}
		} else {
			callOK(t, app, http.MethodPut, routeSuiteClientRules, nil, rules, http.StatusOK, nil)
		}
		if resp := clientIntercept(t, app, scope, "r", "curl/8"); resp.StatusCode == http.StatusForbidden {
			t.Fatalf("%s must never 403", mode)
		}
		st := clientState(t, app)
		if mode == "observe" && (st.State == nil || len(st.State.Recent) != 1 || st.State.Recent[0].Result != "allowed" && st.State.Recent[0].Result != "not-in-list") {
			t.Fatalf("observe must record without blocking: %+v", st.State)
		}
		if mode == "off" && st.State != nil {
			t.Fatal("off must not run the guard")
		}
	}
}

func writeGuardFiles(t *testing.T, plugins, scope string) (string, string) {
	t.Helper()
	dir := filepath.Join(plugins, "client-guard")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	rules := `{"default":{"mode":"log-only","allow":["codex"]},"keys":{"` + scope + `":{"label":"alice","mode":"enforce","allow":["re:^codex[_-]","header:X-Title=Cherry Studio"]},"` + strings.Repeat("a", 64) + `":{"mode":"inherit","allow":[]}}}`
	stats := `{"seen":{"` + scope + `":{"curl/8":{"client":"curl","ua":"curl/8","count":7,"blocked":7,"first":"2026-10-01T00:00:00Z","last":"2026-10-02T00:00:00Z"}}},"recent":[{"time":"2026-10-02T00:00:00Z","scope":"` + scope + `","ua":"curl/8","originator":"","client":"curl","mode":"enforce","result":"blocked"}]}`
	rp, sp := filepath.Join(dir, "rules.json"), filepath.Join(dir, "stats.json")
	if err := os.WriteFile(rp, []byte(rules), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(sp, []byte(stats), 0o644); err != nil {
		t.Fatal(err)
	}
	return rp, sp
}

func startSuite(t *testing.T, state, plugins string) *App {
	t.Helper()
	app := newTestApp(t)
	if err := registerWithYAML(t, app, "enabled: true\nstate_file: \""+state+"\"\nimport_from: none\nplugins_dir: \""+plugins+"\"\n"); err != nil {
		t.Fatal(err)
	}
	return app
}

// TestSuiteClientGuardMigration: stage 1 client-guard.so present -> paused,
// nothing imported; stage 2 .so moved out -> import once, files untouched,
// key identity preserved, never re-imported.
func TestSuiteClientGuardMigration(t *testing.T) {
	dir := t.TempDir()
	plugins, state := filepath.Join(dir, "plugins"), filepath.Join(dir, "s.db")
	const key = "sk-guard-migrate-0001"
	scope := billing.CallerScope(key)
	rp, sp := writeGuardFiles(t, plugins, scope)
	so := filepath.Join(plugins, runtime.GOOS, runtime.GOARCH, "client-guard.so")
	touchFile(t, so)
	touchFile(t, so+".bak")
	hashes := func() string { return sha256File(t, rp) + sha256File(t, sp) }
	before := hashes()

	app := startSuite(t, state, plugins)
	st := clientState(t, app)
	if !st.Module.Paused || st.Module.Import != nil || !strings.Contains(st.Module.Reason, "client-guard.so") {
		t.Fatalf("stage 1 must pause without importing: %+v", st.Module)
	}
	if resp := clientIntercept(t, app, scope, "a", "curl/8"); resp.Terminate {
		t.Fatal("paused module must not 403 (client-guard.so still enforces)")
	}
	page := callManagementResource(t, app)
	if !strings.Contains(page, "client-guard.so 仍在加载") || strings.Contains(page, "CPA Suite 只观察模式") {
		t.Fatal("stage 1 banner must explain the pause; billing stays active")
	}
	var ws struct {
		WatchOnly bool `json:"watch_only"`
	}
	callOK(t, app, http.MethodGet, routeSuiteStatus, nil, nil, http.StatusOK, &ws)
	if ws.WatchOnly {
		t.Fatal("client-guard must not put billing into watch-only")
	}
	app.Shutdown()

	if err := os.Rename(so, filepath.Join(dir, "client-guard.so")); err != nil {
		t.Fatal(err)
	}
	app = startSuite(t, state, plugins)
	st = clientState(t, app)
	if !st.Module.Active || st.Module.Import == nil || st.Module.Import.KeyRules != 2 || !st.Module.Import.FoundStats || !st.Module.Import.SourceUnchanged {
		t.Fatalf("stage 2 import = %+v %+v", st.Module, st.Module.Import)
	}
	if r := st.State.Rules.Keys[scope]; r.Mode != "enforce" || len(r.Allow) != 2 {
		t.Fatalf("key rule not mapped to the same caller_scope: %+v", st.State.Rules.Keys)
	}
	if len(st.State.Recent) != 1 || st.State.Recent[0].Scope != scope {
		t.Fatalf("stats not imported: %+v", st.State.Recent)
	}
	if resp := clientIntercept(t, app, scope, "b", "curl/8"); resp.StatusCode != http.StatusForbidden {
		t.Fatalf("migrated enforce rule must 403: %+v", resp)
	}
	if resp := clientIntercept(t, app, scope, "c", "Mozilla/5.0"); resp.StatusCode != http.StatusForbidden {
		t.Fatal("header rule must not match without X-Title")
	}
	if resp := clientIntercept(t, app, scope, "d", codexUA); resp.Terminate {
		t.Fatalf("codex must pass: %+v", resp)
	}
	if hashes() != before {
		t.Fatal("client-guard files were modified")
	}
	importedAt := st.Module.Import.ImportedAt
	// Edit rules in the suite, change the source files, restart: no re-import.
	callOK(t, app, http.MethodPut, routeSuiteClientRules, nil, map[string]any{
		"default": map[string]any{"mode": "log-only", "allow": []string{"codex"}},
		"keys":    map[string]any{scope: map[string]any{"mode": "off", "allow": []string{}}},
	}, http.StatusOK, nil)
	app.Shutdown()
	if err := os.WriteFile(rp, []byte(`{"default":{"mode":"enforce","allow":["nothing"]},"keys":{}}`), 0o644); err != nil {
		t.Fatal(err)
	}
	app = startSuite(t, state, plugins)
	t.Cleanup(app.Shutdown)
	st = clientState(t, app)
	if st.Module.Import == nil || st.Module.Import.ImportedAt != importedAt || st.State.Rules.Default.Mode != "log-only" || st.State.Rules.Keys[scope].Mode != "off" {
		t.Fatalf("must not re-import: %+v rules=%+v", st.Module.Import, st.State.Rules)
	}
	if resp := clientIntercept(t, app, scope, "e", "curl/8"); resp.Terminate {
		t.Fatal("suite-edited rules (off) must win after restart")
	}
	var raw json.RawMessage
	callOK(t, app, http.MethodGet, routeSuiteStatus, nil, nil, http.StatusOK, &raw)
	if !strings.Contains(string(raw), `"client_guard"`) {
		t.Fatal("status must include client_guard")
	}
}

// A broken rules.json must not be imported as empty rules: paused, retried next start.
func TestSuiteClientGuardBadImportRetries(t *testing.T) {
	dir := t.TempDir()
	plugins, state := filepath.Join(dir, "plugins"), filepath.Join(dir, "s.db")
	scope := billing.CallerScope("sk-bad-import-0001")
	rp, _ := writeGuardFiles(t, plugins, scope)
	good, _ := os.ReadFile(rp)
	if err := os.WriteFile(rp, []byte("{not json"), 0o644); err != nil {
		t.Fatal(err)
	}
	app := startSuite(t, state, plugins)
	if st := clientState(t, app); st.Module.Active || st.Module.Error == "" || st.Module.Import != nil {
		t.Fatalf("bad import must pause: %+v", st.Module)
	}
	app.Shutdown()
	if err := os.WriteFile(rp, good, 0o644); err != nil {
		t.Fatal(err)
	}
	app = startSuite(t, state, plugins)
	t.Cleanup(app.Shutdown)
	if st := clientState(t, app); !st.Module.Active || st.Module.Import == nil {
		t.Fatalf("retry must import: %+v", st.Module)
	}
}
