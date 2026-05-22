package main

import (
	"errors"
	"fmt"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/molecule"
	"github.com/gastownhall/gascity/internal/session"
)

// wispGC performs mechanical garbage collection of closed GC-owned beads that
// have exceeded their retention policy. Follows the nil-guard tracker pattern
// used by crashTracker and idleTracker: nil means disabled.
type wispGC interface {
	// shouldRun returns true if enough time has elapsed since the last run.
	shouldRun(now time.Time) bool

	// runGC lists closed policy-owned beads, deletes those past retention, and
	// returns the count of purged entries. Errors from individual deletes are
	// best-effort and surfaced without stopping the purge; the returned error
	// also covers list failures.
	runGC(store beads.Store, now time.Time) (int, error)
}

// memoryWispGC is the production implementation of wispGC.
type memoryWispGC struct {
	interval time.Duration
	policies []beadGCPolicy
	lastRun  time.Time
}

type beadGCPolicy struct {
	name     string
	ttl      time.Duration
	queries  []beads.ListQuery
	deleteFn func(beads.Store, string) error
}

const (
	beadPolicyWisp          = "wisp"
	beadPolicyOrderTracking = "order_tracking"
	beadPolicySession       = "session"
	beadPolicyWait          = "wait"
	beadPolicyNudge         = "nudge"
)

// newWispGC creates a legacy-compatible wisp GC tracker. Returns nil if
// disabled (interval or TTL is zero). Callers nil-guard before use.
func newWispGC(interval, ttl time.Duration) wispGC {
	if interval <= 0 || ttl <= 0 {
		return nil
	}
	return newWispGCWithPolicies(interval, legacyWispGCPolicies(ttl))
}

func newWispGCFromConfig(cfg *config.City) wispGC {
	if cfg == nil {
		return nil
	}
	interval := cfg.Daemon.WispGCIntervalDuration()
	if interval <= 0 {
		return nil
	}
	policies := configuredWispGCPolicies(cfg.Daemon.WispTTLDuration(), cfg.Beads.Policies)
	return newWispGCWithPolicies(interval, policies)
}

func newWispGCWithPolicies(interval time.Duration, policies []beadGCPolicy) wispGC {
	if interval <= 0 || len(policies) == 0 {
		return nil
	}
	return &memoryWispGC{
		interval: interval,
		policies: policies,
	}
}

func legacyWispGCPolicies(ttl time.Duration) []beadGCPolicy {
	return []beadGCPolicy{
		{
			name: beadPolicyWisp,
			ttl:  ttl,
			queries: []beads.ListQuery{
				{
					Status: "closed",
					Label:  molecule.WispLabel,
				},
				{
					Status: "closed",
					Type:   "molecule",
				},
			},
			deleteFn: deleteExpiredBeadClosure,
		},
		{
			name: beadPolicyOrderTracking,
			ttl:  ttl,
			queries: []beads.ListQuery{
				{
					Status: "closed",
					Label:  labelOrderTracking,
				},
				{
					Status: "closed",
					Label:  legacyLabelOrderTracking,
				},
			},
			deleteFn: deleteWorkflowBead,
		},
	}
}

func configuredWispGCPolicies(defaultTTL time.Duration, overrides map[string]config.BeadPolicyConfig) []beadGCPolicy {
	specs := []beadGCPolicy{
		legacyWispGCPolicies(defaultTTL)[0],
		legacyWispGCPolicies(defaultTTL)[1],
		{
			name: beadPolicySession,
			queries: []beads.ListQuery{
				{
					Status: "closed",
					Type:   session.BeadType,
					Label:  session.LabelSession,
				},
			},
			deleteFn: deleteWorkflowBead,
		},
		{
			name: beadPolicyWait,
			queries: []beads.ListQuery{
				{
					Status: "closed",
					Label:  session.WaitBeadLabel,
				},
			},
			deleteFn: deleteWorkflowBead,
		},
		{
			name: beadPolicyNudge,
			queries: []beads.ListQuery{
				{
					Status: "closed",
					Label:  nudgeBeadLabel,
				},
			},
			deleteFn: deleteWorkflowBead,
		},
	}
	policies := make([]beadGCPolicy, 0, len(specs))
	for _, spec := range specs {
		ttl := effectiveBeadGCPolicyTTL(spec.name, defaultTTL, overrides)
		if ttl <= 0 {
			continue
		}
		spec.ttl = ttl
		policies = append(policies, spec)
	}
	return policies
}

func effectiveBeadGCPolicyTTL(name string, defaultTTL time.Duration, overrides map[string]config.BeadPolicyConfig) time.Duration {
	if policy, ok := overrides[name]; ok && policy.DeleteAfterClose != "" {
		return policy.DeleteAfterCloseDuration()
	}
	switch name {
	case beadPolicyWisp, beadPolicyOrderTracking:
		return defaultTTL
	default:
		return 0
	}
}

func (m *memoryWispGC) shouldRun(now time.Time) bool {
	return now.Sub(m.lastRun) >= m.interval
}

func (m *memoryWispGC) runGC(store beads.Store, now time.Time) (int, error) {
	m.lastRun = now
	if store == nil {
		return 0, fmt.Errorf("listing closed GC policy beads: bead store unavailable")
	}

	purged := 0
	var runErr error
	for _, policy := range m.policies {
		cutoff := now.Add(-policy.ttl)
		entries, err := policy.list(store, cutoff)
		if err != nil {
			runErr = errors.Join(runErr, fmt.Errorf("listing closed %s beads: %w", policy.name, err))
			continue
		}
		policyPurged, deleteErr := purgeExpiredBeads(store, entries, cutoff, policy.deleteFn)
		purged += policyPurged
		runErr = errors.Join(runErr, deleteErr)
	}

	return purged, runErr
}

func (p beadGCPolicy) list(store beads.Store, cutoff time.Time) ([]beads.Bead, error) {
	entries := make([]beads.Bead, 0)
	seen := make(map[string]struct{})
	reader := beads.HandlesFor(store).Live
	for _, query := range p.queries {
		query.ClosedBefore = cutoff
		items, err := reader.List(query)
		if err != nil {
			return nil, err
		}
		for _, item := range items {
			if item.ID == "" {
				continue
			}
			if _, ok := seen[item.ID]; ok {
				continue
			}
			seen[item.ID] = struct{}{}
			entries = append(entries, item)
		}
	}
	return entries, nil
}

func purgeExpiredBeads(store beads.Store, entries []beads.Bead, cutoff time.Time, deleteFn func(beads.Store, string) error) (int, error) {
	purged := 0
	var deleteErr error
	for _, entry := range entries {
		if entry.ClosedAt.IsZero() || !entry.ClosedAt.Before(cutoff) {
			continue
		}
		if err := deleteFn(store, entry.ID); err != nil {
			deleteErr = errors.Join(deleteErr, fmt.Errorf("deleting expired bead %q: %w", entry.ID, err))
			continue
		}
		purged++
	}
	return purged, deleteErr
}

func deleteExpiredBeadClosure(store beads.Store, rootID string) error {
	// deleteWorkflowBead removes every dependency attached to each closure
	// member before deleting the bead. Only use the closure deleter for roots
	// whose full ownership tree is safe to collect.
	ids, err := collectExpiredBeadClosure(store, rootID)
	if err != nil {
		return err
	}
	for _, id := range ids {
		if err := deleteWorkflowBead(store, id); err != nil {
			return err
		}
	}
	return nil
}

func collectExpiredBeadClosure(store beads.Store, rootID string) ([]string, error) {
	if store == nil {
		return nil, fmt.Errorf("bead store unavailable")
	}
	rootOwned := make([]string, 0, 4)
	related, err := store.List(beads.ListQuery{
		Metadata:      map[string]string{"gc.root_bead_id": rootID},
		IncludeClosed: true,
	})
	if err != nil {
		return nil, fmt.Errorf("list workflow-owned beads for %s: %w", rootID, err)
	}
	for _, bead := range related {
		if bead.ID != "" && bead.ID != rootID {
			rootOwned = append(rootOwned, bead.ID)
		}
	}

	seen := make(map[string]struct{}, len(rootOwned)+1)
	ids := make([]string, 0, len(rootOwned)+1)
	var visit func(string) error
	visit = func(id string) error {
		if id == "" {
			return nil
		}
		if _, ok := seen[id]; ok {
			return nil
		}
		seen[id] = struct{}{}

		if id == rootID {
			for _, relatedID := range rootOwned {
				if err := visit(relatedID); err != nil {
					return err
				}
			}
		}

		// Treat structural parentage as workflow ownership. Some molecule step
		// beads are linked only by ParentID / parent-child deps and do not carry
		// gc.root_bead_id metadata, so GC must follow those ownership edges while
		// still ignoring non-ownership deps such as blocks or waits-for.
		children, err := store.Children(id, beads.IncludeClosed)
		if err != nil {
			return fmt.Errorf("list children for %s: %w", id, err)
		}
		for _, child := range children {
			if err := visit(child.ID); err != nil {
				return err
			}
		}

		upDeps, err := store.DepList(id, "up")
		if err != nil {
			return fmt.Errorf("list dependents for %s: %w", id, err)
		}
		for _, dep := range upDeps {
			if dep.Type != "parent-child" || dep.IssueID == "" {
				continue
			}
			if err := visit(dep.IssueID); err != nil {
				return err
			}
		}

		ids = append(ids, id)
		return nil
	}
	if err := visit(rootID); err != nil {
		return nil, err
	}
	return ids, nil
}
