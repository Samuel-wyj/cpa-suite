package plugin

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"cpa-key-billing/internal/billing"
)

// per-rule credential priority.

func fp(id string) string { return billing.CredentialFingerprint(id) }

func cand(id string, priority int) SchedulerAuthCandidate {
	return SchedulerAuthCandidate{ID: id, Provider: "codex", Priority: priority, Attributes: map[string]string{"path": "/auth/" + id + ".json"}}
}

func customOrder(items ...string) *billing.CredentialOrder {
	order := &billing.CredentialOrder{Mode: billing.CredentialOrderModeCustom}
	for i, id := range items {
		order.Items = append(order.Items, billing.CredentialOrderItem{Ref: fp(id), Priority: (len(items) - i) * 10})
	}
	return order
}

type pickResult struct {
	SchedulerPickResponse
	err *EnvelopeError
}

func pick(t *testing.T, app *App, req SchedulerPickRequest) pickResult {
	t.Helper()
	raw, err := app.HandleMethod(MethodSchedulerPick, mustMarshal(t, req))
	if err != nil {
		t.Fatal(err)
	}
	var envelope Envelope
	if err := json.Unmarshal(raw, &envelope); err != nil {
		t.Fatal(err)
	}
	if !envelope.OK {
		return pickResult{err: envelope.Error}
	}
	var response SchedulerPickResponse
	if err := json.Unmarshal(envelope.Result, &response); err != nil {
		t.Fatal(err)
	}
	return pickResult{SchedulerPickResponse: response}
}

func withSession(req SchedulerPickRequest, session string) SchedulerPickRequest {
	req.Options.Headers = map[string][]string{"Session_id": {session}}
	return req
}

func wantPick(t *testing.T, got pickResult, id string) {
	t.Helper()
	if got.err != nil || !got.Handled || got.AuthID != id {
		t.Fatalf("pick = %+v err=%+v, want %s", got.SchedulerPickResponse, got.err, id)
	}
}

func TestRegistrationRequestsCandidatesAcrossPriorities(t *testing.T) {
	raw, _ := json.Marshal(registration().Capabilities)
	if !strings.Contains(string(raw), `"scheduler_across_priorities":true`) {
		t.Fatalf("capabilities = %s", raw)
	}
}

// The user's model: tick 1 Pro + 2 Plus, drag the Plus above Pro. CPA gives
// them different tiers (Pro is the highest); the rule order still wins and
// falls through only when a Plus is not a candidate (cooling/quota/disabled).
func TestCustomOrderOverridesCPATiersAndFallsThrough(t *testing.T) {
	rule := billing.RouteRule{CredentialIDs: []string{fp("pro"), fp("plus-a"), fp("plus-b")}, CredentialOrder: customOrder("plus-a", "plus-b", "pro")}
	app, scope := configuredRoutingApp(t, rule)
	all := []SchedulerAuthCandidate{cand("pro", 10), cand("plus-a", 0), cand("plus-b", -10), cand("other", 20)}
	wantPick(t, pick(t, app, schedulerRequest(scope, all...)), "plus-a")
	wantPick(t, pick(t, app, schedulerRequest(scope, all[0], all[2], all[3])), "plus-b")
	wantPick(t, pick(t, app, schedulerRequest(scope, all[0], all[3])), "pro")
	got := pick(t, app, schedulerRequest(scope, all[3]))
	if got.err == nil || got.err.HTTPStatus != http.StatusServiceUnavailable || !strings.Contains(got.err.Message, noRoutedCredentialMessage) {
		t.Fatalf("only a disallowed candidate left: %+v", got)
	}
}

// 14:10 reproduction: the legacy rule allows only a lower CPA tier. Before
// test5 the plugin saw only the top tier and returned 503.
func TestLegacyRuleFallsThroughToLowerTierInsteadOf503(t *testing.T) {
	app, scope := configuredRoutingApp(t, billing.RouteRule{CredentialIDs: []string{fp("pro")}})
	wantPick(t, pick(t, app, schedulerRequest(scope, cand("plus-top", 10), cand("pro", 0))), "pro")
}

func TestLegacyRuleKeepsHighestAllowedTierAndRoundRobin(t *testing.T) {
	app, scope := configuredRoutingApp(t, billing.RouteRule{DeniedCredentialIDs: []string{fp("denied")}})
	cands := []SchedulerAuthCandidate{cand("a", 5), cand("b", 5), cand("low", 0), cand("denied", 9)}
	seen := map[string]int{}
	for range 4 {
		got := pick(t, app, schedulerRequest(scope, cands...))
		seen[got.AuthID]++
	}
	if seen["a"] != 2 || seen["b"] != 2 {
		t.Fatalf("legacy rule must round-robin the highest allowed tier: %v", seen)
	}
	// All candidates allowed and no custom order: CPA keeps choosing.
	got := pick(t, app, schedulerRequest(scope, cand("a", 5), cand("low", 0)))
	if got.err != nil || got.Handled {
		t.Fatalf("all allowed legacy rule should defer to CPA: %+v", got)
	}
}

func TestKeyWithoutRulesIsUnchanged(t *testing.T) {
	app := newConfiguredApp(t)
	const key = "sk-route-test-unbound-01"
	if _, err := app.store.SyncKeys([]string{key}, false); err != nil {
		t.Fatal(err)
	}
	got := pick(t, app, schedulerRequest(billing.CallerScope(key), cand("a", 10), cand("b", 0)))
	if got.err != nil || got.Handled {
		t.Fatalf("unbound key must defer to CPA: %+v", got)
	}
}

func TestOrderOnlyRuleIsHonouredWithoutAllowlist(t *testing.T) {
	app, scope := configuredRoutingApp(t, billing.RouteRule{CredentialOrder: customOrder("b")})
	wantPick(t, pick(t, app, schedulerRequest(scope, cand("a", 10), cand("b", 0), cand("c", 5))), "b")
	// Unlisted candidates follow by CPA priority.
	wantPick(t, pick(t, app, schedulerRequest(scope, cand("a", 0), cand("c", 5))), "c")
}

func TestSameTierFillFirstAndRoundRobin(t *testing.T) {
	order := &billing.CredentialOrder{Mode: billing.CredentialOrderModeCustom, Items: []billing.CredentialOrderItem{{Ref: fp("x"), Priority: 10}, {Ref: fp("y"), Priority: 10}, {Ref: fp("z"), Priority: 5}}}
	app, scope := configuredRoutingApp(t, billing.RouteRule{CredentialOrder: order})
	for range 3 {
		wantPick(t, pick(t, app, schedulerRequest(scope, cand("z", 9), cand("y", 0), cand("x", 0))), "x")
	}
	order.SameTier = billing.SameTierRoundRobin
	if _, err := app.store.UpdateRoute(billing.RoutePatch{ID: "route-test", Rule: &billing.RouteRule{CredentialOrder: order}}, nil); err != nil {
		t.Fatal(err)
	}
	seen := map[string]int{}
	for range 4 {
		seen[pick(t, app, schedulerRequest(scope, cand("z", 9), cand("y", 0), cand("x", 0))).AuthID]++
	}
	if seen["x"] != 2 || seen["y"] != 2 {
		t.Fatalf("round-robin within the same level: %v", seen)
	}
}

func TestStickinessKeepsSessionUntilNewSession(t *testing.T) {
	rule := billing.RouteRule{CredentialIDs: []string{fp("plus"), fp("pro")}, CredentialOrder: customOrder("plus", "pro")}
	app, scope := configuredRoutingApp(t, rule)
	now := time.Date(2026, 10, 19, 12, 0, 0, 0, time.UTC)
	app.store.SetClockForTest(func() time.Time { return now })
	both := schedulerRequest(scope, cand("pro", 0), cand("plus", 0))
	onlyPro := schedulerRequest(scope, cand("pro", 0))
	wantPick(t, pick(t, app, withSession(both, "s1")), "plus")
	// Plus cooling: s1 falls through and is bound to Pro.
	wantPick(t, pick(t, app, withSession(onlyPro, "s1")), "pro")
	// Plus recovers: s1 stays on Pro (cache), a new session starts on Plus.
	wantPick(t, pick(t, app, withSession(both, "s1")), "pro")
	wantPick(t, pick(t, app, withSession(both, "s2")), "plus")
	// Without a session header there is no stickiness.
	wantPick(t, pick(t, app, both), "plus")
	// TTL (default 120 min) expiry returns s1 to Plus.
	now = now.Add(121 * time.Minute)
	wantPick(t, pick(t, app, withSession(both, "s1")), "plus")

	// immediate: switch back as soon as the higher credential is a candidate.
	rule.CredentialOrder.FallbackReturn = billing.FallbackReturnImmediate
	if _, err := app.store.UpdateRoute(billing.RoutePatch{ID: "route-test", Rule: &rule}, nil); err != nil {
		t.Fatal(err)
	}
	wantPick(t, pick(t, app, withSession(onlyPro, "s3")), "pro")
	wantPick(t, pick(t, app, withSession(both, "s3")), "plus")
}

func TestStickinessDisabledByZeroTTL(t *testing.T) {
	app := newTestApp(t)
	t.Cleanup(app.Shutdown)
	raw, err := app.HandleMethod(MethodPluginRegister, mustMarshal(t, LifecycleRequest{ConfigYAML: append(testConfigYAML(t, true), []byte("route_session_ttl_minutes: 0\n")...)}))
	if err != nil {
		t.Fatal(err)
	}
	decodeResult(t, raw, nil)
	const key = "sk-route-test-00000002"
	scope := billing.CallerScope(key)
	if _, err := app.store.SyncKeys([]string{key}, false); err != nil {
		t.Fatal(err)
	}
	if _, err := app.store.CreateRoute(billing.Route{ID: "route-ttl", Name: "TTL", Rule: billing.RouteRule{CredentialOrder: customOrder("plus", "pro")}}, []string{scope}); err != nil {
		t.Fatal(err)
	}
	if app.store.RouteSessionTTL() != 0 {
		t.Fatalf("ttl = %v", app.store.RouteSessionTTL())
	}
	wantPick(t, pick(t, app, withSession(schedulerRequest(scope, cand("pro", 0)), "s")), "pro")
	wantPick(t, pick(t, app, withSession(schedulerRequest(scope, cand("pro", 0), cand("plus", 0)), "s")), "plus")
}

func TestInterRulePriorityMergeAndConflicts(t *testing.T) {
	app, scope := configuredRoutingApp(t, billing.RouteRule{CredentialOrder: customOrder("y", "x")})
	high := customOrder("x", "y")
	high.RulePriority = 100
	if _, err := app.store.CreateRoute(billing.Route{ID: "route-high", Name: "High", Rule: billing.RouteRule{CredentialOrder: high}}, []string{scope}); err != nil {
		t.Fatal(err)
	}
	wantPick(t, pick(t, app, schedulerRequest(scope, cand("y", 9), cand("x", 0))), "x")
	decision := app.store.ResolveRouting(scope, "gpt-5.6", "gpt-5.6")
	if len(decision.Order.Conflicts) != 2 || decision.Order.Conflicts[0].Winner != "High" || decision.Order.SettingsRule != "High" {
		t.Fatalf("conflicts = %+v", decision.Order)
	}
	// Lower the high rule below the other: order flips.
	high.RulePriority = -5
	if _, err := app.store.UpdateRoute(billing.RoutePatch{ID: "route-high", Rule: &billing.RouteRule{CredentialOrder: high}}, nil); err != nil {
		t.Fatal(err)
	}
	wantPick(t, pick(t, app, schedulerRequest(scope, cand("y", 0), cand("x", 9))), "y")
}

func TestCredentialOrderValidation(t *testing.T) {
	for name, rule := range map[string]billing.RouteRule{
		"denied":     {DeniedCredentialIDs: []string{fp("a")}, CredentialOrder: customOrder("a")},
		"not listed": {CredentialIDs: []string{fp("b")}, CredentialOrder: customOrder("a")},
		"duplicate":  {CredentialOrder: &billing.CredentialOrder{Mode: "custom", Items: []billing.CredentialOrderItem{{Ref: fp("a")}, {Ref: fp("a")}}}},
		"bad mode":   {CredentialOrder: &billing.CredentialOrder{Mode: "first"}},
		"bad ref":    {CredentialOrder: &billing.CredentialOrder{Mode: "custom", Items: []billing.CredentialOrderItem{{Ref: "nope"}}}},
		"bad tier":   {CredentialOrder: &billing.CredentialOrder{SameTier: "random"}},
	} {
		if _, err := billing.NormalizeRouteRule(rule); err == nil {
			t.Errorf("%s: expected validation error", name)
		}
	}
	// Category allowlists can order any credential they cover.
	if _, err := billing.NormalizeRouteRule(billing.RouteRule{CredentialProviders: []billing.CredentialProviderSelector{{Source: "auth-files", Provider: "codex"}}, CredentialOrder: customOrder("a")}); err != nil {
		t.Fatal(err)
	}
	rule, err := billing.NormalizeRouteRule(billing.RouteRule{CredentialOrder: &billing.CredentialOrder{Mode: "cpa", Items: customOrder("a").Items}})
	if err != nil || rule.CredentialOrder.Items != nil || rule.CredentialOrder.SameTier != billing.SameTierFillFirst || rule.CredentialOrder.FallbackReturn != billing.FallbackReturnNewSession {
		t.Fatalf("cpa mode normalization = %+v %v", rule.CredentialOrder, err)
	}
	bindings, err := billing.NormalizeRouteBindings(billing.RouteBindings{RouteRule: billing.RouteRule{CredentialOrder: customOrder("a")}})
	if err != nil || bindings.CredentialOrder != nil {
		t.Fatalf("inline rule must not carry an order: %+v %v", bindings, err)
	}
}

func TestNoMatchMessageIsDescriptiveAndHidesAccounts(t *testing.T) {
	app, scope := configuredRoutingApp(t, billing.RouteRule{CredentialIDs: []string{fp("plus"), fp("pro")}})
	app.routingMu.Lock()
	app.credentials[fp("plus")] = credentialView{Ref: fp("plus"), Source: "auth-files", Provider: "codex", DisplayName: "plus@example.com", Unavailable: true}
	app.credentials[fp("pro")] = credentialView{Ref: fp("pro"), Source: "auth-files", Provider: "codex", DisplayName: "pro@example.com", Disabled: true}
	app.routingMu.Unlock()
	req := schedulerRequest(scope, cand("other", 0))
	req.Provider = "codex"
	got := pick(t, app, req)
	if got.err == nil {
		t.Fatal("expected 503")
	}
	msg := got.err.Message
	for _, want := range []string{noRoutedCredentialMessage, `"Route"`, "2 allowed", "1 cooling down or quota exhausted", "1 disabled"} {
		if !strings.Contains(msg, want) {
			t.Fatalf("message %q lacks %q", msg, want)
		}
	}
	if strings.Contains(msg, "@example.com") {
		t.Fatalf("message leaks account: %q", msg)
	}
}

func TestRouteLogRecordsRankAndSkips(t *testing.T) {
	rule := billing.RouteRule{CredentialIDs: []string{fp("plus"), fp("pro")}, CredentialOrder: customOrder("plus", "pro")}
	app, scope := configuredRoutingApp(t, rule)
	app.routingMu.Lock()
	app.credentials[fp("plus")] = credentialView{Ref: fp("plus"), Source: "auth-files", Provider: "codex", DisplayName: "plus@example.com", Unavailable: true}
	app.routingMu.Unlock()
	if _, err := app.HandleMethod(MethodRequestInterceptBefore, mustMarshal(t, RequestInterceptRequest{RequestID: "r1", SourceFormat: "openai", Model: "gpt-5.6", RequestedModel: "gpt-5.6", Metadata: map[string]any{MetadataCallerScope: scope}})); err != nil {
		t.Fatal(err)
	}
	wantPick(t, pick(t, app, schedulerRequest(scope, cand("pro", 10))), "pro")
	if _, err := app.HandleMethod(MethodRequestInterceptAfter, mustMarshal(t, RequestInterceptRequest{RequestID: "r1", Metadata: map[string]any{MetadataCallerScope: scope, MetadataSelectedAuth: "pro"}})); err != nil {
		t.Fatal(err)
	}
	if _, err := app.HandleMethod(MethodRequestComplete, mustMarshal(t, RequestCompletion{RequestID: "r1", Outcome: "succeeded", StatusCode: 200})); err != nil {
		t.Fatal(err)
	}
	page, err := app.store.PluginLogsPage(billing.PluginLogQuery{Levels: []billing.PluginLogLevel{billing.PluginLogDebug}, Limit: 10})
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, entry := range page.Entries {
		if !strings.HasPrefix(entry.Message, "route ") {
			continue
		}
		var row map[string]any
		if err := json.Unmarshal([]byte(strings.TrimPrefix(entry.Message, "route ")), &row); err != nil {
			t.Fatal(err)
		}
		skipped, _ := row["skipped"].([]any)
		if row["credential_rank"] != float64(2) || row["credential_order"] != "custom" || len(skipped) != 1 || !strings.Contains(skipped[0].(string), "cooling down or quota exhausted") {
			t.Fatalf("route row = %v", row)
		}
		if strings.Contains(entry.Message, "plus@example.com") {
			t.Fatalf("route log leaks email: %s", entry.Message)
		}
		found = true
	}
	if !found {
		t.Fatalf("no route log: %+v", page.Entries)
	}
}

func TestRoutePreviewShowsEffectiveOrderAndHints(t *testing.T) {
	rule := billing.RouteRule{CredentialIDs: []string{fp("plus-a"), fp("plus-b"), fp("pro")}, CredentialOrder: customOrder("plus-a", "plus-b", "pro")}
	app, scope := configuredRoutingApp(t, billing.RouteRule{})
	app.routingMu.Lock()
	app.credentials[fp("plus-a")] = credentialView{Ref: fp("plus-a"), Source: "auth-files", Provider: "codex", DisplayName: "a@example.com", Unavailable: true, Websockets: true}
	app.credentials[fp("plus-b")] = credentialView{Ref: fp("plus-b"), Source: "auth-files", Provider: "codex", DisplayName: "b@example.com", Priority: -10}
	app.credentials[fp("pro")] = credentialView{Ref: fp("pro"), Source: "auth-files", Provider: "codex", DisplayName: "p@example.com", Priority: 10}
	app.routingMu.Unlock()
	var response struct {
		Keys  []previewKey `json:"keys"`
		Hints previewHints `json:"hints"`
	}
	callOK(t, app, http.MethodPost, routeRoutesPreview, nil, map[string]any{"id": "route-test", "name": "Mine", "rule": rule, "scopes": []string{scope}}, http.StatusOK, &response)
	if len(response.Keys) != 1 || !response.Keys[0].Custom {
		t.Fatalf("preview = %+v", response)
	}
	order := response.Keys[0].Order
	if len(order) != 3 || order[0].Ref != fp("plus-a") || order[1].Ref != fp("plus-b") || order[2].Ref != fp("pro") ||
		order[0].State != "unavailable" || !order[1].CurrentFirst || order[2].Rank != 3 {
		t.Fatalf("order = %+v", order)
	}
	if !response.Hints.WebsocketsMismatch {
		t.Fatalf("hints = %+v", response.Hints)
	}
	if response.Hints.SessionTTLMinutes != 120 {
		t.Fatalf("ttl = %d", response.Hints.SessionTTLMinutes)
	}
}

func TestHostTierSignal(t *testing.T) {
	app, scope := configuredRoutingApp(t, billing.RouteRule{CredentialOrder: customOrder("a")})
	app.routingMu.Lock()
	app.credentials[fp("low")] = credentialView{Ref: fp("low"), Source: "auth-files", Provider: "codex", Priority: -10}
	app.routingMu.Unlock()
	pick(t, app, schedulerRequest(scope, cand("a", 5), cand("b", 5)))
	if signal := app.hostTierSignal(); signal.AcrossObserved || signal.SuspectAt.IsZero() {
		t.Fatalf("single-tier candidates with a usable lower-tier credential: %+v", signal)
	}
	pick(t, app, schedulerRequest(scope, cand("a", 5), cand("low", -10)))
	if signal := app.hostTierSignal(); !signal.AcrossObserved {
		t.Fatalf("multi-tier candidates: %+v", signal)
	}
}

func TestConfigCredentialSyncKeepsCPAPriority(t *testing.T) {
	app := newConfiguredApp(t)
	app.observeCandidates([]SchedulerAuthCandidate{{ID: "codex:apikey:a", Provider: "codex", Priority: 10, Attributes: map[string]string{"source_backend": "config"}},
		{ID: "codex:apikey:b", Provider: "codex", Priority: -5, Attributes: map[string]string{"source_backend": "config"}}})
	refA, refB := fp("codex:apikey:a"), fp("codex:apikey:b")
	five := 5
	var response struct {
		Credentials []credentialView `json:"credentials"`
	}
	callOK(t, app, http.MethodPost, routeCredentialsSync, nil, map[string]any{"credentials": []map[string]any{
		{"ref": refA, "provider": "codex", "display_name": "dummy-a"},
		{"ref": refB, "provider": "codex", "display_name": "dummy-b", "priority": five, "websockets": true},
	}}, http.StatusOK, &response)
	got := map[string]credentialView{}
	for _, item := range response.Credentials {
		got[item.Ref] = item
	}
	if got[refA].Priority != 10 || got[refB].Priority != 5 || !got[refB].Websockets {
		t.Fatalf("after sync: %+v", got)
	}
}
