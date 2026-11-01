package billing

import (
	"fmt"
	"os"
	"sort"
	"strings"
	"time"
)

const MaxConcurrencyLimit = 10000

// DefaultConcurrencyStaleMinutes is how long a slot may go without any
// lifecycle activity before it is presumed leaked and reclaimed.
const DefaultConcurrencyStaleMinutes = 10

// MaxConcurrencyStaleMinutes bounds the configurable stale window (one day).
const MaxConcurrencyStaleMinutes = 1440

const (
	SlotTransportHTTP      = "http"
	SlotTransportWebSocket = "ws"
)

// SlotDecision reports the result of reserving one generation concurrency
// slot. Active is the number of slots already occupied when a request is
// refused, or the number occupied after a successful reservation.
type SlotDecision struct {
	Allowed  bool
	Acquired bool
	Limit    int
	Active   int
}

// SlotInfo describes an admitted request for the slot list.
type SlotInfo struct {
	Transport string
	Path      string
	Model     string
}

// concurrencySlot is one process-local reservation, keyed by the host
// request ID. Start and LastActivity let stale reservations be reclaimed
// when a completion event never arrives.
type concurrencySlot struct {
	Scope        string
	Start        time.Time
	LastActivity time.Time
	SlotInfo
}

// SlotView is the management view of one reservation.
type SlotView struct {
	RequestID    string    `json:"request_id"`
	StartedAt    time.Time `json:"started_at"`
	LastActivity time.Time `json:"last_activity_at"`
	Transport    string    `json:"transport"`
	Path         string    `json:"path,omitempty"`
	Model        string    `json:"model,omitempty"`
}

type reclaimedSlot struct {
	RequestID string
	concurrencySlot
}

// AcquireSlot atomically checks and reserves one API-key-scoped generation
// slot. Every attributable generation is tracked even while its limit is zero,
// so lowering an unlimited key to a finite limit immediately accounts for
// requests that were already running.
func (s *Store) AcquireSlot(scope, requestID string) SlotDecision {
	return s.AcquireSlotWith(scope, requestID, SlotInfo{})
}

// AcquireSlotWith is AcquireSlot with descriptive request details. Stale
// reservations are reclaimed first, so a leaked slot cannot block admission
// for longer than the configured stale window.
func (s *Store) AcquireSlotWith(scope, requestID string, info SlotInfo) SlotDecision {
	decision := SlotDecision{Allowed: true}
	scope = strings.TrimSpace(scope)
	requestID = strings.TrimSpace(requestID)
	if scope == "" {
		return decision
	}
	now := s.Now()

	s.mu.Lock()
	reclaimed := s.reclaimStaleSlotsLocked(now)
	decision = s.acquireSlotLocked(scope, requestID, info, now)
	s.mu.Unlock()

	s.logReclaimedSlots(reclaimed, now)
	return decision
}

func (s *Store) acquireSlotLocked(scope, requestID string, info SlotInfo, now time.Time) SlotDecision {
	decision := SlotDecision{Allowed: true}
	if key := s.state.Keys[scope]; key != nil {
		decision.Limit = key.ConcurrencyLimit
	}
	decision.Active = s.activeByScope[scope]
	if requestID == "" {
		// A finite limit cannot be enforced safely without an exact completion
		// correlation. Current compatible hosts always provide RequestID.
		decision.Allowed = decision.Limit <= 0
		return decision
	}
	if existing, exists := s.activeRequests[requestID]; exists {
		decision.Allowed = existing.Scope == scope
		// The original admission owns this reservation. Reporting a duplicate
		// as newly acquired would let a later admission check roll it back.
		decision.Acquired = false
		return decision
	}
	if decision.Limit > 0 && decision.Active >= decision.Limit {
		decision.Allowed = false
		return decision
	}
	if info.Transport == "" {
		info.Transport = SlotTransportHTTP
	}
	s.activeRequests[requestID] = &concurrencySlot{Scope: scope, Start: now, LastActivity: now, SlotInfo: info}
	s.activeByScope[scope] = decision.Active + 1
	decision.Active++
	decision.Acquired = true
	return decision
}

// ReleaseSlot idempotently releases the slot reserved for requestID. Unknown
// and duplicate completion events are harmless.
func (s *Store) ReleaseSlot(requestID string) bool {
	requestID = strings.TrimSpace(requestID)
	if requestID == "" {
		return false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.releaseSlotLocked(requestID)
}

func (s *Store) releaseSlotLocked(requestID string) bool {
	slot, exists := s.activeRequests[requestID]
	if !exists {
		return false
	}
	delete(s.activeRequests, requestID)
	if active := s.activeByScope[slot.Scope]; active > 1 {
		s.activeByScope[slot.Scope] = active - 1
	} else {
		delete(s.activeByScope, slot.Scope)
	}
	return true
}

// TouchSlot records lifecycle activity for a running request (for example
// the post-auth interception), postponing stale reclamation.
func (s *Store) TouchSlot(requestID string) bool {
	requestID = strings.TrimSpace(requestID)
	if requestID == "" {
		return false
	}
	now := s.Now()
	s.mu.Lock()
	defer s.mu.Unlock()
	slot, exists := s.activeRequests[requestID]
	if !exists {
		return false
	}
	slot.LastActivity = now
	return true
}

// ResetSlots drops every reservation held by one API key and returns how many
// were removed. Running requests are not cancelled; their later completion
// events are ignored as unknown.
func (s *Store) ResetSlots(scope string) (int, error) {
	scope = normalizeScope(scope)
	if scope == "" {
		return 0, invalidf("API key identifier is required")
	}
	s.mu.Lock()
	if s.state.Keys[scope] == nil {
		s.mu.Unlock()
		return 0, notFoundf("API key %q does not exist", scope)
	}
	removed := 0
	for id, slot := range s.activeRequests {
		if slot.Scope == scope {
			s.releaseSlotLocked(id)
			removed++
		}
	}
	s.mu.Unlock()
	if removed > 0 {
		s.logSlot("Concurrency slots reset for API key %s: %d released", previewScope(scope), removed)
	}
	return removed, nil
}

// ReclaimStaleSlots releases reservations without activity for longer than
// the stale window and returns how many were released.
func (s *Store) ReclaimStaleSlots() int {
	now := s.Now()
	s.mu.Lock()
	reclaimed := s.reclaimStaleSlotsLocked(now)
	s.mu.Unlock()
	s.logReclaimedSlots(reclaimed, now)
	return len(reclaimed)
}

// SlotViews lists one key's reservations, oldest first. Caller must not hold s.mu.
func (s *Store) SlotViews(scope string) []SlotView {
	scope = normalizeScope(scope)
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.slotViewsLocked(scope)
}

func (s *Store) slotViewsLocked(scope string) []SlotView {
	views := []SlotView{}
	for id, slot := range s.activeRequests {
		if slot.Scope != scope {
			continue
		}
		views = append(views, SlotView{RequestID: id, StartedAt: slot.Start, LastActivity: slot.LastActivity,
			Transport: slot.Transport, Path: slot.Path, Model: slot.Model})
	}
	sort.Slice(views, func(i, j int) bool {
		if !views[i].StartedAt.Equal(views[j].StartedAt) {
			return views[i].StartedAt.Before(views[j].StartedAt)
		}
		return views[i].RequestID < views[j].RequestID
	})
	return views
}

// StaleSlotWindow is the configured stale window; zero disables reclamation.
func (s *Store) StaleSlotWindow() time.Duration {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.staleSlotWindowLocked()
}

func (s *Store) staleSlotWindowLocked() time.Duration {
	minutes := s.cfg.ConcurrencyStaleMinutes
	if minutes <= 0 {
		return 0
	}
	return time.Duration(minutes) * time.Minute
}

func (s *Store) reclaimStaleSlotsLocked(now time.Time) []reclaimedSlot {
	window := s.staleSlotWindowLocked()
	if window <= 0 {
		return nil
	}
	var reclaimed []reclaimedSlot
	for id, slot := range s.activeRequests {
		if now.Sub(slot.LastActivity) > window {
			reclaimed = append(reclaimed, reclaimedSlot{RequestID: id, concurrencySlot: *slot})
			s.releaseSlotLocked(id)
		}
	}
	return reclaimed
}

func (s *Store) logReclaimedSlots(reclaimed []reclaimedSlot, now time.Time) {
	for _, slot := range reclaimed {
		s.logSlot("Reclaimed stale concurrency slot for API key %s: request %s, %s %s %s, started %s, last activity %s (%s idle); no completion event arrived",
			previewScope(slot.Scope), shortRequestID(slot.RequestID), slot.Transport, slot.Path, slot.Model,
			slot.Start.Format(time.RFC3339), slot.LastActivity.Format(time.RFC3339), now.Sub(slot.LastActivity).Round(time.Second))
	}
}

func (s *Store) logSlot(format string, args ...any) {
	message := fmt.Sprintf(format, args...)
	fmt.Fprintf(os.Stderr, "[cpa-suite] WARN %s\n", message)
	s.AddPluginLog(PluginLogInfo, "%s", message)
}

func previewScope(scope string) string {
	if len(scope) > 12 {
		return scope[:12]
	}
	return scope
}

func shortRequestID(id string) string {
	if len(id) > 8 {
		return id[:8]
	}
	return id
}

func (s *Store) SetConcurrencyLimit(scope string, limit int) error {
	scope = normalizeScope(scope)
	if scope == "" {
		return invalidf("API key identifier is required")
	}
	if limit < 0 || limit > MaxConcurrencyLimit {
		return invalidf("Concurrency limit must be an integer from 0 to %d", MaxConcurrencyLimit)
	}

	_, err := editConfiguration(s, func(state *State) (struct{}, Changes, error) {
		key := state.liveKey(scope)
		if key == nil {
			return struct{}{}, Changes{}, notFoundf("API key %q does not exist", scope)
		}
		if key.ConcurrencyLimit == limit {
			return struct{}{}, Changes{}, nil
		}
		key.ConcurrencyLimit = limit
		return struct{}{}, Changes{Keys: []string{scope}}, nil
	})
	return err
}
