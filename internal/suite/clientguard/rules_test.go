package clientguard

import (
	"encoding/json"
	"net/http"
	"os"
	"strings"
	"testing"
)

var scopeA = strings.Repeat("a", 64)

func mustCompile(t *testing.T, rs RuleSet) compiledRuleSet {
	t.Helper()
	c, err := compileRuleSet(rs)
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func TestPatternMatching(t *testing.T) {
	cases := []struct {
		p, s string
		want bool
	}{
		{"codex", "codex_cli_rs/0.46.0 (Mac OS 15.1; arm64)", true},
		{"CODEX", "Codex Desktop/1.0", true},
		{"codex", "CherryStudio/1.5", false},
		{"re:^curl/", "curl/8.4.0", true},
		{"re:^curl/", "my-curl/1", false},
		{"claude-cli", "claude-cli/1.0.80 (external, cli)", true},
		{"codex", "", false},
	}
	for _, c := range cases {
		cp, err := compilePattern(c.p)
		if err != nil {
			t.Fatal(err)
		}
		if got := cp.match(c.s); got != c.want {
			t.Errorf("%q vs %q = %v want %v", c.p, c.s, got, c.want)
		}
	}
	if _, err := compilePattern("re:("); err == nil {
		t.Error("bad regex accepted")
	}
}

func TestDecide(t *testing.T) {
	c := mustCompile(t, RuleSet{Default: Rule{Mode: ModeLog, Allow: []string{"codex"}}, Keys: map[string]Rule{
		scopeA: {Mode: ModeEnforce},
	}})
	// 默认 log-only：不在列表也不拦
	d := c.decide("anonymous", "CherryStudio/1.0", "")
	if d.Block || d.Allowed || d.Mode != ModeLog {
		t.Fatalf("log-only: %+v", d)
	}
	// key enforce 且允许列表为空 → 沿用默认列表 codex
	d = c.decide(scopeA, "Mozilla/5.0 CherryStudio/1.0", "")
	if !d.Block {
		t.Fatalf("enforce should block: %+v", d)
	}
	d = c.decide(scopeA, "something", "codex_cli_rs")
	if d.Block || d.Matched != "codex" {
		t.Fatalf("originator should match: %+v", d)
	}
	// 全部为空的 enforce 不拦
	c2 := mustCompile(t, RuleSet{Default: Rule{Mode: ModeEnforce}})
	if d := c2.decide(scopeA, "x", ""); d.Block || !d.EmptyAllow {
		t.Fatalf("empty allow: %+v", d)
	}
	// key off
	c3 := mustCompile(t, RuleSet{Default: Rule{Mode: ModeEnforce, Allow: []string{"codex"}}, Keys: map[string]Rule{scopeA: {Mode: ModeOff}}})
	if d := c3.decide(scopeA, "curl/8", ""); d.Block || d.Mode != ModeOff {
		t.Fatalf("off: %+v", d)
	}
	if d := c3.decide("anonymous", "curl/8", ""); !d.Block {
		t.Fatalf("default enforce: %+v", d)
	}
}

func TestNormalizeRejects(t *testing.T) {
	if _, err := NormalizeRuleSet(RuleSet{Default: Rule{Mode: "inherit"}}); err == nil {
		t.Error("default inherit accepted")
	}
	if _, err := NormalizeRuleSet(RuleSet{Default: Rule{Mode: "log-only"}, Keys: map[string]Rule{"bad": {}}}); err == nil {
		t.Error("bad scope accepted")
	}
	rs, err := NormalizeRuleSet(RuleSet{Default: Rule{Allow: []string{" codex ", "codex", ""}}})
	if err != nil || rs.Default.Mode != ModeLog || len(rs.Default.Allow) != 1 {
		t.Fatalf("%+v %v", rs, err)
	}
}

func TestParseConfigDefaults(t *testing.T) {
	c, err := parseConfig([]byte("enabled: true\npriority: 10\n"))
	if err != nil || c.DefaultMode != ModeLog || !c.FailOpen || c.DataDir != defaultDataDir || len(c.DefaultAllow) != 1 {
		t.Fatalf("%+v %v", c, err)
	}
	if _, err := parseConfig([]byte("default_mode: block\n")); err == nil {
		t.Error("bad mode accepted")
	}
	c, _ = parseConfig([]byte("fail_open: false\ndefault_allow: []\n"))
	if c.FailOpen || len(c.DefaultAllow) != 0 {
		t.Fatalf("%+v", c)
	}
}

func TestClientLabel(t *testing.T) {
	cases := map[[2]string]string{
		{"codex_cli_rs/0.46.0 (Mac OS 15.1; arm64) iTerm", "codex_cli_rs"}: "Codex CLI",
		{"curl/8.4.0", ""}: "curl",
		{"Mozilla/5.0 (Windows NT 10.0) AppleWebKit/537.36 CherryStudio/1.5.3 Chrome/130 Electron/33 Safari/537.36", ""}: "CherryStudio",
		{"", ""}: "(无 User-Agent)",
	}
	for in, want := range cases {
		if got := clientLabel(in[0], in[1]); got != want {
			t.Errorf("%v => %q want %q", in, got, want)
		}
	}
}

// 端到端：register → intercept → 统计 → 重启恢复（CPA Suite 版：经 Guard API，不经 cgo）。
func TestInterceptFlow(t *testing.T) {
	dir := t.TempDir()
	g := New(nil)
	cfg := []byte("data_dir: " + dir + "\n")
	g.Configure(cfg, 6)
	call := func(ua string) requestInterceptResponse {
		raw, _ := json.Marshal(requestInterceptRequest{Model: "gpt-5", Headers: http.Header{"User-Agent": {ua}}, Metadata: map[string]any{"caller_scope": scopeA, "request_path": "/v1/responses"}})
		out, _ := g.InterceptBefore(raw, Options{})
		var env envelope
		json.Unmarshal(out, &env)
		var r requestInterceptResponse
		json.Unmarshal(env.Result, &r)
		return r
	}
	if r := call("CherryStudio/1"); r.Terminate {
		t.Fatal("log-only must not block")
	}
	if _, _, err := g.SaveRules(RuleSet{Default: Rule{Mode: ModeLog, Allow: []string{"codex"}}, Keys: map[string]Rule{scopeA: {Mode: ModeEnforce}}}); err != nil {
		t.Fatal(err)
	}
	r := call("CherryStudio/1")
	if !r.Terminate || r.StatusCode != 403 || !strings.Contains(string(r.ResponseBody), "client_not_allowed") {
		t.Fatalf("expected 403: %+v %s", r, r.ResponseBody)
	}
	if r := call("codex_cli_rs/0.46"); r.Terminate {
		t.Fatal("codex must pass")
	}
	snap := g.Snapshot()
	if len(snap.Recent) != 3 || len(snap.Seen[scopeA]) != 2 {
		t.Fatalf("stats: %+v", snap)
	}
	g.Flush()
	if _, err := os.Stat(dir + "/rules.json"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(dir + "/stats.json"); err != nil {
		t.Fatal(err)
	}
	// 重启后恢复
	g.Close()
	g = New(nil)
	g.Configure(cfg, 6)
	if s := g.Snapshot(); len(s.Recent) != 3 || s.Rules.Keys[scopeA].Mode != ModeEnforce {
		t.Fatalf("restore failed: %+v", s.Rules)
	}
	// 观察模式：记录但不拦截
	if raw, _ := json.Marshal(requestInterceptRequest{Headers: http.Header{"User-Agent": {"CherryStudio/1"}}, Metadata: map[string]any{"caller_scope": scopeA}}); true {
		if _, term := g.InterceptBefore(raw, Options{Observe: true}); term {
			t.Fatal("observe must not block")
		}
	}
	// 坏 JSON → fail-open
	out, term := g.InterceptBefore([]byte("{bad"), Options{})
	if term || strings.Contains(string(out), "Terminate") {
		t.Fatal("should fail open")
	}
	g.Close()
}
