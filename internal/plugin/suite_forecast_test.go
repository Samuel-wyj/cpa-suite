package plugin

import (
	"database/sql"
	"encoding/json"
	"math"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"cpa-key-billing/internal/suite"
)

func seedEvent(t *testing.T, db *sql.DB, at time.Time, scope, authIndex, provider string, usd float64, priceSource string, headers http.Header) {
	t.Helper()
	raw := "{}"
	if headers != nil {
		b, _ := json.Marshal(headers)
		raw = string(b)
	}
	if _, err := db.Exec(`INSERT INTO request_events(at, scope, auth_index, provider, billing_model, total_usd, uncached_input_tokens, billed_output_tokens, price_source, response_headers_json)
		VALUES(?,?,?,?,?,?,?,?,?,?)`, at.UnixNano(), scope, authIndex, provider, "gpt-5.5", usd, 1000, 100, priceSource, raw); err != nil {
		t.Fatal(err)
	}
}

func codexHeaders(primaryUsed, secondaryUsed float64, primaryReset, secondaryReset time.Time) http.Header {
	h := http.Header{}
	h.Set("X-Codex-Primary-Used-Percent", strconv.FormatFloat(primaryUsed, 'f', -1, 64))
	h.Set("X-Codex-Primary-Window-Minutes", "300")
	h.Set("X-Codex-Primary-Reset-At", strconv.FormatInt(primaryReset.Unix(), 10))
	h.Set("X-Codex-Secondary-Used-Percent", strconv.FormatFloat(secondaryUsed, 'f', -1, 64))
	h.Set("X-Codex-Secondary-Window-Minutes", "10080")
	h.Set("X-Codex-Secondary-Reset-At", strconv.FormatInt(secondaryReset.Unix(), 10))
	h.Set("X-Codex-Plan-Type", "plus")
	return h
}

type forecastResult struct {
	Mode     string `json:"mode"`
	Accounts []struct {
		AuthIndex, Provider, Plan, Note string
		Fetch                           *struct {
			OK         bool `json:"ok"`
			FetchedNow bool `json:"fetched_now"`
		}
		Windows []struct {
			Key            string   `json:"key"`
			UsedPercent    *float64 `json:"used_percent"`
			CapacityUSD    *float64 `json:"capacity_usd"`
			CapacitySource string   `json:"capacity_source"`
			CapacityNote   string   `json:"capacity_note"`
			RemainingUSD   *float64 `json:"remaining_usd"`
			UsedUSD        float64  `json:"used_usd"`
			Unpriced       int64    `json:"unpriced_requests"`
			PercentSource  string   `json:"percent_source"`
			Risk           string   `json:"risk"`
			HistoryCycles  int      `json:"history_cycles"`
			CapacityP25    *float64 `json:"capacity_p25"`
			CapacityP75    *float64 `json:"capacity_p75"`
			CapacityTokens *float64 `json:"capacity_tokens"`
			Confidence     string   `json:"confidence"`
			Coverage       string   `json:"coverage"`
			Samples        int      `json:"samples"`
			Burn           *struct {
				Pace        string  `json:"pace"`
				Recent      float64 `json:"recent_per_hour"`
				RecentBasis string  `json:"recent_basis"`
				Sustainable float64 `json:"sustainable_per_hour"`
				Cumulative  float64 `json:"cumulative_per_hour"`
			} `json:"burn"`
			BurnUSDPerHour float64 `json:"burn_usd_per_hour"`
			USDPoints      []any   `json:"usd_points"`
			PercentPoints  []any   `json:"percent_points"`
		} `json:"windows"`
	} `json:"accounts"`
}

func forecastApp(t *testing.T, extra string) (*App, *sql.DB, *int32, string) {
	app, _, scope, state := watchOnlyApp(t, extra)
	db, err := sql.Open("sqlite3", "file:"+state+"?_busy_timeout=5000")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	var httpCalls int32
	app.SetHostCaller(func(method string, payload any) (json.RawMessage, error) {
		switch method {
		case hostAuthList:
			return json.RawMessage(`{"files":[{"auth_index":"cx-1","name":"plus@example.com","type":"codex"},{"auth_index":"xa-1","name":"grok@example.com","type":"xai"},{"auth_index":"gm-1","name":"g.json","type":"gemini"}]}`), nil
		case hostAuthGet:
			return mustJSONRaw(t, map[string]any{"json": map[string]any{"access_token": superGrokToken()}}), nil
		case hostHTTPDo:
			atomic.AddInt32(&httpCalls, 1)
			r := payload.(hostHTTPRequest)
			// The captured weekly period ended 2026-10-19; keep it current so the
			// forecast does not treat it as a stale window.
			credits := strings.NewReplacer(
				"2026-10-12T06:01:34", time.Now().UTC().Add(-6*24*time.Hour).Format("2006-01-02T15:04:05"),
				"2026-10-19T06:01:34", time.Now().UTC().Add(24*time.Hour).Format("2006-01-02T15:04:05"),
			).Replace(realSuperGrokCredits)
			body := map[string]string{xaiCredits: credits, xaiMonthly: realSuperGrokMonthly}[r.URL]
			if body == "" {
				t.Fatalf("unexpected upstream call %s (Codex has fresh headers, must not be queried)", r.URL)
			}
			return mustJSONRaw(t, hostHTTPResponse{StatusCode: 200, Body: []byte(body)}), nil
		}
		return json.RawMessage(`{}`), nil
	})
	return app, db, &httpCalls, scope
}

func TestSuiteForecastCodexAndGrok(t *testing.T) {
	app, db, calls, scope := forecastApp(t, "")
	now := time.Now().UTC()
	r5, rw := now.Add(2*time.Hour), now.Add(3*24*time.Hour)
	// Codex: previous 5h cycle (reset an hour ago) 50% for $10 → history capacity $20.
	prevReset := now.Add(-time.Hour)
	seedEvent(t, db, prevReset.Add(-3*time.Hour), scope, "cx-1", "codex", 10, "builtin", codexHeaders(50, 5, prevReset, rw))
	// Current 5h window started 3h ago: $4 over three requests, 40% used.
	seedEvent(t, db, now.Add(-150*time.Minute), scope, "cx-1", "codex", 1, "builtin", codexHeaders(10, 6, r5, rw))
	seedEvent(t, db, now.Add(-30*time.Minute), scope, "cx-1", "codex", 2, "builtin", codexHeaders(30, 8, r5, rw))
	seedEvent(t, db, now.Add(-10*time.Minute), scope, "cx-1", "codex", 1, "builtin", codexHeaders(40, 9, r5, rw))
	seedEvent(t, db, now.Add(-5*time.Minute), scope, "cx-1", "codex", 0, "none", nil)
	// Grok: $0.42 spent this week, official 1 %.
	seedEvent(t, db, now.Add(-time.Hour), scope, "xa-1", "xai", 0.42, "builtin", nil)

	var got forecastResult
	callOK(t, app, http.MethodGet, routeSuiteForecast, nil, nil, http.StatusOK, &got)
	if got.Mode != "on" || len(got.Accounts) != 2 {
		t.Fatalf("only Codex and Grok accounts: %+v", got)
	}
	cx, xa := got.Accounts[0], got.Accounts[1]
	if cx.Provider != "codex" || cx.Plan != "Plus" || len(cx.Windows) != 2 || cx.Fetch != nil {
		t.Fatalf("codex %+v", cx)
	}
	w5 := cx.Windows[0]
	if w5.Key != "5h" || *w5.UsedPercent != 40 || w5.PercentSource != "header" || w5.UsedUSD != 4 || w5.CapacitySource != "milestones" || *w5.CapacityUSD != 12.5 || *w5.RemainingUSD != 7.5 || w5.Unpriced != 1 ||
		*w5.CapacityP25 != 8.75 || *w5.CapacityP75 != 16.25 || w5.Samples != 2 || w5.Confidence != "medium" || w5.Coverage != "unknown" {
		// milestones 10→30 % cost $1 ($5 per 100 %), 30→40 % cost $2 ($20 per 100 %): median $12.50
		t.Fatalf("codex 5h %+v", w5)
	}
	if w5.HistoryCycles != 1 || w5.BurnUSDPerHour != 3 || (w5.Risk != "low" && w5.Risk != "medium") || w5.Burn == nil || w5.Burn.RecentBasis != "milestones" || math.Abs(w5.Burn.Recent-30.0/140*60) > 0.1 /* 90-min anchor, 10→40 % over 140 min */ || w5.Burn.Pace != "slow" /* 60 % of the time, 40 % used */ || len(w5.USDPoints) < 2 || len(w5.PercentPoints) != 3 {
		t.Fatalf("codex 5h burn/history/chart %+v", w5)
	}
	if ww := cx.Windows[1]; ww.Key != "weekly" || *ww.UsedPercent != 9 || ww.CapacityUSD == nil {
		t.Fatalf("codex weekly %+v", ww)
	}
	if xa.Provider != "xai" || xa.Fetch == nil || !xa.Fetch.OK || !xa.Fetch.FetchedNow || len(xa.Windows) != 1 || atomic.LoadInt32(calls) != 2 {
		t.Fatalf("grok %+v calls=%d", xa, *calls)
	}
	wx := xa.Windows[0]
	if wx.Key != "weekly" || *wx.UsedPercent != 1 || wx.PercentSource != "api" || wx.CapacityUSD != nil || wx.CapacityNote != "not_enough_data" || wx.UsedUSD != 0.42 {
		t.Fatalf("grok weekly %+v", wx)
	}
	// Cached: no second fetch; manual refresh inside 60 s is also refused.
	callOK(t, app, http.MethodGet, routeSuiteForecast, url.Values{"refresh": {"xa-1"}}, nil, http.StatusOK, &got)
	if atomic.LoadInt32(calls) != 2 || got.Accounts[1].Fetch.FetchedNow || len(got.Accounts[1].Windows) != 1 {
		t.Fatalf("fetch must be cached/rate-limited, calls=%d %+v", *calls, got.Accounts[1])
	}
}

func TestSuiteForecastPassiveAndOff(t *testing.T) {
	app, _, calls, _ := forecastApp(t, "modules:\n  forecast: passive\n")
	var got forecastResult
	callOK(t, app, http.MethodGet, routeSuiteForecast, url.Values{"refresh": {"xa-1"}}, nil, http.StatusOK, &got)
	if got.Mode != "passive" || atomic.LoadInt32(calls) != 0 || got.Accounts[1].Note != "no_reading_passive" {
		t.Fatalf("passive must never call upstream: calls=%d %+v", *calls, got)
	}
	app, _, calls, _ = forecastApp(t, "modules:\n  forecast: off\n  ranking: off\n")
	callOK(t, app, http.MethodGet, routeSuiteForecast, nil, nil, http.StatusOK, &got)
	var rank struct {
		Mode string `json:"mode"`
		Keys []any  `json:"keys"`
	}
	callOK(t, app, http.MethodGet, routeSuiteRanking, nil, nil, http.StatusOK, &rank)
	if got.Mode != "off" || len(got.Accounts) != 0 || rank.Mode != "off" || len(rank.Keys) != 0 || atomic.LoadInt32(calls) != 0 {
		t.Fatalf("off: %+v %+v", got, rank)
	}
}

func TestSuiteRankingAndAnomalies(t *testing.T) {
	app, db, _, scope := forecastApp(t, "")
	now := time.Now().UTC()
	// Baseline: $1/day on 7 active days; last 24 h: $33 → 33×, +$32 → high (0.7 rules).
	for d := 2; d <= 8; d++ {
		seedEvent(t, db, now.Add(-time.Duration(d)*24*time.Hour+time.Hour), scope, "cx-1", "codex", 1, "builtin", nil)
	}
	for i := 0; i < 3; i++ {
		seedEvent(t, db, now.Add(-time.Duration(i+1)*time.Hour), scope, "xa-1", "xai", 11, "builtin", nil)
	}
	seedEvent(t, db, now.Add(-time.Hour), "other-scope", "cx-1", "codex", 0.5, "builtin", nil)
	q := url.Values{"from": {now.Add(-24 * time.Hour).Format(time.RFC3339Nano)}, "to": {now.Add(time.Minute).Format(time.RFC3339Nano)}}
	var got struct {
		Keys      []rankRow `json:"keys"`
		Accounts  []rankRow `json:"accounts"`
		Anomalies []anomaly `json:"anomalies"`
	}
	callOK(t, app, http.MethodGet, routeSuiteRanking, q, nil, http.StatusOK, &got)
	if len(got.Keys) != 2 || got.Keys[0].ID != scope || got.Keys[0].USD != 33 || got.Keys[0].Requests != 3 || got.Keys[0].Tokens != 3300 {
		t.Fatalf("keys %+v", got.Keys)
	}
	if len(got.Accounts) != 2 || got.Accounts[0].ID != "xa-1" || got.Accounts[0].Name != "grok@example.com" || got.Accounts[0].Provider != "xai" {
		t.Fatalf("accounts %+v", got.Accounts)
	}
	if len(got.Anomalies) != 1 || got.Anomalies[0].Scope != scope || got.Anomalies[0].Severity != "high" || got.Anomalies[0].Ratio != 33 || got.Anomalies[0].ActiveDays != 7 {
		t.Fatalf("anomalies %+v", got.Anomalies)
	}
}

func TestSuiteAnomalyRules(t *testing.T) {
	st := suite.DefaultAlertSettings()
	days := func(usd ...float64) []keyDay {
		out := []keyDay{}
		for _, u := range usd {
			out = append(out, keyDay{USD: u, Reqs: int64(u * 10)})
		}
		return out
	}
	// Screenshot replay: key b7b76d… — 10.9× its old 7-day average, but the
	// baseline had only two light days: not a ratio alert any more.
	if a := evaluateKeyAnomaly("b7b76d", 12.6, 140, days(4.1, 4), st); len(a) != 0 {
		t.Fatalf("10.9× on a 2-day baseline must not fire: %+v", a)
	}
	// Same jump with $25 on a sparse key: only an info note.
	if a := evaluateKeyAnomaly("b7b76d", 25, 300, days(4.1, 4), st); len(a) != 1 || a[0].Kind != "key_new" || a[0].Severity != "info" {
		t.Fatalf("sparse key, big day: %+v", a)
	}
	// Regular key: 5 active days at $2, today $9 → 4.5× but only +$7: quiet.
	if a := evaluateKeyAnomaly("s", 9, 90, days(2, 2, 2, 2, 2), st); len(a) != 0 {
		t.Fatalf("small absolute jump: %+v", a)
	}
	// True case: $2/day, today $15 (7.5×, +$13) → medium; $40 (20×, +$38) → high.
	if a := evaluateKeyAnomaly("s", 15, 150, days(2, 2, 2, 2, 2), st); len(a) != 1 || a[0].Severity != "medium" || a[0].ActiveDays != 5 {
		t.Fatalf("true medium: %+v", a)
	}
	if a := evaluateKeyAnomaly("s", 40, 400, days(2, 2, 2, 2, 2), st); len(a) != 1 || a[0].Severity != "high" {
		t.Fatalf("true high: %+v", a)
	}
	// Idle days do not dilute the baseline: old rule $15 / ($9/7) = 11.7× high; now 5× of the active average → medium.
	if a := evaluateKeyAnomaly("s", 15, 150, append(days(3, 3, 3), keyDay{USD: 0.01}, keyDay{}), st); len(a) != 1 || a[0].Severity != "medium" || math.Abs(a[0].Ratio-5) > 1e-9 {
		t.Fatalf("active-day baseline: %+v", a)
	}
	// Request spike with free usage.
	if a := evaluateKeyAnomaly("s", 0, 3000, []keyDay{{Reqs: 100}, {Reqs: 120}, {Reqs: 80}}, st); len(a) != 1 || a[0].Metric != "requests" || a[0].Severity != "high" {
		t.Fatalf("request spike: %+v", a)
	}
	zone := time.FixedZone("CST", 8*3600)
	now := time.Date(2026, 10, 12, 4, 0, 0, 0, time.UTC)
	day := func(d int) string { return now.In(zone).AddDate(0, 0, -d).Format("2006-01-02") }
	daily := map[string]suiteClientDayAlias{day(0): {Blocked: 60}, day(1): {Blocked: 5}, day(3): {Blocked: 9}}
	if a := blockAnomalies(toClientDays(daily), now); len(a) != 1 || a[0].Severity != "high" || a[0].Baseline != 2 {
		t.Fatalf("block spike: %+v", a)
	}
	if a := blockAnomalies(toClientDays(map[string]suiteClientDayAlias{day(0): {Blocked: 15}}), now); len(a) != 0 {
		t.Fatalf("15 blocks is below the floor: %+v", a)
	}
}

type suiteClientDayAlias = struct{ Requests, Blocked int64 }

func toClientDays(in map[string]suiteClientDayAlias) map[string]suite.ClientDay {
	out := map[string]suite.ClientDay{}
	for k, v := range in {
		out[k] = suite.ClientDay{Requests: v.Requests, Blocked: v.Blocked}
	}
	return out
}
