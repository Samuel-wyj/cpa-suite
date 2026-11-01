package clientguard

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestSummaryAndDaily(t *testing.T) {
	g := newGuardState()
	g.cfg.RecentLimit = 10
	g.setRulesLocked(RuleSet{Default: Rule{Mode: ModeEnforce, Allow: []string{"codex"}}, Keys: map[string]Rule{
		scopeA:                  {Mode: ModeInherit},
		strings.Repeat("b", 64): {Mode: ModeLog, Label: "李四"},
	}})
	g.record(LogEntry{Time: "2026-10-12 10:00:00", Scope: scopeA, Client: "codex", Result: "allowed"}, false)
	g.record(LogEntry{Time: "2026-10-12 10:01:00", Scope: scopeA, Client: "curl", Result: "blocked"}, true)
	g.record(LogEntry{Time: "2026-10-11 09:00:00", Scope: "anonymous", Client: "curl", Result: "not-in-list"}, false)
	g.mu.RLock()
	s := g.summaryLocked("2026-10-12")
	g.mu.RUnlock()
	if s.TodayRequests != 2 || s.TodayBlocked != 1 || s.TotalRequests != 3 || s.TotalBlocked != 1 {
		t.Fatalf("summary counts: %+v", s)
	}
	if s.KeysSeen != 2 || s.ClientsSeen != 2 || s.KeysWithRules != 1 || s.EnforcedKeys != 1 {
		t.Fatalf("summary keys: %+v", s)
	}
	if d := g.stats.Daily["2026-10-11"]; d == nil || d.NotInList != 1 {
		t.Fatalf("daily: %+v", g.stats.Daily)
	}
}

func TestDailyKeepsAtMost31Days(t *testing.T) {
	g := newGuardState()
	g.setRulesLocked(RuleSet{Default: Rule{Mode: ModeLog}})
	for d := 1; d <= 40; d++ {
		day := "2026-08-" + pad2(d)
		if d > 31 {
			day = "2026-09-" + pad2(d-31)
		}
		g.record(LogEntry{Time: day + " 00:00:00", Scope: scopeA, Client: "c", Result: "allowed"}, false)
	}
	if len(g.stats.Daily) != maxDailyDays {
		t.Fatalf("daily len=%d", len(g.stats.Daily))
	}
	if _, ok := g.stats.Daily["2026-08-01"]; ok {
		t.Fatal("oldest day not evicted")
	}
}

func pad2(n int) string { return string(rune('0'+n/10)) + string(rune('0'+n%10)) }

// 0.1.0 写出的 stats.json 没有 daily 字段，必须能正常读取并继续计数。
func TestLoadOldStatsWithoutDaily(t *testing.T) {
	dir := t.TempDir()
	old := `{"recent":[],"seen":{"` + scopeA + `":{"codex":{"client":"codex","count":5,"blocked":2,"first_seen":"x","last_seen":"y"}}}}`
	if err := os.WriteFile(filepath.Join(dir, "stats.json"), []byte(old), 0o600); err != nil {
		t.Fatal(err)
	}
	g := newGuardState()
	g.cfg.DataDir = dir
	g.loadStatsLocked()
	g.setRulesLocked(RuleSet{Default: Rule{Mode: ModeLog}})
	g.record(LogEntry{Time: "2026-10-12 10:00:00", Scope: scopeA, Client: "codex", Result: "allowed"}, false)
	snap := g.snapshot()
	if snap.Summary.TotalRequests != 6 || snap.Summary.TotalBlocked != 2 {
		t.Fatalf("summary=%+v", snap.Summary)
	}
	raw, _ := json.Marshal(snap)
	if !strings.Contains(string(raw), `"summary":{`) {
		t.Fatal("summary missing from state json")
	}
}

// 0.1.0 保存的 rules.json（含 label）在 0.1.1 中必须原样可用。
func TestOldRulesJSONCompatible(t *testing.T) {
	old := `{"default":{"mode":"enforce","allow":["codex"]},"keys":{"` + scopeA + `":{"mode":"inherit","allow":[],"label":"张三"}}}`
	var rs RuleSet
	if err := json.Unmarshal([]byte(old), &rs); err != nil {
		t.Fatal(err)
	}
	norm, err := NormalizeRuleSet(rs)
	if err != nil {
		t.Fatal(err)
	}
	if norm.Default.Mode != ModeEnforce || norm.Keys[scopeA].Label != "张三" {
		t.Fatalf("norm=%+v", norm)
	}
}
