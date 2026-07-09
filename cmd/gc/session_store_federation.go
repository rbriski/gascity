package main

import (
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/storeref"
)

// classFederatedSessionStore makes the session-lifecycle store handle owner-route its
// by-id operations across the session-class store, the graph store, and the work store.
//
// A session-lifecycle bead can land on a DIFFERENT physical store than this handle's
// primary. A pool-agent session bead carrying wisp markers classifies as ClassGraph
// (internal/coordclass/classify.go — the wisp case precedes the session case), and the
// city store's policy create-chokepoint routes ClassGraph beads to the graph store,
// while [beads.classes.sessions] may still resolve to the work store. The session
// reconciler then reads AND writes that bead through this handle (Get, then
// preWakeCommit's SetMetadataBatch, then the session_key/instance_token SetMetadata),
// so a plain single-store handle hits the wrong store and fails "bead not found",
// looping the pool-agent start forever. Owner-routing resolves the bead's real store
// per operation so reads and writes land where the bead physically lives. This is the
// session-lifecycle analog of the convoy ref-by-id + beadPolicyStore.Get federation.
//
// New-bead Create and listings stay on the primary session store (promoted from the
// embedded Store); only by-id ops on an EXISTING bead owner-route. When every candidate
// store is the same physical store (default bd backend) the wrapper is never
// constructed (see newClassFederatedSessionStore), so the path stays byte-identical.
type classFederatedSessionStore struct {
	beads.Store               // primary: the session-class store (Create/List/Ready/…)
	federation  []beads.Store // owner-resolution set (deduped): [session, graph, work]
}

// newClassFederatedSessionStore returns primary unwrapped when the candidate stores
// collapse to a single distinct store (the byte-identical default bd backend), otherwise
// the owner-routing wrapper over the deduped set.
func newClassFederatedSessionStore(primary beads.Store, others ...beads.Store) beads.Store {
	federation := distinctBeadStores(append([]beads.Store{primary}, others...))
	if len(federation) < 2 {
		return primary
	}
	return classFederatedSessionStore{Store: primary, federation: federation}
}

// distinctBeadStores drops nils and identical handles, preserving order.
func distinctBeadStores(stores []beads.Store) []beads.Store {
	out := make([]beads.Store, 0, len(stores))
	for _, s := range stores {
		if s == nil {
			continue
		}
		seen := false
		for _, kept := range out {
			if kept == s {
				seen = true
				break
			}
		}
		if !seen {
			out = append(out, s)
		}
	}
	return out
}

// owner returns the store that physically holds id, preferring the cheap prefix route
// and falling back to probing each store. It falls back to the primary store when no
// store claims id, preserving the prior single-store not-found behavior for writes.
func (s classFederatedSessionStore) owner(id string) beads.Store {
	if o := storeref.PrefixOwner(id, s.federation); o != nil {
		return o
	}
	for _, st := range s.federation {
		if _, err := st.Get(id); err == nil {
			return st
		}
	}
	return s.Store
}

// Get resolves the bead across every candidate store (federated by-id read).
func (s classFederatedSessionStore) Get(id string) (beads.Bead, error) {
	return storeref.Resolve(id, s.federation)
}

// SetMetadata routes to the store that owns id.
func (s classFederatedSessionStore) SetMetadata(id, key, value string) error {
	return s.owner(id).SetMetadata(id, key, value)
}

// SetMetadataBatch routes to the store that owns id.
func (s classFederatedSessionStore) SetMetadataBatch(id string, kvs map[string]string) error {
	return s.owner(id).SetMetadataBatch(id, kvs)
}

// Update routes to the store that owns id.
func (s classFederatedSessionStore) Update(id string, opts beads.UpdateOpts) error {
	return s.owner(id).Update(id, opts)
}

// Close routes to the store that owns id.
func (s classFederatedSessionStore) Close(id string) error {
	return s.owner(id).Close(id)
}
