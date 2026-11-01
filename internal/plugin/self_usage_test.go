package plugin

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"cpa-key-billing/internal/billing"
	"cpa-key-billing/internal/suite"
)

func usageFixture(now time.Time) ([]forecastAccount, []hostAuthFile) {
	p := func(v float64) *float64 { return &v }
	win := func(key string, rem float64) forecastWindow {
		var w forecastWindow
		w.Key, w.RemainingPercent, w.ResetAt, w.ObservedAt = key, p(rem), now.Add(2*time.Hour), now.Add(-3*time.Minute)
		return w
	}
	accounts := []forecastAccount{
		{AuthIndex: "a1", Provider: "codex", Name: "codex-alice@example.com.json", Email: "alice@example.com", Plan: "Plus", Windows: []forecastWindow{win("5h", 18), win("weekly", 87)}},
		{AuthIndex: "a2", Provider: "xai", Name: "grok-bob@example.com.json", Email: "bob@example.com", Plan: "SuperGrok", Windows: []forecastWindow{win("weekly", 60)}},
	}
	files := []hostAuthFile{
		{ID: "codex-alice.json", AuthIndex: "a1", Type: "codex", Provider: "codex", Source: "file", Path: "/auth/codex-alice.json", Email: "alice@example.com"},
		{ID: "grok-bob.json", AuthIndex: "a2", Type: "xai", Provider: "xai", Source: "file", Path: "/auth/grok-bob.json", Email: "bob@example.com"},
	}
	return accounts, files
}

func rowNames(rows []UsageData) string {
	var n []string
	for _, r := range rows {
		n = append(n, r.PlanName)
	}
	return strings.Join(n, " | ")
}

func TestSharedQuotaFollowsRouting(t *testing.T) {
	now := time.Date(2026, 10, 16, 10, 0, 0, 0, time.UTC)
	accounts, files := usageFixture(now)
	decision := func(r billing.RouteRule) billing.RoutingDecision { return billing.RoutingDecision{RouteRule: r} }
	codexSel := billing.CredentialProviderSelector{Source: billing.CredentialSourceAuthFiles, Provider: "codex"}
	xaiSel := billing.CredentialProviderSelector{Source: billing.CredentialSourceAuthFiles, Provider: "xai"}
	cases := []struct {
		name      string
		d         billing.RoutingDecision
		want      []string
		forbidden []string
	}{
		{"no rule: every account", decision(billing.RouteRule{}), []string{"Codex Plus · 5 小时", "Codex Plus · 周", "Grok SuperGrok · 周"}, nil},
		{"allowed providers: codex only", decision(billing.RouteRule{CredentialProviders: []billing.CredentialProviderSelector{codexSel}}), []string{"Codex Plus"}, []string{"Grok"}},
		{"denied provider xai", decision(billing.RouteRule{DeniedCredentialProviders: []billing.CredentialProviderSelector{xaiSel}}), []string{"Codex Plus"}, []string{"Grok"}},
		{"allowed account id", decision(billing.RouteRule{CredentialIDs: []string{billing.CredentialFingerprint("grok-bob.json")}}), []string{"Grok"}, []string{"Codex"}},
		{"denied account id", decision(billing.RouteRule{DeniedCredentialIDs: []string{billing.CredentialFingerprint("codex-alice.json")}}), []string{"Grok"}, []string{"Codex"}},
		{"no credential rule, models grok only", decision(billing.RouteRule{Models: []string{"grok-4", "grok-code-fast-1"}}), []string{"Grok"}, []string{"Codex"}},
		{"no credential rule, models gpt only", decision(billing.RouteRule{Models: []string{"gpt-5.5", "gpt-5-codex"}}), []string{"Codex"}, []string{"Grok"}},
		{"broken rule shows nothing", billing.RoutingDecision{ConfigurationError: "missing"}, nil, []string{"Codex", "Grok"}},
	}
	for _, c := range cases {
		rows := sharedQuotaRows(accounts, files, c.d, now)
		names := rowNames(rows)
		for _, w := range c.want {
			if !strings.Contains(names, w) {
				t.Errorf("%s: want %q in %q", c.name, w, names)
			}
		}
		for _, f := range c.forbidden {
			if strings.Contains(names, f) {
				t.Errorf("%s: %q must not be visible: %q", c.name, f, names)
			}
		}
		raw, _ := json.Marshal(rows)
		if strings.Contains(string(raw), "example.com") || strings.Contains(string(raw), "alice") {
			t.Fatalf("%s: leaked account identity: %s", c.name, raw)
		}
	}
	rows := sharedQuotaRows(accounts, files, decision(billing.RouteRule{}), now)
	if r := rows[0]; r.Unit != "%" || *r.Remaining != 18 || *r.Total != 100 || !strings.Contains(r.Extra, "重置 10/16 20:00") || !strings.Contains(r.Extra, "3 分钟前") {
		t.Fatalf("row %+v", r)
	}
}

func TestPlanUsageRows(t *testing.T) {
	end := time.Date(2026, 10, 16, 12, 0, 0, 0, time.UTC)
	q := billing.QuotaView{Windows: []billing.QuotaWindowView{{Name: "5h", Started: true, EndAt: end, Dimensions: []billing.QuotaBalance{
		{Metric: billing.QuotaAmount, Limit: "5", Used: "1.25", Remaining: "3.75"},
		{Metric: billing.QuotaRequests, Limit: "100", Used: "100", Remaining: "0", Blocked: true},
	}}}}
	rows := planUsage("标准套餐", q)
	if len(rows) != 2 || rows[0].PlanName != "标准套餐 · 5h 额度" || *rows[0].Remaining != 3.75 || rows[0].Unit != "USD" || rows[0].Extra != "重置 10/16 20:00" {
		t.Fatalf("rows %+v", rows)
	}
	if *rows[1].IsValid || !strings.Contains(rows[1].InvalidMessage, "已用完") {
		t.Fatalf("blocked row %+v", rows[1])
	}
	if u := planUsage("", billing.QuotaView{Unlimited: true}); *u[0].Total != -1 {
		t.Fatalf("unlimited %+v", u)
	}
}

func TestBadKeyLimiter(t *testing.T) {
	l := &usageLimiter{cache: map[string]usageCacheEntry{}, bad: map[string][]time.Time{}}
	now := time.Now()
	for i := 0; i < badKeyLimit; i++ {
		if l.blocked("10.0.0.5", now) {
			t.Fatalf("blocked after %d", i)
		}
		l.fail("10.0.0.5", now)
	}
	if !l.blocked("10.0.0.5", now) || l.blocked("10.0.0.6", now) {
		t.Fatal("per-address limit")
	}
	if l.blocked("10.0.0.5", now.Add(badKeyWindow+time.Second)) {
		t.Fatal("window should expire")
	}
	if clientAddress(http.Header{"X-Forwarded-For": {"10.1.1.1, 172.16.0.1"}}) != "10.1.1.1" || clientAddress(http.Header{}) != "-" {
		t.Fatal("client address")
	}
}

func TestSharedQuotaModuleRegistered(t *testing.T) {
	mods, err := suite.ResolveModules(map[string]string{"shared_quota": "off"})
	if err != nil {
		t.Fatal(err)
	}
	for _, m := range mods {
		if m.ID == "shared_quota" && m.Mode == suite.ModeOff && m.Default == suite.ModeOn {
			return
		}
	}
	t.Fatal("shared_quota module missing or wrong default")
}
