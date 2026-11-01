package plugin

// CPA Suite parity harness. This file only uses APIs that exist in upstream
// cpa-key-billing (plus billing/suite_clock.go), so scripts/parity.sh copies
// it verbatim into a pristine upstream worktree, runs the same scripted
// scenario against both builds and diffs the transcripts.

import (
	"database/sql"
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"

	"cpa-key-billing/internal/billing"
)

type parityStep struct {
	Step     string          `json:"step"`
	Response json.RawMessage `json:"response"`
}

func TestParityTranscript(t *testing.T) {
	out := os.Getenv("SUITE_PARITY_OUT")
	if out == "" {
		t.Skip("set SUITE_PARITY_OUT (scripts/parity.sh) to produce a transcript")
	}
	dir := os.Getenv("SUITE_PARITY_DIR")
	if dir == "" {
		dir = t.TempDir()
	}
	state := filepath.Join(dir, "parity.db")
	now := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)
	app := newTestApp(t)
	app.store.SetClockForTest(func() time.Time { return now })
	if _, err := app.HandleMethod(MethodPluginRegister, mustMarshal(t, LifecycleRequest{
		ConfigYAML: []byte("enabled: true\nstate_file: \"" + state + "\"\n"),
	})); err != nil {
		t.Fatal(err)
	}
	var steps []parityStep
	record := func(step string, raw []byte) {
		steps = append(steps, parityStep{Step: step, Response: json.RawMessage(raw)})
	}
	call := func(step string, method string, req any) {
		raw, err := app.HandleMethod(method, mustMarshal(t, req))
		if err != nil {
			raw, _ = json.Marshal(map[string]string{"go_error": err.Error()})
		}
		record(step, raw)
	}
	manage := func(step, method, suffix string, body any) {
		resp := callManagement(t, app, method, suffix, nil, body)
		raw, _ := json.Marshal(map[string]any{"status": resp.StatusCode, "body": json.RawMessage(nonEmptyJSON(resp.Body))})
		record(step, raw)
	}
	keys := []string{"sk-parity-key-0000001", "sk-parity-key-0000002", "sk-parity-key-0000003"}
	scopes := []string{billing.CallerScope(keys[0]), billing.CallerScope(keys[1]), billing.CallerScope(keys[2])}
	cacheRead := 0.2
	manage("price gpt-5.5", http.MethodPut, routePrices, billing.CustomPrice{ModelID: "gpt-5.5", PriceRates: billing.PriceRates{
		InputPer1M: 2, OutputPer1M: 8, CacheReadPer1M: &cacheRead,
		LongContext: &billing.LongContextPrice{ThresholdInputTokens: 200000, InputPer1M: 4, OutputPer1M: 12}}})
	manage("price mini", http.MethodPut, routePrices, billing.CustomPrice{ModelID: "gpt-5.5-mini", PriceRates: billing.PriceRates{InputPer1M: 0.5, OutputPer1M: 1}})
	manage("sync keys", http.MethodPost, routeKeysSync, map[string]any{"keys": keys})
	manage("label k1", http.MethodPost, routeKeysLabel, map[string]any{"scope": scopes[0], "label": "王康"})
	manage("plan independent", http.MethodPost, routePlans, map[string]any{"id": "indep", "name": "Indep", "scopes": []string{scopes[0]},
		"windows": []billing.QuotaWindow{{Name: "5h", AmountUSD: 1, PeriodSeconds: 18000}, {Name: "week", RequestLimit: 20, PeriodSeconds: 604800}}})
	manage("plan shared", http.MethodPost, routePlans, map[string]any{"id": "shared", "name": "Shared", "scopes": []string{scopes[1]},
		"windows": []billing.QuotaWindow{{Name: "day", AmountUSD: 0.5, TokenLimit: 1000000, PeriodSeconds: 86400, CycleAnchorAt: now.Add(time.Hour)}}})
	manage("concurrency k1", http.MethodPost, routeKeysConcurrency, map[string]any{"scope": scopes[0], "concurrency_limit": 1})
	manage("route models k2", http.MethodPost, routeRoutes, map[string]any{"name": "Only 5.5", "rule": map[string]any{"models": []string{"gpt-5.5"}}, "scopes": []string{scopes[1]}})
	if _, err := app.store.CreateRoute(billing.Route{ID: "cred-a", Name: "Cred A", Rule: billing.RouteRule{
		CredentialIDs: []string{billing.CredentialFingerprint("auth-a")}, DeniedModels: []string{"gpt-5.5-mini"}}}, []string{scopes[0]}); err != nil {
		t.Fatal(err)
	}

	seq := 0
	request := func(k int, model string, detail UsageDetail, failed bool, complete bool) {
		seq++
		id := "req-" + time.Duration(seq).String()
		meta := map[string]any{MetadataCallerScope: scopes[k], MetadataRequestPath: "/v1/chat/completions"}
		call(id+" intercept_before", MethodRequestInterceptBefore, RequestInterceptRequest{RequestID: id, SourceFormat: "openai", Model: model, RequestedModel: model, Metadata: meta})
		call(id+" intercept_after", MethodRequestInterceptAfter, RequestInterceptRequest{RequestID: id, SourceFormat: "openai", Model: model, RequestedModel: model, Metadata: meta})
		pick := SchedulerPickRequest{Model: model, Candidates: []SchedulerAuthCandidate{{ID: "auth-a", Provider: "openai"}, {ID: "auth-b", Provider: "openai"}, {ID: "auth-c", Provider: "openai", Attributes: map[string]string{"weight": "2"}}}}
		pick.Options.Metadata = meta
		call(id+" scheduler.pick", MethodSchedulerPick, pick)
		usage := UsageRecord{Provider: "openai", ExecutorType: "OpenAICompatExecutor", Model: model, Alias: model, APIKey: keys[k], AuthIndex: "auth-a", AuthType: "oauth",
			Generate: true, RequestedAt: now, Latency: 1500 * time.Millisecond, TTFT: 300 * time.Millisecond, Detail: detail, Failed: failed}
		if failed {
			usage.Failure = UsageFailure{StatusCode: 500, Body: `{"error":{"message":"upstream boom"}}`}
		}
		call(id+" usage.handle", MethodUsageHandle, usage)
		if complete {
			call(id+" request.complete", MethodRequestComplete, RequestCompletion{RequestID: id, Outcome: "succeeded", StatusCode: 200})
		}
		now = now.Add(7 * time.Minute)
	}
	big := UsageDetail{InputTokens: 100000, OutputTokens: 20000, ReasoningTokens: 5000, CacheReadTokens: 40000, TotalTokens: 120000}
	small := UsageDetail{InputTokens: 1000, OutputTokens: 200, TotalTokens: 1200}
	longCtx := UsageDetail{InputTokens: 250000, OutputTokens: 1000, TotalTokens: 251000}
	for i := 0; i < 5; i++ { // k1: independent $1 window exhausts
		request(0, "gpt-5.5", big, false, true)
	}
	request(0, "gpt-5.5", small, false, false)     // holds k1's only slot (if admitted)
	request(0, "gpt-5.5", small, false, true)      // concurrency
	request(0, "gpt-5.5-mini", small, false, true) // denied model for k1
	request(1, "gpt-5.5-mini", small, false, true) // k2 route allows gpt-5.5 only
	for i := 0; i < 4; i++ {
		request(1, "gpt-5.5", longCtx, false, true) // long-context tier, shared window
	}
	request(2, "zzz-unpriced-model", small, false, true)
	request(2, "gpt-5.5", small, true, true)                                                           // failed: recorded, not charged
	request(2, "gpt-5.5", UsageDetail{InputTokens: 10, OutputTokens: 5, TotalTokens: 99}, false, true) // inconsistent tokens
	now = now.Add(6 * time.Hour)
	request(0, "gpt-5.5", small, false, true) // after 5h window
	request(1, "gpt-5.5", small, false, true) // after shared reset
	manage("reset k1", http.MethodPost, routeKeysReset, billing.ResetRequest{Mode: "all", Scopes: []string{scopes[0]}})
	request(0, "gpt-5.5", big, false, true)
	manage("unbind k2", http.MethodPost, routeKeysUnbind, map[string]any{"scope": scopes[1]})
	request(1, "gpt-5.5", small, false, true)

	manage("view keys", http.MethodGet, routeKeys, nil)
	manage("view plans", http.MethodGet, routePlans, nil)
	manage("view routes", http.MethodGet, routeRoutes, nil)
	events, err := app.store.RequestEvents(billing.RequestEventQuery{Limit: 200})
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := json.Marshal(events.Entries)
	record("request events", raw)
	errorsView, err := app.store.RequestErrors(billing.RequestErrorQuery{Limit: 200})
	if err != nil {
		t.Fatal(err)
	}
	raw, _ = json.Marshal(errorsView.Entries)
	record("request errors", raw)
	app.Shutdown()
	for _, q := range []string{
		"SELECT scope, preview, label, plan_id, concurrency_limit, cycles_json, route_bindings_json FROM api_keys ORDER BY scope",
		"SELECT id, at, scope, billing_model, failed, accounting_quality, price_source, total_usd, uncached_input_usd, cache_read_usd, output_usd, uncached_input_tokens, cache_read_tokens, billed_output_tokens, reasoning_tokens, tiered, long_context FROM request_events ORDER BY id",
	} {
		rows := dbRows(t, state, q)
		raw, _ := json.Marshal(rows)
		record("db "+q, raw)
	}
	raw, err = json.MarshalIndent(steps, "", " ")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(out, raw, 0o644); err != nil {
		t.Fatal(err)
	}
}

func nonEmptyJSON(raw []byte) []byte {
	if len(raw) == 0 {
		return []byte("null")
	}
	return raw
}

func dbRows(t *testing.T, path, query string) []string {
	t.Helper()
	db, err := sql.Open("sqlite3", "file:"+path+"?mode=ro")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	rows, err := db.Query(query)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	cols, _ := rows.Columns()
	var out []string
	for rows.Next() {
		values := make([]any, len(cols))
		ptrs := make([]any, len(cols))
		for i := range values {
			ptrs[i] = &values[i]
		}
		if err := rows.Scan(ptrs...); err != nil {
			t.Fatal(err)
		}
		raw, _ := json.Marshal(values)
		out = append(out, string(raw))
	}
	return out
}
