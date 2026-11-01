package plugin

import (
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"testing"

	"cpa-key-billing/internal/billing"
)

// per-credential model support from host.auth.models.

func modelsHost(t *testing.T, app *App, models map[string][]string, fail bool) {
	t.Helper()
	app.SetHostCaller(func(method string, _ any) (json.RawMessage, error) {
		if method != hostAuthModels {
			return nil, errors.New("unsupported")
		}
		if fail {
			return nil, errors.New("unknown method host.auth.models")
		}
		resp := hostAuthModelsResponse{}
		for id, list := range models {
			resp.Credentials = append(resp.Credentials, hostAuthModelsEntry{ID: id, Provider: "codex", Models: list})
		}
		raw, _ := json.Marshal(resp)
		return raw, nil
	})
}

func TestRouteSkipsCredentialWithoutRequestedModel(t *testing.T) {
	rule := billing.RouteRule{CredentialIDs: []string{fp("pro"), fp("plus-a"), fp("plus-b")}, CredentialOrder: customOrder("plus-a", "plus-b", "pro")}
	app, scope := configuredRoutingApp(t, rule)
	modelsHost(t, app, map[string][]string{"plus-a": {"gpt-5.4"}, "plus-b": {"gpt-5.6", "gpt-5.4"}, "pro": {"gpt-5.6"}}, false)
	all := []SchedulerAuthCandidate{cand("pro", 10), cand("plus-a", 0), cand("plus-b", -10)}
	// plus-a is first in the rule but lacks gpt-5.6 → next in rule order.
	wantPick(t, pick(t, app, schedulerRequest(scope, all...)), "plus-b")
	// thinking suffix on the requested model still matches.
	req := schedulerRequest(scope, all...)
	req.Model = "gpt-5.6(high)"
	wantPick(t, pick(t, app, req), "plus-b")
}

func TestRouteAllCredentialsLackModelReturns503(t *testing.T) {
	rule := billing.RouteRule{CredentialIDs: []string{fp("pro"), fp("plus-a")}, CredentialOrder: customOrder("plus-a", "pro")}
	app, scope := configuredRoutingApp(t, rule)
	modelsHost(t, app, map[string][]string{"plus-a": {"gpt-5.4"}, "pro": {"gpt-5.4"}}, false)
	got := pick(t, app, schedulerRequest(scope, cand("pro", 10), cand("plus-a", 0), cand("other", 20)))
	if got.err == nil || got.err.HTTPStatus != http.StatusServiceUnavailable || got.err.Code != "model_not_supported_by_route" ||
		!strings.Contains(got.err.Message, "没有账号支持模型 gpt-5.6") || !strings.Contains(got.err.Message, `supports model "gpt-5.6"`) {
		t.Fatalf("err = %+v", got.err)
	}
	// Even when CPA offers no rule credential at all, the reason is the model.
	got = pick(t, app, schedulerRequest(scope, cand("other", 20)))
	if got.err == nil || got.err.Code != "model_not_supported_by_route" {
		t.Fatalf("no-candidate err = %+v", got.err)
	}
}

func TestRouteMissingModelInfoKeepsBehaviour(t *testing.T) {
	rule := billing.RouteRule{CredentialIDs: []string{fp("pro"), fp("plus-a")}, CredentialOrder: customOrder("plus-a", "pro")}
	for name, setup := range map[string]func(*App){
		"no host":      func(*App) {},
		"host error":   func(a *App) { modelsHost(t, a, nil, true) },
		"empty list":   func(a *App) { modelsHost(t, a, map[string][]string{"plus-a": {}}, false) },
		"not reported": func(a *App) { modelsHost(t, a, map[string][]string{"pro": {"gpt-5.6"}}, false) },
	} {
		t.Run(name, func(t *testing.T) {
			app, scope := configuredRoutingApp(t, rule)
			setup(app)
			wantPick(t, pick(t, app, schedulerRequest(scope, cand("pro", 10), cand("plus-a", 0))), "plus-a")
		})
	}
}

func TestRegistrationOmitsInterceptorBody(t *testing.T) {
	reg := registration()
	if reg.SchemaVersion != 7 {
		t.Fatalf("schema = %d", reg.SchemaVersion)
	}
	raw, _ := json.Marshal(reg.Capabilities)
	if !strings.Contains(string(raw), `"request_interceptor_omit_body_before":true`) || !strings.Contains(string(raw), `"request_interceptor_omit_body_after":true`) {
		t.Fatalf("capabilities = %s", raw)
	}
}
