package plugin

import (
	"bytes"
	"os"
	"path/filepath"
	"regexp"
	"testing"
)

var pluginPathRef = regexp.MustCompile(`/v0/(management|resource)/plugins/[A-Za-z0-9_.-]`)

// Regression for test build 1: the UI hardcoded /v0/management/plugins/cpa-key-billing
// and every call 404'd under cpa-suite. No web asset may hardcode a plugin path.
func TestSuiteWebAssetsHaveNoHardcodedPluginPaths(t *testing.T) {
	var files []string
	_ = filepath.WalkDir("web", func(path string, d os.DirEntry, err error) error {
		if err == nil && !d.IsDir() {
			files = append(files, path)
		}
		return err
	})
	if len(files) == 0 {
		t.Fatal("no web assets found")
	}
	for _, f := range files {
		raw, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		if loc := pluginPathRef.FindIndex(raw); loc != nil {
			t.Fatalf("%s hardcodes a plugin API path: %q", f, raw[loc[0]:min(len(raw), loc[1]+30)])
		}
		if bytes.Contains(raw, []byte("plugins/cpa-key-billing")) {
			t.Fatalf("%s references plugins/cpa-key-billing", f)
		}
	}
}

func TestSuiteUIInjectsPluginID(t *testing.T) {
	app := newTestApp(t)
	t.Cleanup(app.Shutdown)
	page := app.suiteUI(uiHTML)
	if !bytes.Contains(page, []byte(`<meta name="cpa-plugin-id" content="cpa-suite">`)) {
		t.Fatal("served UI lacks the plugin ID meta tag")
	}
	if !bytes.Contains(page, []byte(`"/v0/management/plugins/" + PLUGIN_ID`)) {
		t.Fatal("UI does not build PLUGIN_BASE from PLUGIN_ID")
	}
}
