package plugin

import (
	"encoding/json"
	"fmt"
	"strings"

	"cpa-key-billing/internal/billing"
)

// per-credential model support via host.auth.models (CPA v8.0.40+).

const hostAuthModels = "host.auth.models"

type hostAuthModelsEntry struct {
	ID       string   `json:"id"`
	Provider string   `json:"provider"`
	Models   []string `json:"models"`
}

type hostAuthModelsResponse struct {
	Credentials []hostAuthModelsEntry `json:"credentials"`
}

// credentialModels returns raw credential ID -> model set. Credentials with no
// reported models are omitted (unknown = supported). A nil map means the host
// could not answer; callers then keep the previous behaviour.
func (a *App) credentialModels() map[string]map[string]struct{} {
	if a == nil || a.hostCaller == nil {
		return nil
	}
	raw, err := a.hostCaller(hostAuthModels, map[string]any{})
	if err != nil {
		return nil
	}
	var resp hostAuthModelsResponse
	if json.Unmarshal(raw, &resp) != nil {
		return nil
	}
	out := make(map[string]map[string]struct{}, len(resp.Credentials))
	for _, entry := range resp.Credentials {
		id := strings.TrimSpace(entry.ID)
		if id == "" || len(entry.Models) == 0 {
			continue
		}
		set := make(map[string]struct{}, len(entry.Models))
		for _, m := range entry.Models {
			if m = normalizeModelID(m); m != "" {
				set[m] = struct{}{}
			}
		}
		if len(set) > 0 {
			out[id] = set
		}
	}
	return out
}

func normalizeModelID(model string) string {
	model = strings.ToLower(strings.TrimSpace(model))
	if i := strings.Index(model, "("); i > 0 { // thinking suffix, e.g. gpt-5(high)
		model = strings.TrimSpace(model[:i])
	}
	return model
}

func modelSupported(models map[string]map[string]struct{}, id string, wanted []string) bool {
	set, ok := models[strings.TrimSpace(id)]
	if !ok {
		return true
	}
	known := false
	for _, w := range wanted {
		if w = normalizeModelID(w); w == "" {
			continue
		}
		known = true
		if _, hit := set[w]; hit {
			return true
		}
	}
	return !known
}

func wantedModels(req SchedulerPickRequest, requested string) []string {
	return []string{req.Model, requested}
}

// unsupportedModelMessage reports when the rule's credentials (of the request's
// providers) all have known model lists and none lists the model.
func (a *App) unsupportedModelMessage(req SchedulerPickRequest, decision billing.RoutingDecision, models map[string]map[string]struct{}, wanted []string) string {
	if models == nil {
		return ""
	}
	providers := map[string]struct{}{}
	for _, p := range append([]string{req.Provider}, req.Providers...) {
		if p = strings.ToLower(strings.TrimSpace(p)); p != "" {
			providers[p] = struct{}{}
		}
	}
	a.routingMu.Lock()
	ids := make(map[string]string, len(a.credentialsByRawID))
	for id, ref := range a.credentialsByRawID {
		ids[id] = ref
	}
	creds := a.credentials
	total, supported := 0, 0
	for id, ref := range ids {
		c, ok := creds[ref]
		if !ok {
			continue
		}
		if _, ok := providers[c.Provider]; len(providers) > 0 && !ok {
			continue
		}
		if !decision.AllowsCredential(ref, c.Source, c.Provider) {
			continue
		}
		total++
		if modelSupported(models, id, wanted) {
			supported++
		}
	}
	a.routingMu.Unlock()
	if total == 0 || supported > 0 {
		return ""
	}
	return unsupportedModelText(decision, displayModel(wanted))
}

func displayModel(wanted []string) string {
	for i := len(wanted) - 1; i >= 0; i-- {
		if m := strings.TrimSpace(wanted[i]); m != "" {
			return boundedLogValue(m, 128)
		}
	}
	return "?"
}

func unsupportedModelText(decision billing.RoutingDecision, model string) string {
	rules := ""
	if len(decision.RuleNames) > 0 {
		q := make([]string, len(decision.RuleNames))
		for i, n := range decision.RuleNames {
			q[i] = fmt.Sprintf("「%s」", boundedLogValue(n, 128))
		}
		rules = strings.Join(q, "、")
	}
	return fmt.Sprintf("该路由规则%s里没有账号支持模型 %s。No credential in the routing rule supports model %q; ask the administrator to add a credential that serves it.", rules, model, model)
}
