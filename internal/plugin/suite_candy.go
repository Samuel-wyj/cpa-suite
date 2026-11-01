package plugin

// CPA Suite candy test (0.6, module candy_test, OFF by default).
//
// A manual, confirmed, rate-limited single request to ONE selected upstream
// account through the host model executor (host.model.execute), asking a
// fixed puzzle whose correct answer is 21. It records pass/fail, reasoning
// tokens, latency and the model the upstream reported.
//
// The puzzle prompt, the "standalone 21" check and the Codex CLI context
// (candy/codex_context.json) come from cpa-codex-candy-eval v0.3.15
// (https://github.com/haowang02/cpa-plugin-codex-candy-eval), MIT License,
// Copyright (c) 2026 Hao Wang — see candy/NOTICE.md and
// candy/LICENSE-cpa-codex-candy-eval. The request builder below is a
// simplified re-implementation. Nothing here reads or calls that plugin.
//
// Accounting: the host delivers the test's usage record to usage.handle with
// an empty API key. While a run is pending for that account, suiteGate hands
// the record to candyCaptureUsage, which keeps its metrics for the run and
// answers the hook itself, so upstream billing never writes a request event
// (no key, ranking, usage statistics or passive-monitor entry). The forecast
// skips milestone intervals that overlap a run (forecastExclusions),
// because that upstream usage is not in request_events.

import (
	"bytes"
	"crypto/rand"
	"database/sql"
	_ "embed"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"

	"cpa-key-billing/internal/suite"
)

const (
	routeSuiteCandy    = "/suite/candy"
	routeSuiteCandyRun = "/suite/candy/run"
	candyInterval      = 10 * time.Minute
	candyTimeout       = 10 * time.Minute
	candyUsageGrace    = 60 * time.Second
	candyCodexTokens   = 14000
	candyGrokTokens    = 300
	hostModelExecute   = "host.model.execute"
)

// From cpa-codex-candy-eval (MIT, Copyright (c) 2026 Hao Wang).
const candyPrompt = `不使用任何外部工具回答以下问题：

在一个黑色的袋子里放有三种口味的糖果，每种糖果有两种不同的形状（圆形和五角星形，不同的形状靠手感可以分辨）。现已知不同口味的糖和不同形状的数量统计如下表。参赛者需要在活动前决定摸出的糖果数目，那么，最少取出多少个糖果才能保证手中同时拥有不同形状的苹果味和桃子味的糖？（同时手中有圆形苹果味匹配五角星桃子味糖果，或者有圆形桃子味匹配五角星苹果味糖果都满足要求）

        苹果味  桃子味  西瓜味
圆形       7      9      8
五角星形   7      6      4
`

//go:embed candy/codex_context.json
var candyCodexContextJSON []byte

var candyEfforts = map[string]bool{"": true, "low": true, "medium": true, "high": true, "xhigh": true}

type candyPending struct {
	runID    int64
	provider string
	usage    *monitorUsage
	done     bool
}

var (
	candyMu      sync.Mutex
	candyActive  = map[string]*candyPending{} // auth_index → pending run (incl. grace after finish)
	candyNow     = time.Now
	candyLimiter = map[string]time.Time{} // auth_index → last start (also persisted as candy_runs)
)

// candyStandalone21 reports whether "21" appears without adjacent digits
// (cpa-codex-candy-eval's check).
func candyStandalone21(text string) bool {
	isDigit := func(i int) bool { return i >= 0 && i < len(text) && text[i] >= '0' && text[i] <= '9' }
	for i := 0; i+1 < len(text); i++ {
		if text[i] == '2' && text[i+1] == '1' && !isDigit(i-1) && !isDigit(i+2) {
			return true
		}
	}
	return false
}

func candyUUID() string {
	var b [16]byte
	_, _ = rand.Read(b[:])
	b[6] = b[6]&0x0f | 0x40
	b[8] = b[8]&0x3f | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[:4], b[4:6], b[6:8], b[8:10], b[10:])
}

// candyCodexBody builds a Responses request shaped like the first turn of a
// Codex CLI thread (tools + base instructions + context + the puzzle).
func candyCodexBody(model, effort string) ([]byte, http.Header, error) {
	var ctx struct {
		Tools            json.RawMessage `json:"tools"`
		BaseInstructions string          `json:"base_instructions"`
		Messages         []struct {
			Role    string `json:"role"`
			Content []struct {
				Text string `json:"text"`
			} `json:"content"`
		} `json:"messages"`
	}
	if err := json.Unmarshal(candyCodexContextJSON, &ctx); err != nil {
		return nil, nil, err
	}
	type part struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	type item struct {
		Type    string `json:"type"`
		Role    string `json:"role"`
		Content []part `json:"content"`
	}
	input := []item{}
	for _, m := range ctx.Messages {
		it := item{Type: "message", Role: m.Role}
		for _, c := range m.Content {
			it.Content = append(it.Content, part{"input_text", c.Text})
		}
		input = append(input, it)
	}
	input = append(input, item{Type: "message", Role: "user", Content: []part{{"input_text", candyPrompt}}})
	thread := candyUUID()
	body := map[string]any{
		"model": model, "instructions": ctx.BaseInstructions, "input": input, "tools": json.RawMessage(ctx.Tools),
		"tool_choice": "auto", "parallel_tool_calls": false, "store": false, "stream": false,
		"include": []string{"reasoning.encrypted_content"}, "prompt_cache_key": thread,
	}
	if effort != "" {
		body["reasoning"] = map[string]any{"effort": effort, "summary": "auto"}
	}
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(body); err != nil {
		return nil, nil, err
	}
	headers := http.Header{"Content-Type": {"application/json"}, "Originator": {"codex_cli_rs"}, "Session-Id": {thread}}
	return bytes.TrimSpace(buf.Bytes()), headers, nil
}

func candyGrokBody(model, effort string) ([]byte, http.Header, error) {
	body := map[string]any{"model": model, "stream": false,
		"input": []map[string]any{{"role": "user", "content": candyPrompt}}}
	if effort != "" {
		body["reasoning"] = map[string]any{"effort": effort}
	}
	raw, err := json.Marshal(body)
	return raw, http.Header{"Content-Type": {"application/json"}}, err
}

type candyAccount struct {
	AuthIndex    string           `json:"auth_index"`
	Provider     string           `json:"provider"`
	Name         string           `json:"name"`
	DefaultModel string           `json:"default_model"`
	CostTokens   int              `json:"cost_input_tokens"`
	NextAllowed  *time.Time       `json:"next_allowed_at,omitempty"`
	Running      bool             `json:"running"`
	Runs         []suite.CandyRun `json:"runs"`
}

func (a *App) candyAccounts() ([]hostAuthFile, error) {
	files, err := a.listHostAuthFiles()
	if err != nil {
		return nil, err
	}
	out := []hostAuthFile{}
	for _, f := range files {
		c := authCategory(f.Type)
		if (c == "codex" || c == "xai") && strings.TrimSpace(f.AuthIndex) != "" && strings.TrimSpace(f.ID) != "" && !f.Disabled {
			out = append(out, f)
		}
	}
	return out, nil
}

// candyDefaultModel is the model this account served most in the last week.
func (a *App) candyDefaultModel(authIndex, provider string) string {
	model := ""
	_ = a.suite.ReadMain(func(db *sql.DB) error {
		return db.QueryRow(`SELECT upstream_model FROM request_events WHERE auth_index = ? AND at >= ? AND upstream_model != ''
			GROUP BY upstream_model ORDER BY COUNT(*) DESC LIMIT 1`, authIndex, time.Now().Add(-7*24*time.Hour).UnixNano()).Scan(&model)
	})
	if model != "" {
		return model
	}
	if provider == "xai" {
		return "grok-4.3"
	}
	return "gpt-5.5"
}

func (a *App) candyNextAllowed(authIndex string) *time.Time {
	candyMu.Lock()
	last := candyLimiter[authIndex]
	candyMu.Unlock()
	if runs, _ := a.suite.CandyRuns(authIndex, time.Now().Add(-candyInterval), 1); len(runs) > 0 && runs[0].StartedAt.After(last) {
		last = runs[0].StartedAt
	}
	if last.IsZero() || candyNow().Sub(last) >= candyInterval {
		return nil
	}
	next := last.Add(candyInterval)
	return &next
}

func (a *App) suiteCandy(req ManagementRequest) ManagementResponse {
	mode := a.suite.ModuleMode("candy_test")
	out := map[string]any{"mode": mode, "accounts": []candyAccount{}, "interval_minutes": int(candyInterval.Minutes()),
		"cost": map[string]int{"codex": candyCodexTokens, "xai": candyGrokTokens}}
	if mode != suite.ModeOn {
		return JSONResponse(http.StatusOK, out)
	}
	_ = a.suite.InterruptCandyRuns(time.Now().Add(-candyTimeout - time.Minute))
	files, err := a.candyAccounts()
	if err != nil {
		out["error"] = err.Error()
		return JSONResponse(http.StatusOK, out)
	}
	accounts := []candyAccount{}
	for _, f := range files {
		c := authCategory(f.Type)
		acc := candyAccount{AuthIndex: f.AuthIndex, Provider: c, Name: f.Name, DefaultModel: a.candyDefaultModel(f.AuthIndex, c),
			CostTokens: map[string]int{"codex": candyCodexTokens, "xai": candyGrokTokens}[c], NextAllowed: a.candyNextAllowed(f.AuthIndex)}
		candyMu.Lock()
		if p := candyActive[f.AuthIndex]; p != nil && !p.done {
			acc.Running = true
		}
		candyMu.Unlock()
		acc.Runs, _ = a.suite.CandyRuns(f.AuthIndex, time.Time{}, 10)
		if acc.Runs == nil {
			acc.Runs = []suite.CandyRun{}
		}
		accounts = append(accounts, acc)
	}
	out["accounts"] = accounts
	return JSONResponse(http.StatusOK, out)
}

func (a *App) suiteCandyRun(req ManagementRequest) ManagementResponse {
	if a.suite.ModuleMode("candy_test") != suite.ModeOn {
		return JSONError(http.StatusConflict, "candy_off", "The candy test module is off")
	}
	var body struct {
		AuthIndex string `json:"auth_index"`
		Model     string `json:"model"`
		Effort    string `json:"effort"`
		Confirm   bool   `json:"confirm"`
	}
	if err := json.Unmarshal(req.Body, &body); err != nil {
		return JSONError(http.StatusBadRequest, "invalid_json", err.Error())
	}
	body.AuthIndex, body.Model, body.Effort = strings.TrimSpace(body.AuthIndex), strings.TrimSpace(body.Model), strings.ToLower(strings.TrimSpace(body.Effort))
	if !body.Confirm {
		return JSONError(http.StatusBadRequest, "confirm_required", "Each run must be confirmed (it spends upstream quota)")
	}
	if !candyEfforts[body.Effort] || len(body.Model) > 120 || strings.ContainsAny(body.Model, " \t\r\n\"") {
		return JSONError(http.StatusBadRequest, "invalid_params", "Invalid model or effort")
	}
	files, err := a.candyAccounts()
	if err != nil {
		return JSONError(http.StatusBadGateway, "auth_list_failed", err.Error())
	}
	var file *hostAuthFile
	for i := range files {
		if files[i].AuthIndex == body.AuthIndex {
			file = &files[i]
		}
	}
	if file == nil {
		return JSONError(http.StatusNotFound, "not_found", "No enabled Codex or Grok account with that auth_index")
	}
	provider := authCategory(file.Type)
	if body.Model == "" {
		body.Model = a.candyDefaultModel(file.AuthIndex, provider)
	}
	candyMu.Lock()
	if p := candyActive[file.AuthIndex]; p != nil && !p.done {
		candyMu.Unlock()
		return JSONError(http.StatusConflict, "running", "A candy test is already running on this account")
	}
	candyMu.Unlock()
	if next := a.candyNextAllowed(file.AuthIndex); next != nil {
		return JSONResponse(http.StatusTooManyRequests, map[string]any{"error": map[string]any{"code": "rate_limited",
			"message": "One candy test per account every 10 minutes"}, "next_allowed_at": next})
	}
	now := candyNow().UTC()
	run := suite.CandyRun{AuthIndex: file.AuthIndex, Provider: provider, Model: body.Model, Effort: body.Effort, StartedAt: now, Status: "running"}
	id, err := a.suite.StartCandyRun(run)
	if err != nil {
		return JSONError(http.StatusInternalServerError, "store_failed", err.Error())
	}
	run.ID = id
	candyMu.Lock()
	candyActive[file.AuthIndex] = &candyPending{runID: id, provider: provider}
	candyLimiter[file.AuthIndex] = now
	candyMu.Unlock()
	go a.candyExecute(*file, run)
	return JSONResponse(http.StatusAccepted, map[string]any{"run": run})
}

func (a *App) candyExecute(file hostAuthFile, run suite.CandyRun) {
	var raw []byte
	var headers http.Header
	var err error
	if run.Provider == "xai" {
		raw, headers, err = candyGrokBody(run.Model, run.Effort)
	} else {
		raw, headers, err = candyCodexBody(run.Model, run.Effort)
	}
	type result struct {
		resp json.RawMessage
		err  error
	}
	ch := make(chan result, 1)
	start := time.Now()
	if err == nil {
		go func() {
			defer func() {
				if r := recover(); r != nil {
					ch <- result{nil, fmt.Errorf("model request panicked: %v", r)}
				}
			}()
			resp, errCall := a.hostCaller(hostModelExecute, map[string]any{
				"entry_protocol": "openai-response", "exit_protocol": "openai-response", "model": run.Model, "stream": false,
				"body": raw, "headers": headers, "forced_provider": file.Provider, "auth_id": file.ID,
			})
			ch <- result{resp, errCall}
		}()
	}
	var res result
	if err != nil {
		res.err = err
	} else {
		select {
		case res = <-ch:
		case <-time.After(candyTimeout):
			res.err = fmt.Errorf("timed out after %s", candyTimeout)
		}
	}
	run.LatencyMS = time.Since(start).Milliseconds()
	run.Status = "done"
	if res.err != nil {
		run.Status, run.Error = "error", truncateText(res.err.Error(), 500)
	} else {
		var envelope struct {
			StatusCode int    `json:"StatusCode"`
			Body       []byte `json:"Body"`
		}
		if errDecode := json.Unmarshal(res.resp, &envelope); errDecode != nil {
			run.Status, run.Error = "error", "decode host response: "+errDecode.Error()
		} else if envelope.StatusCode >= 400 {
			run.Status, run.Error = "error", fmt.Sprintf("upstream status %d: %s", envelope.StatusCode, truncateText(string(envelope.Body), 300))
		} else {
			answer, model, in, out, reasoning := candyParseResponse(envelope.Body)
			run.Answer, run.ResponseModel, run.InputTokens, run.OutputTokens, run.Reasoning = truncateText(answer, 2000), model, in, out, reasoning
			run.Passed = candyStandalone21(answer)
			if answer == "" {
				run.Status, run.Error = "error", "empty answer"
			}
		}
	}
	// Prefer the host's usage record (same numbers the monitor would see).
	time.Sleep(200 * time.Millisecond)
	candyMu.Lock()
	if p := candyActive[file.AuthIndex]; p != nil && p.runID == run.ID {
		if u := p.usage; u != nil {
			if u.Detail.ReasoningTokens > 0 || run.Reasoning == 0 {
				run.Reasoning = u.Detail.ReasoningTokens
			}
			if u.Detail.InputTokens > 0 {
				run.InputTokens = u.Detail.InputTokens
			}
			if u.Detail.OutputTokens > 0 {
				run.OutputTokens = u.Detail.OutputTokens
			}
			if strings.TrimSpace(u.ResponseModel) != "" {
				run.ResponseModel = strings.TrimSpace(u.ResponseModel)
			}
			if u.Latency > 0 {
				run.LatencyMS = u.Latency.Milliseconds()
			}
		}
		p.done = true
	}
	candyMu.Unlock()
	run.FinishedAt = time.Now().UTC()
	_ = a.suite.FinishCandyRun(run)
	// Keep swallowing a late usage record for a while, then forget the run.
	time.AfterFunc(candyUsageGrace, func() {
		candyMu.Lock()
		if p := candyActive[file.AuthIndex]; p != nil && p.runID == run.ID {
			delete(candyActive, file.AuthIndex)
		}
		candyMu.Unlock()
	})
}

func candyParseResponse(body []byte) (answer, model string, in, out, reasoning int64) {
	var r struct {
		Model  string `json:"model"`
		Output []struct {
			Type    string `json:"type"`
			Content []struct {
				Type string `json:"type"`
				Text string `json:"text"`
			} `json:"content"`
		} `json:"output"`
		Usage struct {
			InputTokens         int64 `json:"input_tokens"`
			OutputTokens        int64 `json:"output_tokens"`
			OutputTokensDetails struct {
				ReasoningTokens int64 `json:"reasoning_tokens"`
			} `json:"output_tokens_details"`
		} `json:"usage"`
	}
	if json.Unmarshal(body, &r) != nil {
		return "", "", 0, 0, 0
	}
	var sb strings.Builder
	for _, item := range r.Output {
		if item.Type != "message" {
			continue
		}
		for _, c := range item.Content {
			if c.Type == "output_text" || c.Type == "text" {
				sb.WriteString(c.Text)
			}
		}
	}
	return sb.String(), r.Model, r.Usage.InputTokens, r.Usage.OutputTokens, r.Usage.OutputTokensDetails.ReasoningTokens
}

// candyCaptureUsage claims the usage record of a pending candy run (empty API
// key, same account). Claimed records are not billed or observed.
func (a *App) candyCaptureUsage(raw []byte) bool {
	candyMu.Lock()
	defer candyMu.Unlock()
	if len(candyActive) == 0 {
		return false
	}
	var r monitorUsage
	if json.Unmarshal(raw, &r) != nil || strings.TrimSpace(r.APIKey) != "" {
		return false
	}
	p := candyActive[strings.TrimSpace(r.AuthIndex)]
	if p == nil {
		return false
	}
	if p.usage == nil {
		p.usage = &r
	}
	return true
}

func truncateText(s string, n int) string {
	if len(s) <= n {
		return s
	}
	cut := n
	for cut > 0 && (s[cut]&0xC0) == 0x80 {
		cut--
	}
	return s[:cut] + "…"
}
