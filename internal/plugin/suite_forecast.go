package plugin

// CPA Suite: USD quota forecast for upstream Codex and Grok (xAI) accounts.
// Read-only. Official readings come from
//   - Codex: the X-Codex-* rate-limit headers upstream already records on every
//     response (request_events.response_headers_json) — no extra requests;
//   - Grok, and Codex accounts without a recent header: upstream's own quota
//     query (the official, read-only quota endpoints), at most once per account
//     every 10 minutes (60 s for an explicit refresh), cached in the suite
//     extension DB. Module mode "passive" never calls them.
// USD is the upstream pricing engine's total_usd of the requests each account
// served. The math lives in internal/suite/forecast.go.

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"

	"cpa-key-billing/internal/suite"
)

const (
	routeSuiteForecast    = "/suite/forecast"
	forecastFetchInterval = 10 * time.Minute
	forecastManualMin     = 60 * time.Second
	forecastHeaderFresh   = 30 * time.Minute
	forecastHistory       = 35 * 24 * time.Hour
	forecastMaxFetches    = 3
)

var forecastFetchMu sync.Mutex // one official fetch at a time

type forecastReading struct {
	window  string
	seconds int64
	used    float64
	reset   time.Time
	at      time.Time
	source  string
}

type forecastPoint struct {
	At    time.Time `json:"at"`
	Value float64   `json:"value"`
}

type forecastWindow struct {
	Key     string `json:"key"`
	Seconds int64  `json:"seconds"`
	suite.Forecast6Result
	Requests int64 `json:"requests"`
	// AlertLevel is what the alert center would raise for this window
	// (0.7 rules: remaining threshold + pace past 100 %): "" | medium | high.
	AlertLevel string `json:"alert_level,omitempty"`
	// Quiet: still inside the alert quiet period at the start of the window.
	Quiet   bool            `json:"quiet,omitempty"`
	Percent []forecastPoint `json:"percent_points"`
	USD     []forecastPoint `json:"usd_points"`
}

type forecastAccount struct {
	AuthIndex string               `json:"auth_index"`
	Provider  string               `json:"provider"`
	Name      string               `json:"name"`
	Email     string               `json:"email,omitempty"`
	Plan      string               `json:"plan,omitempty"`
	Fetch     *forecastFetch       `json:"fetch,omitempty"`
	Windows   []forecastWindow     `json:"windows"`
	Note      string               `json:"note,omitempty"`
	Coverage  string               `json:"coverage,omitempty"`
	Ledger    []suite.StoredLedger `json:"ledger"`
	Monthly   []forecastMonth      `json:"monthly"`
}

// forecastMonth is one calendar month (Asia/Shanghai) of an account.
type forecastMonth struct {
	Month     string  `json:"month"`
	USD       float64 `json:"usd"`
	Tokens    int64   `json:"tokens"`
	Requests  int64   `json:"requests"`
	Cycles    int     `json:"cycles"`
	AvgPeak   float64 `json:"avg_peak_percent"`
	Exhausted int     `json:"exhausted_cycles"`
}

type forecastFetch struct {
	At           time.Time `json:"at"`
	OK           bool      `json:"ok"`
	Error        string    `json:"error,omitempty"`
	NextAllowed  time.Time `json:"next_allowed_at"`
	FetchedNow   bool      `json:"fetched_now"`
	RefreshAfter time.Time `json:"manual_refresh_after"`
}

func forecastWindowKey(seconds int64) string {
	switch seconds {
	case 5 * 3600:
		return "5h"
	case 7 * 86400:
		return "weekly"
	}
	if seconds >= 28*86400 && seconds <= 31*86400 {
		return "monthly"
	}
	return fmt.Sprintf("%dm", seconds/60)
}

// Seconds for the labels upstream's quota query produces.
func forecastLabelSeconds(label string) int64 {
	switch label {
	case "5-hour limit":
		return 5 * 3600
	case "Weekly limit":
		return 7 * 86400
	case "Monthly limit", "Monthly allowance":
		return 30 * 86400
	}
	return 0
}

func (a *App) suiteForecast(req ManagementRequest) ManagementResponse {
	return JSONResponse(http.StatusOK, a.forecastData(req.HostCallbackID, strings.TrimSpace(req.Query.Get("refresh")), true))
}

// forecastData builds the forecast; allowFetch=false (alert center) never
// queries an official endpoint and only uses recorded/cached readings.
func (a *App) forecastData(callbackID, refresh string, allowFetch bool) map[string]any {
	mode := a.suite.ModuleMode("forecast")
	out := map[string]any{"mode": mode, "min_ratio_percent": suite.MinRatioPercent, "accounts": []forecastAccount{}, "generated_at": time.Now().UTC()}
	if mode == suite.ModeOff || mode == "" {
		return out
	}
	files, err := a.listHostAuthFiles()
	if err != nil {
		out["error"] = err.Error()
		return out
	}
	now := time.Now().UTC()
	fetches := 0
	accounts := []forecastAccount{}
	alertSettings := a.suite.AlertSettings().Settings
	for _, file := range files {
		category := authCategory(file.Type)
		if (category != "codex" && category != "xai") || strings.TrimSpace(file.AuthIndex) == "" || strings.EqualFold(strings.TrimSpace(file.AccountType), "api_key") || file.Disabled {
			continue
		}
		account := forecastAccount{AuthIndex: file.AuthIndex, Provider: category, Name: file.Name, Email: cleanText(file.Email), Windows: []forecastWindow{}}
		readings, plan, lastHeader := a.forecastHeaderReadings(file.AuthIndex, category, now)
		account.Plan = plan
		// Official endpoint (rate-limited, cached).
		last, hasLast, _ := a.suite.LastQuotaFetch(file.AuthIndex)
		due := false
		if allowFetch && mode == suite.ModeOn && !file.RuntimeOnly && fetches < forecastMaxFetches {
			stale := !hasLast || now.Sub(last.At) >= forecastFetchInterval
			switch {
			case refresh == file.AuthIndex:
				due = !hasLast || now.Sub(last.At) >= forecastManualMin
			case category == "xai":
				due = stale
			case category == "codex":
				due = stale && (lastHeader.IsZero() || now.Sub(lastHeader) > forecastHeaderFresh)
			}
		}
		if due {
			fetches++
			last, hasLast = a.forecastFetch(callbackID, file, category, now), true
		}
		if hasLast {
			f := &forecastFetch{At: last.At, OK: last.OK, Error: last.Error, NextAllowed: last.At.Add(forecastFetchInterval), RefreshAfter: last.At.Add(forecastManualMin), FetchedNow: due}
			account.Fetch = f
			var payload struct{ Plan string }
			if json.Unmarshal([]byte(last.Payload), &payload) == nil && payload.Plan != "" && (account.Plan == "" || category == "xai") {
				account.Plan = payload.Plan
			}
		}
		if samples, errSamples := a.suite.QuotaSamples(file.AuthIndex, now.Add(-forecastHistory)); errSamples == nil {
			for _, s := range samples {
				readings = append(readings, forecastReading{window: s.Window, seconds: s.WindowSeconds, used: s.UsedPercent, reset: s.ResetAt, at: s.At, source: s.Source})
			}
		}
		account.Windows, account.Coverage = a.forecastWindows(file.AuthIndex, category, readings, now)
		for i := range account.Windows {
			if sev, ok := quotaAlertSeverity(account.Windows[i], alertSettings); ok {
				account.Windows[i].AlertLevel = sev
			}
			account.Windows[i].Quiet = quotaQuiet(account.Windows[i], alertSettings)
		}
		account.Ledger, _ = a.suite.Ledger(file.AuthIndex, now.Add(-suite.LedgerRetention))
		if account.Ledger == nil {
			account.Ledger = []suite.StoredLedger{}
		}
		account.Monthly = a.forecastMonthly(file.AuthIndex, account.Ledger, now)
		if len(account.Ledger) > 20 {
			account.Ledger = account.Ledger[:20]
		}
		if len(account.Windows) == 0 {
			switch {
			case mode == suite.ModePassive:
				account.Note = "no_reading_passive"
			case account.Fetch != nil && !account.Fetch.OK:
				account.Note = "fetch_failed"
			default:
				account.Note = "no_reading"
			}
		}
		accounts = append(accounts, account)
	}
	sort.SliceStable(accounts, func(i, j int) bool {
		if accounts[i].Provider != accounts[j].Provider {
			return accounts[i].Provider < accounts[j].Provider
		}
		return strings.ToLower(accounts[i].Name) < strings.ToLower(accounts[j].Name)
	})
	out["accounts"] = accounts
	return out
}

// forecastHeaderReadings parses the quota headers upstream recorded for an account.
func (a *App) forecastHeaderReadings(authIndex, provider string, now time.Time) ([]forecastReading, string, time.Time) {
	var readings []forecastReading
	plan := ""
	var last time.Time
	_ = a.suite.ReadMain(func(db *sql.DB) error {
		rows, err := db.Query(`SELECT at, latency_ms, ttft_ms, response_headers_json FROM request_events
			WHERE auth_index = ? AND at >= ? AND response_headers_json != '{}' ORDER BY at`, authIndex, now.Add(-forecastHistory).UnixNano())
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var at, latency, ttft int64
			var raw string
			if rows.Scan(&at, &latency, &ttft, &raw) != nil {
				continue
			}
			var headers http.Header
			if json.Unmarshal([]byte(raw), &headers) != nil {
				continue
			}
			when := forecastObservedAt(at, latency, ttft)
			sample := parseQuotaHeaders(provider, headers, when)
			if sample.plan != "" {
				plan = codexPlanLabel(sample.plan)
			}
			for _, w := range sample.windows {
				if w.labelPrefix != "" || w.prefix != "X-Codex-" {
					continue // additional / code-review limits are not forecast
				}
				r := forecastReading{window: forecastWindowKey(w.seconds), seconds: w.seconds, used: w.usedPercent, at: when, source: "header"}
				if w.resetAt > 0 {
					r.reset = time.Unix(w.resetAt, 0).UTC()
				}
				readings = append(readings, r)
				last = when
			}
		}
		return rows.Err()
	})
	return readings, plan, last
}

// forecastObservedAt is when a header reading was true: request time + time to
// first byte (the headers arrive with the first byte), or the completion time
// when no TTFT was recorded. request_events.at is the completion time.
func forecastObservedAt(at, latencyMS, ttftMS int64) time.Time {
	done := time.Unix(0, at).UTC()
	if ttftMS <= 0 || latencyMS <= 0 || ttftMS > latencyMS {
		return done
	}
	return done.Add(-time.Duration(latencyMS-ttftMS) * time.Millisecond)
}

// forecastFetch calls upstream's official quota query once and stores the result.
func (a *App) forecastFetch(callbackID string, file hostAuthFile, provider string, now time.Time) suite.QuotaFetch {
	forecastFetchMu.Lock()
	defer forecastFetchMu.Unlock()
	record := suite.QuotaFetch{AuthIndex: file.AuthIndex, Provider: provider, At: now, Payload: "{}"}
	result, err := a.fetchAuthQuota(callbackID, file, provider)
	if err != nil {
		record.Error = err.Error()
		_ = a.suite.PutQuotaFetch(record)
		return record
	}
	record.OK = true
	var samples []suite.QuotaSample
	for _, row := range result.Quota {
		seconds := forecastLabelSeconds(row.Label)
		if row.LabelPrefix != "" || seconds == 0 || row.RemainingPercent == nil {
			continue
		}
		if row.Label == "Monthly allowance" && (row.Limit == nil || *row.Limit <= 0) {
			continue
		}
		s := suite.QuotaSample{AuthIndex: file.AuthIndex, Provider: provider, Window: forecastWindowKey(seconds), WindowSeconds: seconds,
			At: now, UsedPercent: 100 - *row.RemainingPercent, Source: "api"}
		if reset, errReset := time.Parse(time.RFC3339, row.ResetAt); errReset == nil {
			s.ResetAt = reset.UTC()
		}
		samples = append(samples, s)
	}
	if len(samples) == 0 {
		record.Error = "upstream returned no window with a usage percentage"
	}
	payload, _ := json.Marshal(map[string]any{"plan": result.Plan, "windows": len(samples)})
	record.Payload = string(payload)
	_ = a.suite.AddQuotaSamples(samples)
	_ = a.suite.PutQuotaFetch(record)
	return record
}

type forecastSpend struct {
	usd      float64
	requests int64
	unpriced int64
}

func (a *App) forecastSpend(authIndex string, from, to time.Time) forecastSpend {
	var s forecastSpend
	_ = a.suite.ReadMain(func(db *sql.DB) error {
		return db.QueryRow(`SELECT COALESCE(SUM(total_usd),0), COUNT(*), COALESCE(SUM(CASE WHEN price_source = 'none' AND failed = 0 THEN 1 ELSE 0 END),0)
			FROM request_events WHERE auth_index = ? AND at >= ? AND at < ?`, authIndex, from.UnixNano(), to.UnixNano()).Scan(&s.usd, &s.requests, &s.unpriced)
	})
	return s
}

func (a *App) forecastUSDPoints(authIndex string, from, to time.Time) []forecastPoint {
	span := to.Sub(from)
	if span <= 0 {
		return nil
	}
	bucket := span / 48
	if bucket < time.Minute {
		bucket = time.Minute
	}
	var points []forecastPoint
	_ = a.suite.ReadMain(func(db *sql.DB) error {
		rows, err := db.Query(`SELECT (at - ?) / ?, SUM(total_usd) FROM request_events WHERE auth_index = ? AND at >= ? AND at < ? GROUP BY 1 ORDER BY 1`,
			from.UnixNano(), int64(bucket), authIndex, from.UnixNano(), to.UnixNano())
		if err != nil {
			return err
		}
		defer rows.Close()
		total := 0.0
		points = append(points, forecastPoint{At: from, Value: 0})
		for rows.Next() {
			var index int64
			var usd float64
			if rows.Scan(&index, &usd) == nil {
				total += usd
				points = append(points, forecastPoint{At: from.Add(time.Duration(index+1) * bucket), Value: total})
			}
		}
		if last := points[len(points)-1]; last.At.Before(to) {
			points = append(points, forecastPoint{At: to, Value: total})
		}
		return rows.Err()
	})
	return points
}

// forecastSpendEvents loads the account's requests over the history span.
func (a *App) forecastSpendEvents(authIndex string, since time.Time) []suite.SpendEvent {
	var out []suite.SpendEvent
	_ = a.suite.ReadMain(func(db *sql.DB) error {
		rows, err := db.Query(`SELECT at, latency_ms, total_usd, uncached_input_tokens + cache_read_tokens + cache_write_tokens + billed_output_tokens,
			CASE WHEN price_source = 'none' AND failed = 0 THEN 1 ELSE 0 END FROM request_events WHERE auth_index = ? AND at >= ? ORDER BY at`, authIndex, since.UnixNano())
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var at, latency, tokens int64
			var usd float64
			var unpriced int
			if rows.Scan(&at, &latency, &usd, &tokens, &unpriced) == nil {
				done := time.Unix(0, at).UTC()
				out = append(out, suite.SpendEvent{At: done, Start: done.Add(-time.Duration(max(latency, 0)) * time.Millisecond), USD: usd, Tokens: tokens, Unpriced: unpriced == 1})
			}
		}
		return rows.Err()
	})
	return out
}

// forecastExclusions are the account's candy-test runs: their upstream usage
// is deliberately not in request_events, so milestone intervals and coverage
// checks that overlap them are skipped.
func (a *App) forecastExclusions(authIndex string, since time.Time) []suite.TimeRange {
	runs, _ := a.suite.CandyRuns(authIndex, since, 1000)
	out := make([]suite.TimeRange, 0, len(runs))
	for _, r := range runs {
		end := r.FinishedAt
		if end.IsZero() {
			end = time.Now().UTC()
		}
		out = append(out, suite.TimeRange{From: r.StartedAt.Add(-time.Minute), To: end.Add(2 * time.Minute)})
	}
	return out
}

func (a *App) forecastWindows(authIndex, provider string, readings []forecastReading, now time.Time) ([]forecastWindow, string) {
	byKey := map[string][]forecastReading{}
	order := []string{}
	for _, r := range readings {
		if _, ok := byKey[r.window]; !ok {
			order = append(order, r.window)
		}
		byKey[r.window] = append(byKey[r.window], r)
	}
	sort.SliceStable(order, func(i, j int) bool { return byKey[order[i]][0].seconds < byKey[order[j]][0].seconds })
	windows := []forecastWindow{}
	if len(order) == 0 {
		return windows, ""
	}
	since := now.Add(-forecastHistory)
	spend := a.forecastSpendEvents(authIndex, since)
	exclude := a.forecastExclusions(authIndex, since)
	coverage := ""
	for _, key := range order {
		list := byKey[key]
		seconds := list[len(list)-1].seconds
		obs := make([]suite.QuotaObs, 0, len(list))
		for _, r := range list {
			obs = append(obs, suite.QuotaObs{At: r.at, Used: r.used, ResetAt: r.reset, Source: r.source})
		}
		f := suite.Forecast6(suite.Forecast6Input{Provider: provider, Window: key, Seconds: seconds, Obs: obs, Spend: spend, Exclude: exclude, Now: now})
		_ = a.suite.PutLedger(authIndex, provider, f.Ledger)
		points := []forecastPoint{}
		for _, o := range f.CycleObs {
			points = append(points, forecastPoint{At: o.At, Value: o.Used})
		}
		requests := int64(0)
		for _, e := range spend {
			if !e.At.Before(f.Start) {
				requests++
			}
		}
		if coverage == "" || f.Coverage == suite.CoverageMixed || (f.Coverage == suite.CoverageCPAOnly && coverage == suite.CoverageUnknown) {
			coverage = f.Coverage
		}
		windows = append(windows, forecastWindow{Key: key, Seconds: seconds, Forecast6Result: f, Requests: requests,
			Percent: points, USD: a.forecastUSDPoints(authIndex, f.Start, now)})
	}
	return windows, coverage
}

// forecastMonthly sums the account's requests per calendar month in
// Asia/Shanghai (UTC+8, no DST) and joins the ledger's finished periods.
func (a *App) forecastMonthly(authIndex string, ledger []suite.StoredLedger, now time.Time) []forecastMonth {
	shanghai := time.FixedZone("Asia/Shanghai", 8*3600)
	byMonth := map[string]*forecastMonth{}
	var order []string
	get := func(m string) *forecastMonth {
		if byMonth[m] == nil {
			byMonth[m] = &forecastMonth{Month: m}
			order = append(order, m)
		}
		return byMonth[m]
	}
	from := time.Date(now.In(shanghai).Year(), now.In(shanghai).Month()-11, 1, 0, 0, 0, 0, shanghai)
	_ = a.suite.ReadMain(func(db *sql.DB) error {
		rows, err := db.Query(`SELECT strftime('%Y-%m', at / 1000000000 + 28800, 'unixepoch'), COALESCE(SUM(total_usd),0),
			COALESCE(SUM(uncached_input_tokens + cache_read_tokens + cache_write_tokens + billed_output_tokens),0), COUNT(*)
			FROM request_events WHERE auth_index = ? AND at >= ? GROUP BY 1 ORDER BY 1 DESC`, authIndex, from.UnixNano())
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var m string
			var usd float64
			var tokens, n int64
			if rows.Scan(&m, &usd, &tokens, &n) == nil {
				e := get(m)
				e.USD, e.Tokens, e.Requests = usd, tokens, n
			}
		}
		return rows.Err()
	})
	peaks := map[string]float64{}
	for _, l := range ledger {
		if l.End.Before(from) {
			continue
		}
		m := l.End.In(shanghai).Format("2006-01")
		e := get(m)
		e.Cycles++
		peaks[m] += l.PeakPercent
		if l.PeakPercent >= 100 {
			e.Exhausted++
		}
	}
	out := make([]forecastMonth, 0, len(order))
	for _, m := range order {
		e := byMonth[m]
		if e.Cycles > 0 {
			e.AvgPeak = peaks[m] / float64(e.Cycles)
		}
		out = append(out, *e)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Month > out[j].Month })
	return out
}
