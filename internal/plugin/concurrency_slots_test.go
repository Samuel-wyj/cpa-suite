package plugin

import (
	"encoding/json"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"cpa-key-billing/internal/billing"
)

// concurrency-leak safety nets.

type testClock struct {
	mu  sync.Mutex
	now time.Time
}

func (c *testClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *testClock) Advance(d time.Duration) {
	c.mu.Lock()
	c.now = c.now.Add(d)
	c.mu.Unlock()
}

func clockedConcurrencyApp(t *testing.T, limit int) (*App, *testClock) {
	t.Helper()
	app := concurrencyApp(t, limit)
	clock := &testClock{now: time.Now().UTC()}
	app.store.SetClockForTest(clock.Now)
	return app, clock
}

func interceptAfterWithID(t *testing.T, app *App, requestID string) {
	t.Helper()
	raw, errHandle := app.HandleMethod(MethodRequestInterceptAfter, mustMarshal(t, RequestInterceptRequest{
		RequestID: requestID, SourceFormat: "openai", Model: flowModel, RequestedModel: flowModel,
		Metadata: map[string]any{MetadataCallerScope: flowScope(), MetadataRequestPath: "/v1/responses", MetadataGenerate: true},
	}))
	if errHandle != nil {
		t.Fatalf("request.intercept_after error = %v", errHandle)
	}
	decodeResult(t, raw, nil)
}

func currentConcurrency(t *testing.T, app *App) int {
	t.Helper()
	views := app.store.KeyViews()
	if len(views) != 1 {
		t.Fatalf("key views = %+v", views)
	}
	if len(views[0].ConcurrencySlots) != views[0].CurrentConcurrency {
		t.Fatalf("slots %+v disagree with current %d", views[0].ConcurrencySlots, views[0].CurrentConcurrency)
	}
	return views[0].CurrentConcurrency
}

func TestDefaultStaleSlotWindowIsTenMinutes(t *testing.T) {
	app := concurrencyApp(t, 1)
	if got := app.store.StaleSlotWindow(); got != 10*time.Minute {
		t.Fatalf("StaleSlotWindow = %v, want 10m", got)
	}
}

func TestHTTPTurnAcquireAndRelease(t *testing.T) {
	app := concurrencyApp(t, 2)
	if resp := interceptWithID(t, app, "http-1", true); resp.Terminate {
		t.Fatalf("response = %+v", resp)
	}
	views := app.store.KeyViews()
	slots := views[0].ConcurrencySlots
	if len(slots) != 1 || slots[0].RequestID != "http-1" || slots[0].Transport != billing.SlotTransportHTTP ||
		slots[0].Path != "/v1/responses" || slots[0].Model != flowModel || slots[0].StartedAt.IsZero() {
		t.Fatalf("slots = %+v", slots)
	}
	completeRequest(t, app, "http-1")
	if got := currentConcurrency(t, app); got != 0 {
		t.Fatalf("current after completion = %d", got)
	}
}

func TestWebSocketTurnIsCountedPerRequestAndReleased(t *testing.T) {
	app := concurrencyApp(t, 1)
	// Non-duplex WebSocket: CPA runs one lifecycle per response.create message
	// with the upgrade request's headers; an idle socket owns no lifecycle.
	headers := http.Header{"Upgrade": []string{"websocket"}, "Sec-Websocket-Key": []string{"x"}}
	request := func(id string) RequestInterceptResponse {
		raw, errHandle := app.HandleMethod(MethodRequestInterceptBefore, mustMarshal(t, RequestInterceptRequest{
			RequestID: id, SourceFormat: "openai-response", Model: flowModel, RequestedModel: flowModel, Headers: headers,
			Metadata: map[string]any{MetadataCallerScope: flowScope(), MetadataRequestPath: "/v1/responses", MetadataGenerate: true},
		}))
		if errHandle != nil {
			t.Fatal(errHandle)
		}
		var response RequestInterceptResponse
		decodeResult(t, raw, &response)
		return response
	}
	if resp := request("ws-turn-1"); resp.Terminate {
		t.Fatalf("turn 1 = %+v", resp)
	}
	if slots := app.store.KeyViews()[0].ConcurrencySlots; len(slots) != 1 || slots[0].Transport != billing.SlotTransportWebSocket {
		t.Fatalf("slots = %+v", slots)
	}
	completeRequest(t, app, "ws-turn-1")
	// The socket stays open but idle: it holds no slot, so the next turn fits.
	if got := currentConcurrency(t, app); got != 0 {
		t.Fatalf("idle socket holds %d slots", got)
	}
	if resp := request("ws-turn-2"); resp.Terminate {
		t.Fatalf("turn 2 = %+v", resp)
	}
}

func TestLostCompletionIsReclaimedAfterIdleWindow(t *testing.T) {
	app, clock := clockedConcurrencyApp(t, 1)
	if resp := interceptWithID(t, app, "lost-1", true); resp.Terminate {
		t.Fatalf("response = %+v", resp)
	}
	// request.complete never arrives for lost-1.
	clock.Advance(5 * time.Minute)
	if blocked := interceptWithID(t, app, "early", true); !blocked.Terminate {
		t.Fatalf("slot was reclaimed before the idle window: %+v", blocked)
	}
	clock.Advance(5*time.Minute + time.Second)
	if resp := interceptWithID(t, app, "after", true); resp.Terminate {
		t.Fatalf("stale slot not reclaimed: %+v", resp)
	}
	slots := app.store.KeyViews()[0].ConcurrencySlots
	if len(slots) != 1 || slots[0].RequestID != "after" {
		t.Fatalf("slots = %+v", slots)
	}
	logs, errLogs := app.store.PluginLogsPage(billing.PluginLogQuery{Limit: 100})
	if errLogs != nil {
		t.Fatal(errLogs)
	}
	found := false
	for _, entry := range logs.Entries {
		if strings.Contains(entry.Message, "Reclaimed stale concurrency slot") && strings.Contains(entry.Message, "lost-1") {
			found = true
		}
	}
	if !found {
		t.Fatalf("reclaim not logged: %+v", logs)
	}
	// A late completion of the reclaimed request must not free "after".
	completeRequest(t, app, "lost-1")
	if got := currentConcurrency(t, app); got != 1 {
		t.Fatalf("late completion changed count to %d", got)
	}
}

func TestStaleReclaimUsesLastActivity(t *testing.T) {
	app, clock := clockedConcurrencyApp(t, 1)
	interceptWithID(t, app, "long-1", true)
	clock.Advance(9 * time.Minute)
	interceptAfterWithID(t, app, "long-1")
	clock.Advance(9 * time.Minute)
	if got := app.store.ReclaimStaleSlots(); got != 0 {
		t.Fatalf("reclaimed %d slots with recent activity", got)
	}
	clock.Advance(2 * time.Minute)
	if got := app.store.ReclaimStaleSlots(); got != 1 {
		t.Fatalf("reclaimed %d, want 1", got)
	}
}

func TestResetConcurrencyEndpointClearsOnlyThatKey(t *testing.T) {
	app := concurrencyApp(t, 1)
	const otherKey = "sk-other-concurrency-0001"
	if _, errSync := app.store.SyncKeys([]string{testAPIKey, otherKey}, false); errSync != nil {
		t.Fatal(errSync)
	}
	other := billing.CallerScope(otherKey)
	app.store.AcquireSlot(other, "other-1")
	interceptWithID(t, app, "stuck-1", true)
	if blocked := interceptWithID(t, app, "stuck-2", true); !blocked.Terminate {
		t.Fatalf("expected refusal: %+v", blocked)
	}
	var result struct {
		Released int `json:"released"`
	}
	callOK(t, app, http.MethodPost, routeKeysConcurrencyReset, nil, map[string]string{"scope": flowScope()}, http.StatusOK, &result)
	if result.Released != 1 {
		t.Fatalf("released = %d", result.Released)
	}
	if resp := interceptWithID(t, app, "fresh", true); resp.Terminate {
		t.Fatalf("after reset = %+v", resp)
	}
	for _, view := range app.store.KeyViews() {
		if view.Scope == other && view.CurrentConcurrency != 1 {
			t.Fatalf("other key touched: %+v", view)
		}
	}
	// A completion for a reset request is a no-op.
	completeRequest(t, app, "stuck-1")
	for _, view := range app.store.KeyViews() {
		if view.Scope == flowScope() && view.CurrentConcurrency != 1 {
			t.Fatalf("own count = %d", view.CurrentConcurrency)
		}
	}
	if resp := callManagement(t, app, http.MethodPost, routeKeysConcurrencyReset, nil, map[string]string{"scope": strings.Repeat("0", 64)}); resp.StatusCode != http.StatusNotFound {
		t.Fatalf("unknown key status = %d", resp.StatusCode)
	}
	if resp := callManagement(t, app, http.MethodPost, routeKeysConcurrencyReset, nil, map[string]any{"scope": flowScope(), "x": 1}); resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("unknown field status = %d", resp.StatusCode)
	}
}

func TestZeroConcurrencyLimitIsUnlimited(t *testing.T) {
	app := concurrencyApp(t, 0)
	for i := 0; i < 20; i++ {
		if resp := interceptWithID(t, app, "unlimited-"+string(rune('a'+i)), true); resp.Terminate {
			t.Fatalf("request %d refused: %+v", i, resp)
		}
	}
}

func TestConcurrencyRefusalNamesIdleSessions(t *testing.T) {
	app := concurrencyApp(t, 1)
	interceptWithID(t, app, "busy", true)
	blocked := interceptWithID(t, app, "refused", true)
	if blocked.ResponseHeaders.Get("Retry-After") != "5" || !strings.Contains(string(blocked.ResponseBody), "close idle Codex sessions") {
		t.Fatalf("blocked = %+v body=%s", blocked, blocked.ResponseBody)
	}
	// The refused request owns no slot, so its completion releases nothing.
	completeRequest(t, app, "refused")
	if got := currentConcurrency(t, app); got != 1 {
		t.Fatalf("current = %d", got)
	}
}

// Routing-rule rejection (no_routed_credential 503) after admission: CPA
// v8.0.33 completes the lifecycle (completeError), which must free the slot.
func TestRoutingRejectionAfterAdmissionReleasesOnCompletion(t *testing.T) {
	app, scope := configuredRoutingApp(t, billing.RouteRule{CredentialProviders: []billing.CredentialProviderSelector{{Source: billing.CredentialSourceAIProviders, Provider: "codex"}}})
	if errSet := app.store.SetConcurrencyLimit(scope, 1); errSet != nil {
		t.Fatal(errSet)
	}
	for i := 0; i < 80; i++ {
		id := "routed-" + strings.Repeat("x", i%7) + string(rune('A'+i%26)) + string(rune('a'+i/26))
		raw, err := app.HandleMethod(MethodRequestInterceptBefore, mustMarshal(t, RequestInterceptRequest{
			RequestID: id, SourceFormat: "openai-response", Model: "gpt-5.6", RequestedModel: "gpt-5.6",
			Metadata: map[string]any{MetadataCallerScope: scope, MetadataRequestPath: "/v1/responses", MetadataGenerate: true},
		}))
		if err != nil {
			t.Fatal(err)
		}
		var intercept RequestInterceptResponse
		decodeResult(t, raw, &intercept)
		if intercept.Terminate {
			t.Fatalf("request %d refused at admission: %+v (slot leaked by an earlier 503)", i, intercept)
		}
		pick, err := app.HandleMethod(MethodSchedulerPick, mustMarshal(t, schedulerRequest(scope,
			SchedulerAuthCandidate{ID: "file-codex", Provider: "codex", Attributes: map[string]string{"path": "/auth/codex.json"}})))
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(string(pick), "no_routed_credential") {
			t.Fatalf("pick = %s, want routing 503", pick)
		}
		completeRequest(t, app, id)
	}
	for _, view := range app.store.KeyViews() {
		if view.Scope == scope && view.CurrentConcurrency != 0 {
			t.Fatalf("current = %d after routed 503s", view.CurrentConcurrency)
		}
	}
}

func TestResetConcurrencyFromUIReturnsKeyView(t *testing.T) {
	app := concurrencyApp(t, 1)
	interceptWithID(t, app, "stuck-ui", true)
	resp := callManagement(t, app, http.MethodPost, routeKeysConcurrencyReset, url.Values{"view": {"1"}},
		map[string]any{"data": map[string]string{"scope": flowScope()}})
	var result struct {
		Released int `json:"released"`
		View     struct {
			Keys []struct {
				Scope              string `json:"scope"`
				CurrentConcurrency int    `json:"current_concurrency"`
				ConcurrencySlots   []any  `json:"concurrency_slots"`
			} `json:"keys"`
		} `json:"view"`
	}
	if resp.StatusCode != http.StatusOK || json.Unmarshal(resp.Body, &result) != nil {
		t.Fatalf("response = %d %s", resp.StatusCode, resp.Body)
	}
	if result.Released != 1 || len(result.View.Keys) != 1 || result.View.Keys[0].CurrentConcurrency != 0 || len(result.View.Keys[0].ConcurrencySlots) != 0 {
		t.Fatalf("result = %s", resp.Body)
	}
}
