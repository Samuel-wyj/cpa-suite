package clientguard

// Ported from client-guard 0.1.2 handlers.go/types.go (same author, same
// behaviour): request interception, the 403 body and config handling. The
// standalone plugin's ABI/management plumbing is replaced by the CPA Suite host.

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"

	"gopkg.in/yaml.v3"
)

const (
	pluginID = "cpa-suite"
	// SourceVersion is the client-guard release this module was ported from.
	SourceVersion = "0.1.2"
	// StandalonePluginID is the plugin this module replaces.
	StandalonePluginID = "client-guard"
)

var pluginVersion = SourceVersion + "-suite"

type envelope struct {
	OK     bool            `json:"ok"`
	Result json.RawMessage `json:"result,omitempty"`
}

type requestInterceptRequest struct {
	RequestID      string         `json:"RequestID"`
	SourceFormat   string         `json:"SourceFormat"`
	Model          string         `json:"Model"`
	RequestedModel string         `json:"RequestedModel"`
	Headers        http.Header    `json:"Headers"`
	Metadata       map[string]any `json:"Metadata"`
}

type requestInterceptResponse struct {
	Terminate       bool        `json:"Terminate,omitempty"`
	StatusCode      int         `json:"StatusCode,omitempty"`
	ResponseHeaders http.Header `json:"ResponseHeaders,omitempty"`
	ResponseBody    []byte      `json:"ResponseBody,omitempty"`
}

// Guard is one client-guard instance owned by CPA Suite.
type Guard struct{ g *guardState }

// New creates a guard persisting through backend (nil = client-guard files).
func New(backend Backend) *Guard {
	g := newGuardState()
	g.backend = backend
	return &Guard{g: g}
}

// Configure mirrors client-guard's plugin.register: a bad config falls back to
// defaults (log-only) and is reported, never blocking CPA.
func (x *Guard) Configure(yamlConfig []byte, hostSchema uint32) {
	g := x.g
	cfg, err := parseConfig(yamlConfig)
	if err != nil {
		warnf("%v；已使用默认配置", err)
		cfg, _ = parseConfig(nil)
		g.configure(cfg, hostSchema)
		g.mu.Lock()
		g.lastError = err.Error() + "（已使用默认配置）"
		g.mu.Unlock()
		return
	}
	g.configure(cfg, hostSchema)
}

// ValidateConfig reports whether yamlConfig parses as client-guard config.
func ValidateConfig(yamlConfig []byte) error { _, err := parseConfig(yamlConfig); return err }

// Options alter enforcement without touching the rules.
type Options struct {
	// Observe records decisions but never blocks (module mode "observe").
	Observe bool
}

// InterceptBefore is client-guard's request.intercept_before. It returns the
// plugin envelope and whether the request is terminated (403).
func (x *Guard) InterceptBefore(raw []byte, opts Options) (out []byte, terminated bool) {
	out, err := x.interceptRequest(raw, opts)
	if err != nil {
		out, _ = x.internalFailure(err)
	}
	var env envelope
	var resp requestInterceptResponse
	if json.Unmarshal(out, &env) == nil && json.Unmarshal(env.Result, &resp) == nil {
		terminated = resp.Terminate
	}
	return out, terminated
}

func (x *Guard) interceptRequest(raw []byte, opts Options) (out []byte, err error) {
	g := x.g
	defer func() {
		if r := recover(); r != nil {
			out, err = x.internalFailure(fmt.Errorf("panic: %v", r))
		}
	}()
	var req requestInterceptRequest
	if err := decodeJSON(raw, &req); err != nil {
		return x.internalFailure(err)
	}
	scope := anonymousScope
	if s, ok := req.Metadata["caller_scope"].(string); ok {
		s = strings.ToLower(strings.TrimSpace(s))
		if validScope(s) {
			scope = s
		}
	}
	ch := extractHeaders(req.Headers, g.headerNames())
	ua, originator := ch.UA, ch.Originator
	d, cfg := g.decideHeaders(scope, ch)
	if d.Mode == ModeOff {
		return okEnvelope(requestInterceptResponse{})
	}
	if opts.Observe && d.Block {
		d.Block = false
	}
	model := strings.TrimSpace(req.RequestedModel)
	if model == "" {
		model = strings.TrimSpace(req.Model)
	}
	path, _ := req.Metadata["request_path"].(string)
	result := "allowed"
	switch {
	case d.Block:
		result = "blocked"
	case d.EmptyAllow:
		result = "unchecked"
	case !d.Allowed:
		result = "not-in-list"
	}
	g.record(LogEntry{
		Time: g.now(), Scope: scope, UA: ua, Originator: originator,
		XTitle: ch.XTitle, HTTPReferer: ch.HTTPReferer, Referer: ch.Referer, XClientName: ch.XClientName, Custom: ch.Custom,
		Preset: presetID(ch), Suggest: suggestPattern(ch),
		Client: clientLabelHeaders(ch), Path: path, Model: model, Format: req.SourceFormat,
		Mode: d.Mode, Matched: d.Matched, Result: result,
	}, d.Block)
	if d.Block {
		return blockedResponse(cfg.BlockMessage, ua, originator)
	}
	return okEnvelope(requestInterceptResponse{})
}

func (x *Guard) internalFailure(err error) ([]byte, error) {
	g := x.g
	g.mu.RLock()
	failOpen := g.cfg.FailOpen || !g.configured
	g.mu.RUnlock()
	warnf("内部错误: %v（fail_open=%v）", err, failOpen)
	if failOpen {
		return okEnvelope(requestInterceptResponse{})
	}
	body, _ := json.Marshal(map[string]any{"error": map[string]any{
		"type": "client_guard_unavailable", "message": "client-guard 插件内部错误，按 fail_open=false 拒绝请求",
	}})
	return okEnvelope(requestInterceptResponse{
		Terminate: true, StatusCode: http.StatusForbidden,
		ResponseHeaders: http.Header{"Content-Type": []string{"application/json; charset=utf-8"}},
		ResponseBody:    body,
	})
}

func blockedResponse(message, ua, originator string) ([]byte, error) {
	detail := map[string]any{
		"type":       "client_not_allowed",
		"code":       "client_not_allowed",
		"message":    message,
		"user_agent": ua,
	}
	if originator != "" {
		detail["originator"] = originator
	}
	body, err := json.Marshal(map[string]any{"error": detail})
	if err != nil {
		return nil, err
	}
	return okEnvelope(requestInterceptResponse{
		Terminate:       true,
		StatusCode:      http.StatusForbidden,
		ResponseHeaders: http.Header{"Content-Type": []string{"application/json; charset=utf-8"}},
		ResponseBody:    body,
	})
}

func okEnvelope(value any) ([]byte, error) {
	raw, err := json.Marshal(value)
	if err != nil {
		return nil, err
	}
	return json.Marshal(envelope{OK: true, Result: raw})
}

func decodeJSON(raw []byte, target any) error {
	if len(raw) == 0 {
		return fmt.Errorf("request body is required")
	}
	if err := json.Unmarshal(raw, target); err != nil {
		return fmt.Errorf("decode request: %w", err)
	}
	return nil
}

func presetID(h ClientHeaders) string {
	if p := presetMatch(h); p != nil {
		return p.ID
	}
	return ""
}

// State is the management view (client-guard's GET /state).
type State = snapshotView

// Snapshot returns rules, stats, recent requests and presets.
func (x *Guard) Snapshot() State { return x.g.snapshot() }

// SaveRules validates and persists a rule set (client-guard's PUT /rules).
func (x *Guard) SaveRules(rs RuleSet) (RuleSet, State, error) {
	if rs.Keys == nil {
		rs.Keys = map[string]Rule{}
	}
	saved, err := x.g.saveRules(rs)
	return saved, x.g.snapshot(), err
}

// ClearStats empties statistics and recent requests (client-guard's POST /clear-stats).
func (x *Guard) ClearStats() { x.g.clearStats(); x.g.flush() }

// Flush writes pending statistics.
func (x *Guard) Flush() { x.g.flush() }

// Close flushes and stops the background writer.
func (x *Guard) Close() { x.g.shutdown() }

// LoadDir reads client-guard 0.1.2 files read-only and returns the normalized
// rules plus the raw stats document. Missing files return ok=false.
func LoadDir(dir string) (rules *RuleSet, statsRaw []byte, err error) {
	b := DirBackend{Dir: dir}
	if raw, errRead := b.ReadRules(); errRead == nil {
		var rs RuleSet
		if err := json.Unmarshal(raw, &rs); err != nil {
			return nil, nil, fmt.Errorf("rules.json 解析失败: %w", err)
		}
		norm, err := NormalizeRuleSet(rs)
		if err != nil {
			return nil, nil, fmt.Errorf("rules.json 校验失败: %w", err)
		}
		rules = &norm
	} else if !isNotExist(errRead) {
		return nil, nil, errRead
	}
	if raw, errRead := b.ReadStats(); errRead == nil {
		var s stats
		if err := json.Unmarshal(raw, &s); err != nil {
			return nil, nil, fmt.Errorf("stats.json 解析失败: %w", err)
		}
		statsRaw = raw
	} else if !isNotExist(errRead) {
		return nil, nil, errRead
	}
	return rules, statsRaw, nil
}

// ConfiguredDataDir returns the data_dir written in a client-guard config
// block ("" when unset), used as the default import source.
func ConfiguredDataDir(yamlConfig []byte) string {
	var pc pluginConfig
	if yaml.Unmarshal(yamlConfig, &pc) != nil {
		return ""
	}
	return strings.TrimSpace(pc.DataDir)
}

// DailyCounts returns a copy of the per-day counters (CPA Suite: ranking
// anomalies read block spikes from it).
func (x *Guard) DailyCounts() map[string]DayCount {
	x.g.mu.RLock()
	defer x.g.mu.RUnlock()
	out := make(map[string]DayCount, len(x.g.stats.Daily))
	for day, d := range x.g.stats.Daily {
		if d != nil {
			out[day] = *d
		}
	}
	return out
}
