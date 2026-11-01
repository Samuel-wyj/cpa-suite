package plugin

// self-service usage for API key holders.
//
// GET /v0/resource/plugins/cpa-suite/usage (Authorization: Bearer <client key>)
// returns the caller's own plan in cc-switch's usage-query format
// ({success, data: [UsageData…]}) plus, unless the shared_quota module is off,
// the cached 5h / weekly remaining % of the upstream accounts this key's
// routing rules let it reach. It never queries an upstream, never returns
// another key's data, e-mails or tokens, and is rate limited.

import (
	"fmt"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"

	"cpa-key-billing/internal/billing"
	"cpa-key-billing/internal/suite"
)

const (
	routeUsage          = "/usage"
	usageCacheTTL       = 30 * time.Second // per key
	usageForecastTTL    = 60 * time.Second // shared upstream snapshot
	badKeyWindow        = time.Minute
	badKeyLimit         = 10 // failed key checks per client address and window
	badKeySharedLimit   = 60 // when CPA forwards no client address all callers share one bucket
	sharedQuotaModuleID = "shared_quota"
)

var usageTZ = time.FixedZone("UTC+8", 8*3600)

// UsageData mirrors cc-switch's usage-query result object.
type UsageData struct {
	PlanName       string   `json:"planName,omitempty"`
	Extra          string   `json:"extra,omitempty"`
	IsValid        *bool    `json:"isValid,omitempty"`
	InvalidMessage string   `json:"invalidMessage,omitempty"`
	Total          *float64 `json:"total,omitempty"`
	Used           *float64 `json:"used,omitempty"`
	Remaining      *float64 `json:"remaining,omitempty"`
	Unit           string   `json:"unit,omitempty"`
	// Not read by cc-switch; used by the self-service page.
	Kind    string    `json:"kind"` // plan | upstream
	ResetAt time.Time `json:"reset_at,omitzero"`
}

type usageResponse struct {
	Success     bool        `json:"success"`
	Data        []UsageData `json:"data"`
	Shared      bool        `json:"shared_quota"`
	GeneratedAt time.Time   `json:"generated_at"`
	CachedFor   int         `json:"cache_seconds"`
}

type usageLimiter struct {
	mu       sync.Mutex
	cache    map[string]usageCacheEntry
	bad      map[string][]time.Time
	snapshot []forecastAccount
	files    []hostAuthFile
	snapAt   time.Time
}

type usageCacheEntry struct {
	at   time.Time
	resp ManagementResponse
}

var selfUsage = &usageLimiter{cache: map[string]usageCacheEntry{}, bad: map[string][]time.Time{}}

// clientAddress is the best per-client key CPA gives a resource handler (it
// forwards headers only). Without a proxy header all clients share "-".
func clientAddress(h http.Header) string {
	if v := strings.TrimSpace(strings.Split(h.Get("X-Forwarded-For"), ",")[0]); v != "" {
		return v
	}
	if v := strings.TrimSpace(h.Get("X-Real-Ip")); v != "" {
		return v
	}
	return "-"
}

// blocked reports whether this address exceeded the failed-key budget.
func (l *usageLimiter) blocked(addr string, now time.Time) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	kept := l.bad[addr][:0]
	for _, t := range l.bad[addr] {
		if now.Sub(t) < badKeyWindow {
			kept = append(kept, t)
		}
	}
	l.bad[addr] = kept
	limit := badKeyLimit
	if addr == "-" {
		limit = badKeySharedLimit
	}
	return len(kept) >= limit
}

func (l *usageLimiter) fail(addr string, now time.Time) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.bad[addr] = append(l.bad[addr], now)
	if len(l.bad) > 4096 { // bound memory
		for k := range l.bad {
			delete(l.bad, k)
			break
		}
	}
}

func tooManyAttempts() ManagementResponse {
	r := apiKeyJSONError(http.StatusTooManyRequests, "rate_limited", "Too many invalid API key attempts; try again in a minute")
	r.Headers.Set("Retry-After", "60")
	return r
}

func fptr(v float64) *float64 { return &v }
func bptr(v bool) *bool       { return &v }

func usageWhen(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	return t.In(usageTZ).Format("01/02 15:04")
}

// planUsage turns the caller's own plan into cc-switch rows.
func planUsage(name string, q billing.QuotaView) []UsageData {
	if name == "" {
		name = "套餐"
	}
	if q.Unlimited {
		return []UsageData{{PlanName: name, Total: fptr(-1), IsValid: bptr(true), Extra: "不限额", Kind: "plan"}}
	}
	unit := map[billing.QuotaMetric]string{billing.QuotaAmount: "USD", billing.QuotaTokens: "tokens", billing.QuotaRequests: "次"}
	label := map[billing.QuotaMetric]string{billing.QuotaAmount: "额度", billing.QuotaTokens: "Tokens", billing.QuotaRequests: "请求"}
	out := []UsageData{}
	for _, w := range q.Windows {
		for _, d := range w.Dimensions {
			limit, _ := d.Limit.Float64()
			used, _ := d.Used.Float64()
			rem, _ := d.Remaining.Float64()
			extra := "窗口未开始（首个请求后开始计时）"
			if w.Started {
				extra = "重置 " + usageWhen(w.EndAt)
			}
			blocked := d.Blocked || w.Blocked || q.Blocked
			row := UsageData{PlanName: strings.TrimSpace(name + " · " + w.Name + " " + label[d.Metric]),
				Total: fptr(limit), Used: fptr(used), Remaining: fptr(rem), Unit: unit[d.Metric],
				IsValid: bptr(!blocked), Extra: extra, Kind: "plan"}
			if w.Started {
				row.ResetAt = w.EndAt
			}
			if blocked {
				row.InvalidMessage = "已用完，" + extra
			}
			out = append(out, row)
		}
	}
	if len(out) == 0 {
		out = append(out, UsageData{PlanName: name, IsValid: bptr(true), Extra: "没有额度窗口", Kind: "plan"})
	}
	return out
}

// modelProvider guesses which upstream family serves a model id; "" = unknown.
func modelProvider(model string) string {
	m := strings.ToLower(strings.TrimSpace(model))
	switch {
	case strings.Contains(m, "grok"):
		return "xai"
	case strings.HasPrefix(m, "gpt") || strings.Contains(m, "codex") || (len(m) > 1 && m[0] == 'o' && m[1] >= '0' && m[1] <= '9'):
		return "codex"
	}
	return ""
}

// providerReachableByModels: a key limited to an explicit model list only
// reaches the families those models belong to (unknown ids keep every family).
func providerReachableByModels(d billing.RoutingDecision, provider string) bool {
	if len(d.Models) == 0 {
		return true
	}
	for _, m := range d.Models {
		if p := modelProvider(m); p == "" || p == provider {
			return true
		}
	}
	return false
}

// sharedQuotaRows lists the cached 5h / weekly remaining % of every upstream
// account the routing decision lets this key reach. Names are masked.
func sharedQuotaRows(accounts []forecastAccount, files []hostAuthFile, d billing.RoutingDecision, now time.Time) []UsageData {
	if d.ConfigurationError != "" {
		return nil
	}
	byIndex := map[string]hostAuthFile{}
	for _, f := range files {
		byIndex[f.AuthIndex] = f
	}
	title := map[string]string{"codex": "Codex", "xai": "Grok"}
	count := map[string]int{}
	type entry struct {
		acc  forecastAccount
		file hostAuthFile
	}
	var allowed []entry
	for _, acc := range accounts {
		f, ok := byIndex[acc.AuthIndex]
		if !ok || f.Disabled {
			continue
		}
		source := credentialSourceFromHost(f)
		provider := strings.ToLower(strings.TrimSpace(f.Provider))
		if provider == "" {
			provider = strings.ToLower(strings.TrimSpace(f.Type))
		}
		if !d.AllowsCredential(billing.CredentialFingerprint(strings.TrimSpace(f.ID)), source, provider) || !providerReachableByModels(d, acc.Provider) {
			continue
		}
		allowed = append(allowed, entry{acc, f})
		count[acc.Provider]++
	}
	sort.SliceStable(allowed, func(i, j int) bool { return allowed[i].acc.Provider < allowed[j].acc.Provider })
	seq := map[string]int{}
	var out []UsageData
	for _, e := range allowed {
		seq[e.acc.Provider]++
		name := title[e.acc.Provider]
		if name == "" {
			name = e.acc.Provider
		}
		if p := strings.TrimSpace(e.acc.Plan); p != "" {
			name += " " + p
		}
		if count[e.acc.Provider] > 1 {
			name += fmt.Sprintf(" #%d", seq[e.acc.Provider])
		}
		for _, w := range e.acc.Windows {
			if w.RemainingPercent == nil {
				continue
			}
			label := map[string]string{"5h": "5 小时", "weekly": "周"}[w.Key]
			if label == "" {
				label = w.Key
			}
			rem := *w.RemainingPercent
			extra := "重置 " + usageWhen(w.ResetAt)
			if !w.ObservedAt.IsZero() {
				extra += fmt.Sprintf(" · %d 分钟前读数", int(now.Sub(w.ObservedAt).Minutes()))
			}
			if w.Stale {
				extra += "（已过期）"
			}
			row := UsageData{PlanName: "共享 " + name + " · " + label, Total: fptr(100), Used: fptr(100 - rem), Remaining: fptr(rem),
				Unit: "%", IsValid: bptr(rem > 0), Extra: extra, Kind: "upstream", ResetAt: w.ResetAt}
			if rem <= 0 {
				row.InvalidMessage = "上游额度已用完，" + extra
			}
			out = append(out, row)
		}
	}
	return out
}

func (a *App) usageSnapshot(now time.Time) ([]forecastAccount, []hostAuthFile) {
	selfUsage.mu.Lock()
	if now.Sub(selfUsage.snapAt) < usageForecastTTL && selfUsage.snapshot != nil {
		acc, files := selfUsage.snapshot, selfUsage.files
		selfUsage.mu.Unlock()
		return acc, files
	}
	selfUsage.mu.Unlock()
	data := a.forecastData("", "", false) // cached readings only, never fetches
	accounts, _ := data["accounts"].([]forecastAccount)
	files, _ := a.listHostAuthFiles()
	selfUsage.mu.Lock()
	selfUsage.snapshot, selfUsage.files, selfUsage.snapAt = append([]forecastAccount{}, accounts...), files, now
	selfUsage.mu.Unlock()
	return accounts, files
}

func (a *App) accountUsage(req ManagementRequest, access viewAccess) ManagementResponse {
	if !access.Tracked {
		return apiKeyUnauthorized()
	}
	now := time.Now().UTC()
	shared := a.suite != nil && a.suite.ModuleMode(sharedQuotaModuleID) == suite.ModeOn && req.Query.Get("upstream") != "0"
	key := fmt.Sprintf("%s|%v", access.Scope, shared)
	selfUsage.mu.Lock()
	if c, ok := selfUsage.cache[key]; ok && now.Sub(c.at) < usageCacheTTL {
		selfUsage.mu.Unlock()
		return c.resp
	}
	selfUsage.mu.Unlock()
	view := access.Key
	data := planUsage(view.PlanName, view.QuotaView)
	if shared {
		accounts, files := a.usageSnapshot(now)
		data = append(data, sharedQuotaRows(accounts, files, a.store.ResolveRouting(access.Scope, "", ""), now)...)
	}
	resp := apiKeyJSON(http.StatusOK, usageResponse{Success: true, Data: data, Shared: shared, GeneratedAt: now, CachedFor: int(usageCacheTTL.Seconds())})
	resp.Headers.Set("Cache-Control", fmt.Sprintf("private, max-age=%d", int(usageCacheTTL.Seconds())))
	selfUsage.mu.Lock()
	if len(selfUsage.cache) > 1024 {
		selfUsage.cache = map[string]usageCacheEntry{}
	}
	selfUsage.cache[key] = usageCacheEntry{at: now, resp: resp}
	selfUsage.mu.Unlock()
	return resp
}
