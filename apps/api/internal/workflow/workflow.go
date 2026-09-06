// Package workflow provides the tenant-scoped, in-memory claim registry
// and the deterministic apply path for the ClaimOps application edge.
//
// It is stdlib-only by design: no HTTP, no I/O, no logging, no clock reads.
// Lifecycle semantics defer to the claims package (Transition); this package
// only adds storage, tenant isolation, and audit-event emission.
package workflow

import (
	"errors"
	"sync"

	"claimops-api/internal/claims"
)

// ErrNotFound is returned when a claim ID is absent from the store.
var ErrNotFound = errors.New("workflow: claim not found")

// ErrTenantMismatch is returned when the acting tenant does not own the claim.
var ErrTenantMismatch = errors.New("workflow: tenant mismatch")

// EventTypeTransitioned is the Type recorded for successful transitions.
// Replays ("claim.replay_duplicate") are never emitted: idempotent replays
// append nothing (golden case 09).
const EventTypeTransitioned = "claim.transitioned"

// Event is a single audit entry for a successful claim transition.
type Event struct {
	Seq     int
	Type    string
	ClaimID claims.ClaimID
	Tenant  claims.TenantID
	From    claims.ClaimStatus
	To      claims.ClaimStatus
	EventID string
}

// Store is a tenant-scoped in-memory claim registry with per-claim events.
// Both maps are keyed by the composite (tenant, claim) identifier so two
// tenants holding the same ClaimID can never observe or overwrite each
// other's claims or audit trails.
type Store struct {
	mu     sync.Mutex
	claims map[storeKey]claims.Claim
	events map[storeKey][]Event
}

// storeKey is the composite tenant-and-claim identifier.
type storeKey struct {
	tenant claims.TenantID
	id     claims.ClaimID
}

// New returns an empty Store ready for use.
func New() *Store {
	return &Store{
		claims: make(map[storeKey]claims.Claim),
		events: make(map[storeKey][]Event),
	}
}

// cloneClaim deep-copies the idempotency set so stored and returned values
// never alias through the shared map reference.
func cloneClaim(c claims.Claim) claims.Claim {
	if c.ProcessedEvents != nil {
		copied := make(map[string]bool, len(c.ProcessedEvents))
		for k, v := range c.ProcessedEvents {
			copied[k] = v
		}
		c.ProcessedEvents = copied
	}
	return c
}

// Put stores c under its composite (tenant, ID) key. It rejects a tenant
// mismatch with a plain error and mutates nothing on rejection.
func (s *Store) Put(tenant claims.TenantID, c claims.Claim) error {
	if c.Tenant != tenant {
		return ErrTenantMismatch
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.claims[storeKey{tenant: tenant, id: c.ID}] = cloneClaim(c)
	return nil
}

// Get returns a copy of the claim when the composite (tenant, ID) key
// exists. Cross-tenant or missing lookups report (zero, false): the key
// itself enforces isolation, so another tenant's ClaimID is simply absent.
func (s *Store) Get(tenant claims.TenantID, id claims.ClaimID) (claims.Claim, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	stored, ok := s.claims[storeKey{tenant: tenant, id: id}]
	if !ok {
		return claims.Claim{}, false
	}
	return cloneClaim(stored), true
}

// Events returns a copy of the audit trail for the composite (tenant, ID)
// key. Cross-tenant or missing lookups return nil.
func (s *Store) Events(tenant claims.TenantID, id claims.ClaimID) []Event {
	s.mu.Lock()
	defer s.mu.Unlock()
	k := storeKey{tenant: tenant, id: id}
	if _, ok := s.claims[k]; !ok {
		return nil
	}
	list := s.events[k]
	if len(list) == 0 {
		return nil
	}
	out := make([]Event, len(list))
	copy(out, list)
	return out
}

// Apply loads the claim, runs claims.Transition, and on success persists the
// new claim version and appends one Event of type "claim.transitioned".
// An idempotent replay (input claim already saw eventID, version unchanged)
// returns the claim without appending an event. Invalid operations return an
// error and mutate nothing: no event appended, no put performed.
func Apply(tenant claims.TenantID, s *Store, id claims.ClaimID, to claims.ClaimStatus, eventID string, expectedVersion int) (claims.Claim, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	k := storeKey{tenant: tenant, id: id}
	stored, ok := s.claims[k]
	if !ok {
		// Distinguish "absent for this tenant" from "owned by another
		// tenant" without leaking which: both report not-found unless a
		// same-ID claim exists elsewhere, which is a tenant mismatch.
		for existing := range s.claims {
			if existing.id == id {
				return claims.Claim{}, ErrTenantMismatch
			}
		}
		return claims.Claim{}, ErrNotFound
	}
	from := stored.Status
	replay := stored.HasEvent(eventID)
	next, err := claims.Transition(stored, to, eventID, expectedVersion, tenant)
	if err != nil {
		return claims.Claim{}, err
	}
	if replay && next.Version == stored.Version {
		return next, nil
	}
	evt := Event{
		Seq:     len(s.events[k]) + 1,
		Type:    EventTypeTransitioned,
		ClaimID: id,
		Tenant:  tenant,
		From:    from,
		To:      next.Status,
		EventID: eventID,
	}
	s.events[k] = append(s.events[k], evt)
	s.claims[k] = cloneClaim(next)
	return next, nil
}
