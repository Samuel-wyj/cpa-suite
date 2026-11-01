package plugin

import (
	"net/http"
	"slices"
	"strings"
	"time"

	"cpa-key-billing/internal/billing"
	"cpa-key-billing/internal/messages"
)

// effective credential order per key for the rule editor.

const previewDraftRouteID = "__draft__"

type previewEntry struct {
	Ref          string           `json:"ref"`
	Source       string           `json:"source"`
	Provider     string           `json:"provider"`
	DisplayName  string           `json:"display_name"`
	DisplayMsg   messages.Message `json:"display_name_message,omitzero"`
	CPAPriority  int              `json:"cpa_priority"`
	State        string           `json:"state"`
	Rank         int              `json:"rank"`
	Listed       bool             `json:"listed"`
	RuleName     string           `json:"rule_name,omitempty"`
	Websockets   bool             `json:"websockets"`
	NextRetry    time.Time        `json:"next_retry_after,omitzero"`
	CurrentFirst bool             `json:"current_first,omitempty"`
}

type previewKey struct {
	Scope          string                  `json:"scope"`
	Found          bool                    `json:"found"`
	Custom         bool                    `json:"custom"`
	Legacy         bool                    `json:"legacy"`
	Unrestricted   bool                    `json:"unrestricted"`
	SameTier       string                  `json:"same_tier"`
	FallbackReturn string                  `json:"fallback_return"`
	SettingsRule   string                  `json:"settings_rule,omitempty"`
	RuleNames      []string                `json:"rule_names"`
	Order          []previewEntry          `json:"order"`
	Denied         []previewEntry          `json:"denied"`
	Conflicts      []billing.OrderConflict `json:"conflicts,omitempty"`
	Error          string                  `json:"error,omitempty"`
}

type previewHints struct {
	HostTopTierOnly     bool           `json:"host_top_tier_only"`
	HostAcrossObserved  bool           `json:"host_across_observed"`
	WebsocketsMismatch  bool           `json:"websockets_mismatch"`
	WebsocketsOn        []previewEntry `json:"websockets_on,omitempty"`
	WebsocketsOff       []previewEntry `json:"websockets_off,omitempty"`
	OrderUnusable       []previewEntry `json:"order_unusable,omitempty"`
	SessionTTLMinutes   int            `json:"session_ttl_minutes"`
	InventoryIncomplete bool           `json:"inventory_incomplete,omitempty"`
}

func (a *App) previewEntry(ref string, credential credentialView, known bool) previewEntry {
	entry := previewEntry{Ref: ref, State: credentialState(credential, known)}
	if known {
		entry.Source, entry.Provider = credential.Source, credential.Provider
		entry.DisplayName, entry.DisplayMsg = credential.DisplayName, credential.DisplayMessage
		entry.CPAPriority, entry.Websockets, entry.NextRetry = credential.Priority, credential.Websockets, credential.NextRetry
	}
	return entry
}

func (a *App) previewRoute(req ManagementRequest) ManagementResponse {
	var body struct {
		ID     string            `json:"id"`
		Name   string            `json:"name"`
		Rule   billing.RouteRule `json:"rule"`
		Scopes []string          `json:"scopes"`
	}
	if errDecode := decodeStrict(req.Body, &body); errDecode != nil {
		return errorResponse(errDecode)
	}
	rule, errRule := billing.NormalizeRouteRule(body.Rule)
	if errRule != nil {
		return errorResponse(errRule)
	}
	draft := billing.Route{ID: strings.TrimSpace(body.ID), Name: strings.TrimSpace(body.Name), Rule: rule}
	if draft.ID == "" {
		draft.ID = previewDraftRouteID
	}
	if draft.Name == "" {
		draft.Name = draft.ID
	}
	hints := previewHints{SessionTTLMinutes: int(a.store.RouteSessionTTL() / time.Minute)}
	if errRefresh := a.refreshCredentialInventory(); errRefresh != nil {
		hints.InventoryIncomplete = true
	}
	a.routingMu.Lock()
	inventory := make(map[string]credentialView, len(a.credentials))
	for ref, credential := range a.credentials {
		inventory[ref] = credential
	}
	a.routingMu.Unlock()
	signal := a.hostTierSignal()
	hints.HostAcrossObserved = signal.AcrossObserved
	hints.HostTopTierOnly = !signal.AcrossObserved && !signal.SuspectAt.IsZero()

	// Rule-level hints: listed credentials that cannot be used, and Codex
	// WebSockets settings that differ among the credentials the rule allows.
	ruleOnly := billing.RoutingDecision{RouteRule: rule}
	if rule.CredentialOrder != nil {
		for _, item := range rule.CredentialOrder.Items {
			credential, known := inventory[item.Ref]
			if entry := a.previewEntry(item.Ref, credential, known); entry.State == "missing" || entry.State == "disabled" {
				hints.OrderUnusable = append(hints.OrderUnusable, entry)
			}
		}
	}
	for _, ref := range sortedRefs(inventory) {
		credential := inventory[ref]
		if credential.Provider != "codex" || credential.Disabled || !ruleOnly.AllowsCredential(ref, credential.Source, credential.Provider) {
			continue
		}
		entry := a.previewEntry(ref, credential, true)
		if credential.Websockets {
			hints.WebsocketsOn = append(hints.WebsocketsOn, entry)
		} else {
			hints.WebsocketsOff = append(hints.WebsocketsOff, entry)
		}
	}
	hints.WebsocketsMismatch = len(hints.WebsocketsOn) > 0 && len(hints.WebsocketsOff) > 0
	if !hints.WebsocketsMismatch {
		hints.WebsocketsOn, hints.WebsocketsOff = nil, nil
	}

	keys := make([]previewKey, 0, len(body.Scopes))
	for _, scope := range body.Scopes {
		decision, found := a.store.PreviewRouting(scope, draft, true)
		row := previewKey{Scope: scope, Found: found, RuleNames: decision.RuleNames}
		if !found {
			keys = append(keys, row)
			continue
		}
		if decision.ConfigurationError != "" {
			row.Error = decision.ConfigurationError
			keys = append(keys, row)
			continue
		}
		row.Custom, row.Legacy = decision.Order.Custom, decision.Order.Legacy
		row.SameTier, row.FallbackReturn, row.SettingsRule = decision.Order.SameTier, decision.Order.FallbackReturn, decision.Order.SettingsRule
		row.Conflicts = decision.Order.Conflicts
		row.Unrestricted = !decision.ControlsCredentials()
		var candidates []SchedulerAuthCandidate
		for _, ref := range sortedRefs(inventory) {
			credential := inventory[ref]
			if decision.AllowsCredential(ref, credential.Source, credential.Provider) {
				candidates = append(candidates, SchedulerAuthCandidate{ID: ref, Provider: credential.Provider, Priority: credential.Priority})
			} else if containsRef(decision.DeniedCredentialIDs, ref) || deniedByCategory(decision, credential) {
				row.Denied = append(row.Denied, a.previewEntry(ref, credential, true))
			}
		}
		for ref := range decision.Order.Entries {
			if _, known := inventory[ref]; !known {
				candidates = append(candidates, SchedulerAuthCandidate{ID: ref})
			}
		}
		ranked := rankCandidates(candidates, func(c SchedulerAuthCandidate) string { return c.ID }, decision.Order)
		first := map[string]bool{}
		for i, item := range ranked {
			credential, known := inventory[item.Ref]
			entry := a.previewEntry(item.Ref, credential, known)
			entry.Rank, entry.Listed = tierOf(ranked, i), item.Listed
			if item.Listed {
				entry.RuleName = item.Rank.RuleName
			}
			if entry.State == "ok" && !first[entry.Provider] {
				entry.CurrentFirst, first[entry.Provider] = true, true
			}
			row.Order = append(row.Order, entry)
		}
		keys = append(keys, row)
	}
	return JSONResponse(http.StatusOK, map[string]any{"keys": keys, "hints": hints})
}

func sortedRefs(inventory map[string]credentialView) []string {
	refs := make([]string, 0, len(inventory))
	for ref := range inventory {
		refs = append(refs, ref)
	}
	slices.Sort(refs)
	return refs
}

func containsRef(values []string, ref string) bool {
	return slices.ContainsFunc(values, func(item string) bool { return strings.EqualFold(item, ref) })
}

func deniedByCategory(decision billing.RoutingDecision, credential credentialView) bool {
	return slices.Contains(decision.DeniedCredentialProviders, billing.CredentialProviderSelector{Source: credential.Source, Provider: credential.Provider})
}

// routeOrderStatus is the host signal for the routes page banner.
func (a *App) routeOrderStatus(_ ManagementRequest) ManagementResponse {
	signal := a.hostTierSignal()
	return JSONResponse(http.StatusOK, map[string]any{
		"host_across_observed": signal.AcrossObserved,
		"host_top_tier_only":   !signal.AcrossObserved && !signal.SuspectAt.IsZero(),
		"session_ttl_minutes":  int(a.store.RouteSessionTTL() / time.Minute),
	})
}
