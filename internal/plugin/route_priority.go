package plugin

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"

	"cpa-key-billing/internal/billing"
)

// per-rule credential priority, cross-tier fall-through and
// plugin-side session stickiness for keys whose credential the plugin picks.
//
// CPA (v8.0.28+, SchedulerAcrossPriorities) hands the plugin every available
// candidate of every priority tier. CPA has already removed disabled,
// cooling, quota-exhausted, model-unsupported and already-tried credentials,
// so choosing the first candidate in the rule's order is the fall-through.

const (
	maxStickySessions = 8192
	maxPickRecords    = 4096
	pickRecordMaxAge  = 2 * time.Minute
)

type stickyBinding struct {
	AuthID  string
	Expires time.Time
}

// pickRecord remembers why a credential was chosen, for the route log.
type pickRecord struct {
	Rank    int
	Mode    string
	Sticky  bool
	Session string // where the session id came from (a header or metadata name), never its value
	Skipped []string
	At      time.Time
}

type routeOrderState struct {
	mu      sync.Mutex
	sticky  map[string]stickyBinding
	picks   map[string]pickRecord
	tiers   hostTierSignal
	pickSeq uint64
}

// hostTierSignal detects a host that ignores SchedulerAcrossPriorities.
type hostTierSignal struct {
	AcrossObserved bool      `json:"across_observed"`
	SuspectAt      time.Time `json:"suspect_top_tier_only_at,omitzero"`
}

// rankedCandidate is one allowed candidate with its sort key.
type rankedCandidate struct {
	SchedulerAuthCandidate
	Ref      string
	Listed   bool
	Rank     billing.OrderRank
	tierKey  [3]int
	position int
}

func candidateRanking(candidates []SchedulerAuthCandidate, order billing.RoutingOrder) []rankedCandidate {
	return rankCandidates(candidates, func(c SchedulerAuthCandidate) string { return billing.CredentialFingerprint(c.ID) }, order)
}

func rankCandidates(candidates []SchedulerAuthCandidate, refOf func(SchedulerAuthCandidate) string, order billing.RoutingOrder) []rankedCandidate {
	out := make([]rankedCandidate, 0, len(candidates))
	for _, candidate := range candidates {
		ref := refOf(candidate)
		item := rankedCandidate{SchedulerAuthCandidate: candidate, Ref: ref}
		if rank, ok := order.Entries[ref]; ok && order.Custom {
			item.Listed, item.Rank = true, rank
			item.tierKey = [3]int{0, -rank.RulePriority, -rank.Priority}
			item.position = rank.Position
		} else {
			item.tierKey = [3]int{1, -candidate.Priority, 0}
		}
		out = append(out, item)
	}
	sort.SliceStable(out, func(i, j int) bool {
		a, b := out[i], out[j]
		if a.tierKey != b.tierKey {
			for k := range a.tierKey {
				if a.tierKey[k] != b.tierKey[k] {
					return a.tierKey[k] < b.tierKey[k]
				}
			}
		}
		if a.position != b.position {
			return a.position < b.position
		}
		return a.ID < b.ID
	})
	return out
}

func tierOf(ranked []rankedCandidate, i int) int {
	tier := 1
	for k := 1; k <= i; k++ {
		if ranked[k].tierKey != ranked[k-1].tierKey {
			tier++
		}
	}
	return tier
}

func topTier(ranked []rankedCandidate) []rankedCandidate {
	for i := 1; i < len(ranked); i++ {
		if ranked[i].tierKey != ranked[0].tierKey {
			return ranked[:i]
		}
	}
	return ranked
}

var sessionHeaders = []string{
	"x-claude-code-session-id", "session-id", "session_id", "x-session-id", "x-session-affinity", "conversation-id", "conversation_id",
}

var sessionMetadataKeys = []string{"execution_session_id", "canonical_session_id", "derived_session_id"}

// requestSessionID mirrors the explicit identities CPA's session affinity
// prefers. The plugin never sees the request body, so body-derived
// identities (prompt_cache_key, message hashes) are not available.
func requestSessionID(headers map[string][]string, metadata map[string]any) string {
	id, _ := requestSession(headers, metadata)
	return id
}

// requestSession returns the session id and the name of the header or
// metadata key it came from.
func requestSession(headers map[string][]string, metadata map[string]any) (string, string) {
	for _, name := range sessionHeaders {
		for key, values := range headers {
			if !strings.EqualFold(key, name) {
				continue
			}
			for _, value := range values {
				if value = strings.TrimSpace(value); value != "" {
					return name + ":" + value, "header " + name
				}
			}
		}
	}
	for _, key := range sessionMetadataKeys {
		if value := metadataString(metadata, key); value != "" {
			return key + ":" + value, "metadata " + key
		}
	}
	return "", ""
}

func stickyKey(scope, pool, session string) string {
	sum := sha256.Sum256([]byte(scope + "\x00" + pool + "\x00" + session))
	return hex.EncodeToString(sum[:16])
}

func (s *routeOrderState) lookup(key string, now time.Time) (string, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	binding, ok := s.sticky[key]
	if !ok || !now.Before(binding.Expires) {
		if ok {
			delete(s.sticky, key)
		}
		return "", false
	}
	return binding.AuthID, true
}

func (s *routeOrderState) bind(key, authID string, now time.Time, ttl time.Duration) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.sticky == nil {
		s.sticky = make(map[string]stickyBinding)
	}
	if _, exists := s.sticky[key]; !exists && len(s.sticky) >= maxStickySessions {
		var oldestKey string
		var oldest time.Time
		for k, v := range s.sticky {
			if !now.Before(v.Expires) {
				delete(s.sticky, k)
				continue
			}
			if oldestKey == "" || v.Expires.Before(oldest) {
				oldestKey, oldest = k, v.Expires
			}
		}
		if len(s.sticky) >= maxStickySessions && oldestKey != "" {
			delete(s.sticky, oldestKey)
		}
	}
	s.sticky[key] = stickyBinding{AuthID: authID, Expires: now.Add(ttl)}
}

func (s *routeOrderState) forget(key string) {
	s.mu.Lock()
	delete(s.sticky, key)
	s.mu.Unlock()
}

func pickRecordKey(scope, authID string) string { return scope + "\x00" + authID }

func (s *routeOrderState) recordPick(scope, authID string, record pickRecord) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.picks == nil {
		s.picks = make(map[string]pickRecord)
	}
	if len(s.picks) >= maxPickRecords {
		for k, v := range s.picks {
			if record.At.Sub(v.At) > pickRecordMaxAge {
				delete(s.picks, k)
			}
		}
		if len(s.picks) >= maxPickRecords {
			s.picks = make(map[string]pickRecord)
		}
	}
	s.picks[pickRecordKey(scope, authID)] = record
}

func (s *routeOrderState) recentPick(scope, authID string, now time.Time) (pickRecord, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	record, ok := s.picks[pickRecordKey(scope, authID)]
	if !ok || now.Sub(record.At) > pickRecordMaxAge {
		return pickRecord{}, false
	}
	return record, true
}

// observeTiers records whether the host hands over lower tiers.
func (a *App) observeTiers(req SchedulerPickRequest, now time.Time) {
	if len(req.Candidates) == 0 {
		return
	}
	distinct := map[int]struct{}{}
	present := map[string]struct{}{}
	providers := map[string]struct{}{}
	minPriority := req.Candidates[0].Priority
	for _, candidate := range req.Candidates {
		distinct[candidate.Priority] = struct{}{}
		present[billing.CredentialFingerprint(candidate.ID)] = struct{}{}
		providers[strings.ToLower(strings.TrimSpace(candidate.Provider))] = struct{}{}
		minPriority = min(minPriority, candidate.Priority)
	}
	suspect := false
	if len(distinct) == 1 {
		a.routingMu.Lock()
		for ref, credential := range a.credentials {
			if _, ok := providers[credential.Provider]; !ok {
				continue
			}
			if _, ok := present[ref]; ok || credential.Disabled || credential.Unavailable {
				continue
			}
			if credential.Priority < minPriority {
				suspect = true
				break
			}
		}
		a.routingMu.Unlock()
	}
	a.routeOrder.mu.Lock()
	if len(distinct) > 1 {
		a.routeOrder.tiers.AcrossObserved = true
	} else if suspect {
		a.routeOrder.tiers.SuspectAt = now
	}
	a.routeOrder.mu.Unlock()
}

func (a *App) hostTierSignal() hostTierSignal {
	a.routeOrder.mu.Lock()
	defer a.routeOrder.mu.Unlock()
	return a.routeOrder.tiers
}

// credentialState names why an inventory credential is not usable.
func credentialState(credential credentialView, known bool) string {
	switch {
	case !known:
		return "missing"
	case credential.Disabled:
		return "disabled"
	case credential.Unavailable, !credential.NextRetry.IsZero() && time.Now().Before(credential.NextRetry):
		return "unavailable"
	default:
		return "ok"
	}
}

func skipReason(state string) string {
	switch state {
	case "missing":
		return "credential no longer exists"
	case "disabled":
		return "disabled"
	case "unavailable":
		return "cooling down or quota exhausted"
	default:
		return "not offered by CPA for this request (cooling down or out of quota for this model, already tried, or model not supported)"
	}
}

// choose picks the credential among allowed candidates, applying the merged
// order, the same-tier strategy and session stickiness.
func (a *App) choose(scope string, req SchedulerPickRequest, decision billing.RoutingDecision, allowed []SchedulerAuthCandidate) (string, pickRecord) {
	now := a.store.Now()
	order := decision.Order
	if order.SameTier == billing.SameTierRoundRobin {
		// Weighted round-robin (the legacy behaviour) excludes non-positive weights.
		positive := allowed[:0:0]
		for _, candidate := range allowed {
			if candidateWeight(candidate) > 0 {
				positive = append(positive, candidate)
			}
		}
		if allowed = positive; len(allowed) == 0 {
			return "", pickRecord{}
		}
	}
	ranked := candidateRanking(allowed, order)
	pool := routingPoolKey(decision.Model, decision)
	record := pickRecord{Mode: billing.CredentialOrderModeCPA, At: now}
	if order.Custom {
		record.Mode = billing.CredentialOrderModeCustom
	}
	ttl := a.store.RouteSessionTTL()
	session := ""
	if ttl > 0 {
		session, record.Session = requestSession(req.Options.Headers, req.Options.Metadata)
	}
	key := ""
	selected := -1
	if session != "" {
		key = stickyKey(scope, pool, session)
		if bound, ok := a.routeOrder.lookup(key, now); ok {
			for i, item := range ranked {
				if item.ID != bound {
					continue
				}
				if item.tierKey == ranked[0].tierKey || order.FallbackReturn != billing.FallbackReturnImmediate {
					selected = i
					record.Sticky = true
				}
				break
			}
			if selected < 0 {
				a.routeOrder.forget(key)
			}
		}
	}
	if selected < 0 {
		top := topTier(ranked)
		if order.SameTier == billing.SameTierRoundRobin && len(top) > 1 {
			plain := make([]SchedulerAuthCandidate, len(top))
			for i := range top {
				plain[i] = top[i].SchedulerAuthCandidate
			}
			id := a.scheduler.pick(scope, pool, plain)
			for i := range top {
				if top[i].ID == id {
					selected = i
				}
			}
		}
		if selected < 0 {
			selected = 0
		}
	}
	chosen := ranked[selected]
	if key != "" {
		a.routeOrder.bind(key, chosen.ID, now, ttl)
	}
	record.Rank, record.Skipped = a.describeRank(decision, ranked, selected)
	return chosen.ID, record
}

// describeRank gives the chosen credential's 1-based position in the key's
// whole configured order and why the ones before it were skipped.
func (a *App) describeRank(decision billing.RoutingDecision, ranked []rankedCandidate, selected int) (int, []string) {
	chosen := ranked[selected]
	if !decision.Order.Custom {
		return tierOf(ranked, selected), nil
	}
	type planned struct {
		ref  string
		rank billing.OrderRank
	}
	plan := make([]planned, 0, len(decision.Order.Entries))
	for ref, rank := range decision.Order.Entries {
		plan = append(plan, planned{ref, rank})
	}
	sort.Slice(plan, func(i, j int) bool { return plan[i].rank.Position < plan[j].rank.Position })
	present := make(map[string]int, len(ranked))
	for i, item := range ranked {
		present[item.Ref] = i
	}
	a.routingMu.Lock()
	inventory := make(map[string]credentialView, len(plan))
	for _, item := range plan {
		if credential, ok := a.credentials[item.ref]; ok {
			inventory[item.ref] = credential
		}
	}
	a.routingMu.Unlock()
	var skipped []string
	rank := 0
	tierKey := func(r billing.OrderRank) [2]int { return [2]int{-r.RulePriority, -r.Priority} }
	var lastKey [2]int
	for i, item := range plan {
		if i == 0 || tierKey(item.rank) != lastKey {
			rank++
			lastKey = tierKey(item.rank)
		}
		if item.ref == chosen.Ref {
			return rank, skipped
		}
		if chosen.Listed && tierKey(item.rank) == tierKey(chosen.Rank) {
			continue
		}
		credential, known := inventory[item.ref]
		name := "credential " + shortCredentialRef(item.ref)
		if known {
			name = credentialLogName(credential)
		}
		if _, ok := present[item.ref]; ok {
			skipped = append(skipped, fmt.Sprintf("#%d %s: session kept on its current credential", rank, name))
			continue
		}
		skipped = append(skipped, fmt.Sprintf("#%d %s: %s", rank, name, skipReason(credentialState(credential, known))))
	}
	// Unlisted credential: ranks after every listed one.
	unlistedRank := tierOf(ranked, selected)
	firstUnlisted := 0
	for i, item := range ranked {
		if !item.Listed {
			firstUnlisted = i
			break
		}
	}
	return rank + unlistedRank - tierOf(ranked, firstUnlisted) + 1, skipped
}

// noMatchMessage explains a routing 503 without naming accounts: the body
// reaches API clients.
func (a *App) noMatchMessage(req SchedulerPickRequest, decision billing.RoutingDecision) string {
	providers := map[string]struct{}{}
	for _, provider := range append([]string{req.Provider}, req.Providers...) {
		if provider = strings.ToLower(strings.TrimSpace(provider)); provider != "" {
			providers[provider] = struct{}{}
		}
	}
	counts := map[string]int{}
	total := 0
	a.routingMu.Lock()
	for ref, credential := range a.credentials {
		if _, ok := providers[credential.Provider]; len(providers) > 0 && !ok {
			continue
		}
		if !decision.AllowsCredential(ref, credential.Source, credential.Provider) {
			continue
		}
		total++
		counts[credentialState(credential, true)]++
	}
	a.routingMu.Unlock()
	rules := ""
	if len(decision.RuleNames) > 0 {
		quoted := make([]string, len(decision.RuleNames))
		for i, name := range decision.RuleNames {
			quoted[i] = fmt.Sprintf("%q", boundedLogValue(name, 128))
		}
		rules = " (rules " + strings.Join(quoted, ", ") + ")"
	}
	if total == 0 {
		return noRoutedCredentialMessage + rules + ": the rules allow no upstream credential of this provider."
	}
	parts := []string{}
	if counts["unavailable"] > 0 {
		parts = append(parts, fmt.Sprintf("%d cooling down or quota exhausted", counts["unavailable"]))
	}
	if counts["disabled"] > 0 {
		parts = append(parts, fmt.Sprintf("%d disabled", counts["disabled"]))
	}
	if counts["ok"] > 0 {
		parts = append(parts, fmt.Sprintf("%d not offered by CPA for this request (cooling down or out of quota for this model, already tried, or model not supported)", counts["ok"]))
	}
	return fmt.Sprintf("%s%s: %d allowed upstream credential(s), none usable right now (%s). Retry after the cooldown or ask the administrator.",
		noRoutedCredentialMessage, rules, total, strings.Join(parts, ", "))
}
