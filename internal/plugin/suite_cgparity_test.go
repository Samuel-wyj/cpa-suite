package plugin

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestClientGuardParityDump is driven by scripts/clientguard_parity.sh: it
// runs the same cases as the pristine client-guard 0.1.2 through the full
// CPA Suite plugin after importing the same data dir, and dumps the results.
func TestClientGuardParityDump(t *testing.T) {
	plugins, casesPath, outPath := os.Getenv("CG_PLUGINS"), os.Getenv("CG_CASES"), os.Getenv("CG_OUT")
	if plugins == "" {
		t.Skip("set by scripts/clientguard_parity.sh")
	}
	var cases []json.RawMessage
	raw, err := os.ReadFile(casesPath)
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(raw, &cases); err != nil {
		t.Fatal(err)
	}
	block := ""
	for _, line := range strings.Split(strings.TrimSpace(os.Getenv("CG_YAML")), "\n") {
		if strings.TrimSpace(line) != "" {
			block += "  " + line + "\n"
		}
	}
	yaml := "enabled: true\nstate_file: \"" + filepath.Join(t.TempDir(), "s.db") + "\"\nimport_from: none\nplugins_dir: \"" + plugins + "\"\nmodules:\n  core: observe\n"
	if block != "" {
		yaml += "client_guard:\n" + block
	}
	app := newTestApp(t)
	if err := registerWithYAML(t, app, yaml); err != nil {
		t.Fatal(err)
	}
	var outs []json.RawMessage
	for _, c := range cases {
		out, err := app.HandleMethod(MethodRequestInterceptBefore, c)
		if err != nil {
			t.Fatal(err)
		}
		outs = append(outs, out)
	}
	var st struct {
		State json.RawMessage `json:"state"`
	}
	callOK(t, app, "GET", routeSuiteClients, nil, nil, 200, &st)
	res, _ := json.Marshal(map[string]any{"responses": outs, "snapshot": st.State})
	if err := os.WriteFile(outPath, res, 0o644); err != nil {
		t.Fatal(err)
	}
	app.Shutdown()
}
