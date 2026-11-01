package plugin

import (
	"encoding/json"
	"net/http"
	"sort"
	"strings"
	"time"

	"cpa-key-billing/internal/suite"
)

// passive degradation monitor: samples real traffic from usage.handle
// (numbers and status codes only) and compares the last 24h with the 7 days
// before, per account · model · reasoning effort · stream. Never sends a request.

const routeSuiteMonitor = "/suite/monitor"

// monitorUsage holds the usage.handle fields the monitor needs. Stream is part
// of the host's pluginapi.UsageRecord but not of upstream's UsageRecord type,
// so it is decoded here instead of patching upstream's struct.
type monitorUsage struct {
	UsageRecord
	Stream bool `json:"Stream"`
}

var monitorProviders = map[string]bool{"codex": true, "xai": true}

// suiteObserveUsage samples one usage record. Cheap: decode + in-memory append.
func (a *App) suiteObserveUsage(raw []byte) {
	if a == nil || a.suite == nil || a.suite.ModuleMode("grok_monitor") != suite.ModeOn {
		return
	}
	var r monitorUsage
	if json.Unmarshal(raw, &r) != nil {
		return
	}
	provider := strings.ToLower(strings.TrimSpace(r.Provider))
	if !monitorProviders[provider] || !r.Generate || strings.TrimSpace(r.APIKey) == "" || strings.TrimSpace(r.AuthIndex) == "" {
		return
	}
	at := r.RequestedAt
	if at.IsZero() {
		at = time.Now()
	}
	s := suite.MonitorSample{At: at.UTC(), AuthIndex: r.AuthIndex, Provider: provider, Model: strings.TrimSpace(r.Model),
		ResponseModel: strings.TrimSpace(r.ResponseModel), Effort: strings.ToLower(strings.TrimSpace(r.ReasoningEffort)), Stream: r.Stream,
		Failed: r.Failed, Input: r.Detail.InputTokens, Output: r.Detail.OutputTokens, Reasoning: r.Detail.ReasoningTokens,
		LatencyMS: r.Latency.Milliseconds(), TTFTMS: r.TTFT.Milliseconds()}
	if r.Failed {
		s.Status = r.Failure.StatusCode
		s.ErrKind = monitorErrKind(r.Failure.StatusCode, r.Failure.Body)
	}
	a.suite.ObserveSample(s)
}

// monitorErrKind classifies a failure. Only error-shaped refusals and stream
// disconnects are visible; usage.handle carries no response body or
// finish_reason, so a normal 200 refusal or max_output_tokens cut is invisible.
func monitorErrKind(status int, body string) string {
	b := strings.ToLower(body)
	switch {
	case strings.Contains(b, "stream disconnected") || strings.Contains(b, "stream error") || strings.Contains(b, "stream closed before"):
		return "truncated"
	case status == http.StatusTooManyRequests:
		return "rate_limit"
	case status == http.StatusUnauthorized || status == http.StatusForbidden && !strings.Contains(b, "policy"):
		return "auth"
	case status >= 400 && status < 500 && (strings.Contains(b, "content_policy") || strings.Contains(b, "content policy") || strings.Contains(b, "safety") || strings.Contains(b, "refus") || strings.Contains(b, "moderation")):
		return "refusal"
	case status >= 500:
		return "server"
	}
	return "other"
}

type monitorAccount struct {
	AuthIndex string                `json:"auth_index"`
	Provider  string                `json:"provider"`
	Name      string                `json:"name"`
	Verdict   string                `json:"verdict"`
	Severity  string                `json:"severity,omitempty"`
	Buckets   []suite.MonitorBucket `json:"buckets"`
	Series    []monitorPoint        `json:"series"`
}

type monitorPoint struct {
	At        time.Time `json:"at"`
	Requests  int       `json:"requests"`
	Reasoning float64   `json:"reasoning_median"`
	Output    float64   `json:"output_median"`
	ErrorRate float64   `json:"error_rate"`
}

func (a *App) monitorData(now time.Time) map[string]any {
	mode := a.suite.ModuleMode("grok_monitor")
	out := map[string]any{"mode": mode, "accounts": []monitorAccount{}, "generated_at": now,
		"thresholds": map[string]any{"recent_hours": 24, "baseline_days": 7, "min_samples": suite.MonitorMinSamples, "p_value": suite.MonitorPValue,
			"reasoning_drop": suite.MonitorReasoningDrop, "output_drop": suite.MonitorOutputDrop, "share_drop": suite.MonitorShareDrop,
			"soft_p_value": suite.MonitorSoftPValue, "cliff_delta": suite.MonitorCliffDelta, "segments_need": suite.MonitorSegmentsNeed, "segment_hours": int(suite.MonitorSegment.Hours()), "error_points": suite.MonitorErrorAbs, "mismatch_high": suite.MonitorMismatchHigh}}
	if mode != suite.ModeOn {
		return out
	}
	samples, err := a.suite.MonitorSamples(now.Add(-suite.MonitorRecent - suite.MonitorBaseline))
	if err != nil {
		out["error"] = err.Error()
		return out
	}
	names := map[string]string{}
	if files, err := a.listHostAuthFiles(); err == nil {
		for _, f := range files {
			names[f.AuthIndex] = f.Name
		}
	}
	byAccount := map[string]*monitorAccount{}
	candyFailed := map[string]bool{}
	for _, b := range suite.EvaluateMonitor(samples, now) {
		// a soft-only drop is "observe"; a failed candy test on the
		// account in the last 24 h confirms it (→ suspect, medium).
		if b.Verdict == "observe" && b.Deviating > b.Hard {
			failed, seen := candyFailed[b.AuthIndex]
			if !seen {
				if runs, err := a.suite.CandyRuns(b.AuthIndex, now.Add(-suite.MonitorRecent), 5); err == nil {
					for _, r := range runs {
						if r.Status == "done" {
							failed = !r.Passed
							break
						}
					}
				}
				candyFailed[b.AuthIndex] = failed
			}
			if failed {
				b.Verdict, b.Severity, b.CandyFailed = "suspect", "medium", true
			}
		}
		acc := byAccount[b.AuthIndex]
		if acc == nil {
			acc = &monitorAccount{AuthIndex: b.AuthIndex, Provider: b.Provider, Name: names[b.AuthIndex], Verdict: "insufficient"}
			byAccount[b.AuthIndex] = acc
		}
		acc.Buckets = append(acc.Buckets, b)
		switch {
		case b.Verdict == "suspect" && (acc.Verdict != "suspect" || b.Severity == "high"):
			acc.Verdict, acc.Severity = "suspect", b.Severity
		case b.Verdict == "observe" && acc.Verdict != "suspect":
			acc.Verdict = "observe"
		case b.Verdict == "ok" && acc.Verdict == "insufficient":
			acc.Verdict = "ok"
		}
	}
	accounts := make([]monitorAccount, 0, len(byAccount))
	for _, acc := range byAccount {
		acc.Series = monitorSeries(samples, acc.AuthIndex, now)
		accounts = append(accounts, *acc)
	}
	sort.Slice(accounts, func(i, j int) bool {
		if accounts[i].Provider != accounts[j].Provider {
			return accounts[i].Provider > accounts[j].Provider // xai first, then codex
		}
		return accounts[i].Name < accounts[j].Name
	})
	out["accounts"] = accounts
	return out
}

// monitorSeries: 6-hour buckets over the 8 days for the sparkline charts.
func monitorSeries(samples []suite.MonitorSample, authIndex string, now time.Time) []monitorPoint {
	const step = 6 * time.Hour
	start := now.Add(-suite.MonitorRecent - suite.MonitorBaseline).Truncate(step)
	n := int(now.Sub(start)/step) + 1
	type acc struct {
		reasoning, output []float64
		failed, total     int
	}
	buckets := make([]acc, n)
	for _, s := range samples {
		if s.AuthIndex != authIndex || s.At.Before(start) {
			continue
		}
		i := int(s.At.Sub(start) / step)
		if i < 0 || i >= n {
			continue
		}
		buckets[i].total++
		if s.Failed {
			buckets[i].failed++
			continue
		}
		buckets[i].reasoning = append(buckets[i].reasoning, float64(s.Reasoning))
		buckets[i].output = append(buckets[i].output, float64(s.Output))
	}
	out := make([]monitorPoint, 0, n)
	for i, b := range buckets {
		p := monitorPoint{At: start.Add(time.Duration(i) * step), Requests: b.total, Reasoning: suite.Median(b.reasoning), Output: suite.Median(b.output)}
		if b.total > 0 {
			p.ErrorRate = float64(b.failed) / float64(b.total)
		}
		out = append(out, p)
	}
	return out
}

func (a *App) suiteMonitor(ManagementRequest) ManagementResponse {
	return JSONResponse(http.StatusOK, a.monitorData(time.Now().UTC()))
}
