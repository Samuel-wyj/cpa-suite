package suite

import (
	"database/sql"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"

	"gopkg.in/yaml.v3"
)

func TestSplitConfigStripsSuiteKeysAndDefaultsStateFile(t *testing.T) {
	raw := []byte("enabled: true\npriority: 10\nstore:\n  id: cpa-suite\nimport_from: /data/old.db\nplugins_dir: /p\nmodules:\n  core: observe\ndebug: true\n")
	cfg, out, err := SplitConfig(raw)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.ImportFrom != "/data/old.db" || cfg.PluginsDir != "/p" || cfg.Modules["core"] != "observe" || cfg.StateFile != DefaultStateFile {
		t.Fatalf("cfg = %+v", cfg)
	}
	var doc map[string]any
	if err := yaml.Unmarshal(out, &doc); err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"import_from", "plugins_dir", "modules"} {
		if _, ok := doc[key]; ok {
			t.Fatalf("suite key %s leaked to upstream YAML: %s", key, out)
		}
	}
	if doc["state_file"] != DefaultStateFile || doc["enabled"] != true || doc["priority"] != 10 || doc["debug"] != true || doc["store"] == nil {
		t.Fatalf("upstream YAML = %s", out)
	}
}

func TestSplitConfigKeepsExplicitStateFileAndUnknownKeys(t *testing.T) {
	cfg, out, err := SplitConfig([]byte("state_file: x/y.db\nbogus: 1\n"))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.StateFile != "x/y.db" || cfg.PluginsDir != DefaultPluginsDir || !strings.Contains(string(out), "bogus: 1") || !strings.Contains(string(out), "state_file: x/y.db") {
		t.Fatalf("cfg=%+v out=%s", cfg, out)
	}
	if _, out, err = SplitConfig([]byte("state_file: \"\"\n")); err != nil || !strings.Contains(string(out), DefaultStateFile) {
		t.Fatalf("blank state_file: %s %v", out, err)
	}
	if _, out, err = SplitConfig(nil); err != nil || !strings.Contains(string(out), DefaultStateFile) {
		t.Fatalf("empty config: %s %v", out, err)
	}
	if _, _, err = SplitConfig([]byte("modules: [core]\n")); err == nil {
		t.Fatal("modules list must be rejected")
	}
}

func TestResolveModules(t *testing.T) {
	modules, err := ResolveModules(nil)
	if err != nil || modules[0].ID != "core" || modules[0].Mode != ModeEnforce {
		t.Fatalf("defaults = %+v %v", modules, err)
	}
	for _, m := range modules[1:] {
		if m.ID == "candy_test" {
			if !m.Available || m.Mode != ModeOff {
				t.Fatalf("candy_test must be available and OFF by default: %+v", m)
			}
			continue
		}
		if m.ID == "client_guard" || m.ID == "forecast" || m.ID == "ranking" || m.ID == "grok_monitor" || m.ID == "alerts" || m.ID == "auth_alerts" || m.ID == "shared_quota" {
			if !m.Available || m.Mode != ModeOn {
				t.Fatalf("%s must be available and on by default: %+v", m.ID, m)
			}
			continue
		}
		if m.Available || m.Mode != ModeOff {
			t.Fatalf("module %s must be unavailable/off in this build: %+v", m.ID, m)
		}
	}
	if modules, err = ResolveModules(map[string]string{"client_guard": "observe"}); err != nil || modules[1].Mode != ModeObserve {
		t.Fatalf("client_guard observe = %+v %v", modules, err)
	}
	if modules, err = ResolveModules(map[string]string{"forecast": "passive", "ranking": "off"}); err != nil || modules[2].Mode != ModePassive || modules[3].Mode != ModeOff {
		t.Fatalf("forecast passive / ranking off = %+v %v", modules, err)
	}
	for _, m := range modules {
		if m.Description == "pre-release" || m.Description == "" {
			t.Fatalf("module %s needs a real description", m.ID)
		}
		if m.ID == "gemini_quota" {
			t.Fatal("gemini_quota was removed in 0.5")
		}
	}
	if modules, err = ResolveModules(map[string]string{"core": "observe"}); err != nil || modules[0].Mode != ModeObserve {
		t.Fatalf("observe = %+v %v", modules, err)
	}
	for _, bad := range []map[string]string{{"nope": "on"}, {"core": "on"}, {"forecast": "enforce"}, {"ranking": "passive"}, {"gemini_quota": "off"}, {"grok_monitor": "passive"}, {"alerts": "observe"}} {
		if _, err := ResolveModules(bad); err == nil {
			t.Fatalf("%v must be rejected", bad)
		}
	}
}

func touch(t *testing.T, path string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestPluginIDFromFileMatchesCPANaming(t *testing.T) {
	for name, want := range map[string]string{
		"cpa-key-billing.so": "cpa-key-billing", "cpa-key-billing-v1.3.20.so": "cpa-key-billing",
		"cpa-key-billing.dylib": "cpa-key-billing", "client-guard.so": "client-guard", "CPA-KEY-BILLING.SO": "CPA-KEY-BILLING",
		"cpa-key-billing-vX.so": "cpa-key-billing-vX",
	} {
		if got, ok := pluginIDFromFile(name); !ok || got != want {
			t.Fatalf("%s -> %q %v, want %q", name, got, ok, want)
		}
	}
	if _, ok := pluginIDFromFile("cpa-key-billing.so.bak"); ok {
		t.Fatal(".bak must not count as a plugin")
	}
}

func TestScanPluginsFindsConflictsInBothDirs(t *testing.T) {
	dir := t.TempDir()
	touch(t, filepath.Join(dir, runtime.GOOS, runtime.GOARCH, "cpa-key-billing-v1.3.20.so"))
	touch(t, filepath.Join(dir, "cpa-key-billing.so"))
	touch(t, filepath.Join(dir, runtime.GOOS, runtime.GOARCH, "client-guard.so"))
	touch(t, filepath.Join(dir, runtime.GOOS, runtime.GOARCH, "cpa-suite.so"))
	touch(t, filepath.Join(dir, "backup", "cpa-key-billing.so")) // not a CPA discovery dir
	touch(t, filepath.Join(dir, "cpa-key-billing.so.disabled"))
	conflicts, coexisting := ScanPlugins(dir)
	if len(conflicts) != 2 || len(coexisting) != 1 || coexisting[0].PluginID != "client-guard" {
		t.Fatalf("conflicts=%+v coexisting=%+v", conflicts, coexisting)
	}
	if c, co := ScanPlugins(filepath.Join(dir, "missing")); len(c)+len(co) != 0 {
		t.Fatal("missing dir must yield nothing")
	}
}

func newSourceDB(t *testing.T, path string, version int) {
	t.Helper()
	db, err := sql.Open("sqlite3", path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	for _, table := range importedTables {
		if _, err := db.Exec(`CREATE TABLE "` + table + `" (v TEXT)`); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := db.Exec(`INSERT INTO api_keys VALUES ('a'), ('b'); INSERT INTO plans VALUES ('p')`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec("PRAGMA user_version = " + itoa(version)); err != nil {
		t.Fatal(err)
	}
}

func itoa(v int) string { return strconv.Itoa(v) }

func TestImportDatabaseRefusals(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "src.db")
	newSourceDB(t, src, 18)
	if _, err := ImportDatabase(src, filepath.Join(dir, "t1.db"), 17, time.Now()); err == nil || !strings.Contains(err.Error(), "newer") {
		t.Fatalf("newer schema: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "t1.db")); !os.IsNotExist(err) {
		t.Fatal("refused import must not create the target")
	}
	if _, err := ImportDatabase(src, src, 18, time.Now()); err == nil {
		t.Fatal("same file must be refused")
	}
	zero := filepath.Join(dir, "zero.db")
	newSourceDB(t, zero, 0)
	if _, err := ImportDatabase(zero, filepath.Join(dir, "t2.db"), 17, time.Now()); err == nil {
		t.Fatal("user_version 0 must be refused")
	}
	if _, err := ImportDatabase(filepath.Join(dir, "missing.db"), filepath.Join(dir, "t3.db"), 17, time.Now()); err == nil {
		t.Fatal("missing source must be refused")
	}
	ok := filepath.Join(dir, "ok.db")
	newSourceDB(t, ok, 17)
	existing := filepath.Join(dir, "exists.db")
	touch(t, existing)
	if _, err := ImportDatabase(ok, existing, 17, time.Now()); err == nil {
		t.Fatal("existing target must be refused")
	}
	result, err := ImportDatabase(ok, filepath.Join(dir, "new.db"), 17, time.Now())
	if err != nil || !result.SourceUnchanged || result.SchemaVersion != 17 || result.Rows["api_keys"] != 2 || result.Rows["plans"] != 1 {
		t.Fatalf("import = %+v %v", result, err)
	}
	if _, err := os.Stat(filepath.Join(dir, "new.db.importing")); !os.IsNotExist(err) {
		t.Fatal("temporary file left behind")
	}
}

func TestControllerRescansAndHonoursObserveSwitch(t *testing.T) {
	dir := t.TempDir()
	plugins := filepath.Join(dir, "plugins")
	now := time.Date(2026, 10, 12, 0, 0, 0, 0, time.UTC)
	c := NewController(17)
	c.now = func() time.Time { return now }
	defer c.Close()
	cfg := "state_file: " + filepath.Join(dir, "s.db") + "\nplugins_dir: " + plugins + "\nimport_from: none\n"
	if _, err := c.Prepare([]byte(cfg)); err != nil {
		t.Fatal(err)
	}
	if c.WatchOnly() {
		t.Fatal("no conflicts: must enforce")
	}
	touch(t, filepath.Join(plugins, runtime.GOOS, runtime.GOARCH, "cpa-key-billing.so"))
	now = now.Add(rescanInterval - time.Second)
	if c.WatchOnly() {
		t.Fatal("rescan must be rate limited")
	}
	now = now.Add(2 * time.Second)
	if !c.WatchOnly() {
		t.Fatal("conflict appearing at runtime must switch to watch-only")
	}
	if s := c.Status(); !s.WatchOnly || len(s.Conflicts) != 1 || !strings.Contains(c.BannerHTML(), "只观察") {
		t.Fatalf("status = %+v", s)
	}
	os.RemoveAll(plugins)
	now = now.Add(rescanInterval)
	if c.WatchOnly() {
		t.Fatal("removing the conflict must restore enforcement")
	}
	if _, err := c.Prepare([]byte(cfg + "modules:\n  core: observe\n")); err != nil {
		t.Fatal(err)
	}
	if !c.WatchOnly() {
		t.Fatal("core: observe must be watch-only")
	}
}

func TestExtDatabaseVersioning(t *testing.T) {
	path := filepath.Join(t.TempDir(), "x.suite-ext.db")
	e, err := openExt(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := e.put("k", map[string]int{"a": 1}); err != nil {
		t.Fatal(err)
	}
	var got map[string]int
	if ok, err := e.get("k", &got); !ok || err != nil || got["a"] != 1 {
		t.Fatalf("get = %v %v %v", got, ok, err)
	}
	if _, err := e.db.Exec("PRAGMA user_version = 99"); err != nil {
		t.Fatal(err)
	}
	e.close()
	if _, err := openExt(path); err == nil {
		t.Fatal("newer extension schema must be refused")
	}
	if ExtPath("plugins/cpa-suite.db") != "plugins/cpa-suite.suite-ext.db" {
		t.Fatal(ExtPath("plugins/cpa-suite.db"))
	}
}

func TestModuleSwitchesRuntimeAndPrecedence(t *testing.T) {
	dir := t.TempDir()
	c := NewController(17)
	defer c.Close()
	cfg := "state_file: " + filepath.Join(dir, "s.db") + "\nplugins_dir: " + filepath.Join(dir, "p") + "\nimport_from: none\n"
	if _, err := c.Prepare([]byte(cfg + "modules:\n  ranking: off\n")); err != nil {
		t.Fatal(err)
	}
	if c.BannerHTML() != "" {
		t.Fatalf("healthy install must show no banner: %q", c.BannerHTML())
	}
	if _, err := c.SetModuleMode("ranking", "on"); err == nil || !strings.Contains(err.Error(), "config.yaml") {
		t.Fatalf("config-locked module must refuse: %v", err)
	}
	if _, err := c.SetModuleMode("forecast", "bogus"); err == nil {
		t.Fatal("invalid mode accepted")
	}
	mods, err := c.SetModuleMode("forecast", "passive")
	if err != nil || c.ModuleMode("forecast") != "passive" {
		t.Fatalf("%v %v", mods, err)
	}
	if _, err := c.SetModuleMode("candy_test", "on"); err != nil || c.ModuleMode("candy_test") != "on" {
		t.Fatal(err)
	}
	if _, err := c.SetModuleMode("core", "observe"); err != nil || !c.WatchOnly() {
		t.Fatal("core observe from UI must take effect immediately")
	}
	_, _ = c.SetModuleMode("core", "enforce")
	if c.WatchOnly() {
		t.Fatal("core enforce restored")
	}
	// Restart: saved UI switches survive; config.yaml still wins where set.
	if _, err := c.Prepare([]byte(cfg + "modules:\n  ranking: off\n  candy_test: off\n")); err != nil {
		t.Fatal(err)
	}
	src := map[string]Module{}
	for _, m := range c.Status().Modules {
		src[m.ID] = m
	}
	if f := src["forecast"]; f.Mode != "passive" || f.Source != "ui" || f.Locked {
		t.Fatalf("forecast %+v", f)
	}
	if k := src["candy_test"]; k.Mode != "off" || k.Source != "config" || !k.Locked {
		t.Fatalf("candy %+v", k)
	}
	if a := src["alerts"]; a.Source != "default" {
		t.Fatalf("alerts %+v", a)
	}
}
