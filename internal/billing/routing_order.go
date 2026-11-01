package billing

import (
	"slices"
	"sort"
	"strings"
)

// per-rule credential priority.
//
// A rule may carry a credential_order inside its rule_json. Old rules have no
// such field and keep the legacy behaviour ("cpa" mode, weighted round-robin
// within the highest allowed CPA priority tier). No SQLite schema change is
// involved: older builds ignore the unknown JSON field.

const (
	CredentialOrderModeCPA    = "cpa"
	CredentialOrderModeCustom = "custom"

	SameTierFillFirst  = "fill-first"
	SameTierRoundRobin = "round-robin"

	FallbackReturnNewSession = "new-session"
	FallbackReturnImmediate  = "immediate"

	maxCredentialOrderItems = 256
	maxOrderPriority        = 1_000_000
)

type CredentialOrderItem struct {
	Ref      string `json:"ref"`
	Priority int    `json:"priority"`
}

type CredentialOrder struct {
	Mode           string                `json:"mode"`
	RulePriority   int                   `json:"rule_priority"`
	Items          []CredentialOrderItem `json:"items,omitempty"`
	SameTier       string                `json:"same_tier"`
	FallbackReturn string                `json:"fallback_return"`
}

func (o *CredentialOrder) clone() *CredentialOrder {
	if o == nil {
		return nil
	}
	copyOrder := *o
	copyOrder.Items = append([]CredentialOrderItem(nil), o.Items...)
	return &copyOrder
}

func normalizeCredentialOrder(order *CredentialOrder, rule RouteRule) (*CredentialOrder, error) {
	if order == nil {
		return nil, nil
	}
	out := order.clone()
	out.Mode = strings.ToLower(strings.TrimSpace(out.Mode))
	if out.Mode == "" {
		out.Mode = CredentialOrderModeCPA
	}
	if out.Mode != CredentialOrderModeCPA && out.Mode != CredentialOrderModeCustom {
		return nil, invalidf("Credential order mode must be cpa or custom")
	}
	out.SameTier = strings.ToLower(strings.TrimSpace(out.SameTier))
	if out.SameTier == "" {
		out.SameTier = SameTierFillFirst
	}
	if out.SameTier != SameTierFillFirst && out.SameTier != SameTierRoundRobin {
		return nil, invalidf("Same-level credential strategy must be fill-first or round-robin")
	}
	out.FallbackReturn = strings.ToLower(strings.TrimSpace(out.FallbackReturn))
	if out.FallbackReturn == "" {
		out.FallbackReturn = FallbackReturnNewSession
	}
	if out.FallbackReturn != FallbackReturnNewSession && out.FallbackReturn != FallbackReturnImmediate {
		return nil, invalidf("Recovery behaviour must be new-session or immediate")
	}
	if out.RulePriority < -maxOrderPriority || out.RulePriority > maxOrderPriority {
		return nil, invalidf("Rule priority must be between -1000000 and 1000000")
	}
	if out.Mode == CredentialOrderModeCPA {
		out.Items = nil
		return out, nil
	}
	if len(out.Items) > maxCredentialOrderItems {
		return nil, invalidf("A credential order can list at most 256 credentials")
	}
	seen := make(map[string]struct{}, len(out.Items))
	onlyListed := len(rule.CredentialIDs) > 0 && len(rule.CredentialProviders) == 0
	for i := range out.Items {
		ref := strings.ToLower(strings.TrimSpace(out.Items[i].Ref))
		if !ValidCredentialFingerprint(ref) {
			return nil, invalidf("Invalid upstream credential reference")
		}
		if _, ok := seen[ref]; ok {
			return nil, invalidf("A credential appears twice in the credential order")
		}
		seen[ref] = struct{}{}
		if containsRouteValue(rule.DeniedCredentialIDs, ref) {
			return nil, invalidf("A denied credential cannot be placed in the credential order")
		}
		if onlyListed && !containsRouteValue(rule.CredentialIDs, ref) {
			return nil, invalidf("Only credentials allowed by this rule can be placed in the credential order")
		}
		if out.Items[i].Priority < -maxOrderPriority || out.Items[i].Priority > maxOrderPriority {
			return nil, invalidf("Credential priority must be between -1000000 and 1000000")
		}
		out.Items[i].Ref = ref
	}
	return out, nil
}

// OrderRank is where one credential sits in a key's effective custom order.
type OrderRank struct {
	RulePriority int    `json:"rule_priority"`
	Priority     int    `json:"priority"`
	Position     int    `json:"position"`
	RuleID       string `json:"rule_id"`
	RuleName     string `json:"rule_name"`
}

// OrderConflict reports a credential ranked by more than one bound rule.
type OrderConflict struct {
	Ref   string   `json:"ref"`
	Rules []string `json:"rules"`
	// Winner is the rule whose position is used.
	Winner string `json:"winner"`
}

// RoutingOrder is the merged credential order of every rule bound to a key.
type RoutingOrder struct {
	// Custom is true when at least one bound rule uses a custom order.
	Custom bool `json:"custom"`
	// Legacy is true when no bound rule carries credential_order at all.
	Legacy         bool                 `json:"legacy"`
	Entries        map[string]OrderRank `json:"entries,omitempty"`
	SameTier       string               `json:"same_tier"`
	FallbackReturn string               `json:"fallback_return"`
	SettingsRule   string               `json:"settings_rule,omitempty"`
	Conflicts      []OrderConflict      `json:"conflicts,omitempty"`
}

type orderedRoute struct {
	route Route
	index int
}

func mergeRoutingOrder(routes []Route) RoutingOrder {
	result := RoutingOrder{Legacy: true, SameTier: SameTierRoundRobin, FallbackReturn: FallbackReturnNewSession}
	ordered := make([]orderedRoute, 0, len(routes))
	for i, route := range routes {
		if route.Rule.CredentialOrder != nil {
			ordered = append(ordered, orderedRoute{route: route, index: i})
		}
	}
	if len(ordered) == 0 {
		return result
	}
	sort.SliceStable(ordered, func(i, j int) bool {
		return ordered[i].route.Rule.CredentialOrder.RulePriority > ordered[j].route.Rule.CredentialOrder.RulePriority
	})
	first := ordered[0].route
	result.Legacy = false
	result.SameTier = first.Rule.CredentialOrder.SameTier
	result.FallbackReturn = first.Rule.CredentialOrder.FallbackReturn
	result.SettingsRule = first.Name
	if result.SameTier == "" {
		result.SameTier = SameTierFillFirst
	}
	if result.FallbackReturn == "" {
		result.FallbackReturn = FallbackReturnNewSession
	}
	owners := map[string][]string{}
	position := 0
	for _, item := range ordered {
		order := item.route.Rule.CredentialOrder
		if order.Mode != CredentialOrderModeCustom {
			continue
		}
		result.Custom = true
		if result.Entries == nil {
			result.Entries = make(map[string]OrderRank)
		}
		for _, entry := range order.Items {
			owners[entry.Ref] = append(owners[entry.Ref], item.route.Name)
			if _, exists := result.Entries[entry.Ref]; exists {
				continue
			}
			result.Entries[entry.Ref] = OrderRank{
				RulePriority: order.RulePriority, Priority: entry.Priority, Position: position,
				RuleID: item.route.ID, RuleName: item.route.Name,
			}
			position++
		}
	}
	refs := make([]string, 0, len(owners))
	for ref, names := range owners {
		if len(names) > 1 {
			refs = append(refs, ref)
		}
	}
	slices.Sort(refs)
	for _, ref := range refs {
		result.Conflicts = append(result.Conflicts, OrderConflict{Ref: ref, Rules: owners[ref], Winner: result.Entries[ref].RuleName})
	}
	return result
}

// PreviewRouting resolves scope's routing with draft in place of the stored
// rule of the same ID (or added when the key is newly bound to it). bind
// reports whether the draft is bound to the key in the preview.
func (s *Store) PreviewRouting(scope string, draft Route, bind bool) (RoutingDecision, bool) {
	var decision RoutingDecision
	found := false
	s.read(func(state *State) {
		key := state.liveKey(normalizeScope(scope))
		if key == nil {
			return
		}
		found = true
		copyKey := *key
		copyKey.RouteBindings = key.RouteBindings.clone()
		ids := slices.DeleteFunc(copyKey.RouteBindings.RouteIDs, func(id string) bool { return id == draft.ID })
		if bind {
			ids = append(ids, draft.ID)
		}
		copyKey.RouteBindings.RouteIDs = ids
		preview := *state
		preview.Routes = slices.DeleteFunc(slices.Clone(state.Routes), func(route Route) bool { return route.ID == draft.ID })
		preview.Routes = append(preview.Routes, draft)
		decision = resolveRoutingState(&preview, &copyKey)
	})
	return decision, found
}
