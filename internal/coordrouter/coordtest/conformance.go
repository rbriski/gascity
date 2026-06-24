// Package coordtest provides conformance suites for the per-class store seams of
// the work-vs-infrastructure split (engdocs/design/beads-work-infra-split.md). It
// is the coordrouter analog of internal/beads/beadstest and
// internal/mail/mailtest: an exported Run* function takes a factory closure and
// drives t.Run subtests, so every implementation behind a class seam — the
// bd-delegating first impl AND any future faster backend — runs the IDENTICAL
// suite. That shared suite is the structural defense against the project's
// removed-backend history: a swap cannot silently change semantics if both impls
// must pass the same tests.
//
// P0 status: the suites default to SKIPPED (Options.Skip true) because no
// production backend is wired behind a seam yet — the doc's P0 exit criteria is
// "RunGraphStoreTests/RunClassedStoreTests (skipped)". The suites are NOT
// vacuous: the package's own tests run them with Skip:false against a MemStore to
// prove every subtest executes and passes, so the harness is ready for the P1
// author to flip Skip off per backend without changing a signature.
package coordtest

import (
	"context"
	"reflect"
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/coordclass"
	"github.com/gastownhall/gascity/internal/coordrouter"
)

// Options controls the conformance suites, modeled on beadstest.Options. P0
// defaults to a whole-suite skip whose Reason MUST name where the work is
// tracked, so a skipped seam is documented, never silently hidden.
type Options struct {
	// Skip skips the entire suite via t.Skip. P0 default is true: the
	// bd-delegating adapters are not wired behind any production seam yet.
	Skip bool
	// Reason is reported via t.Skip when Skip is true. It MUST name the
	// initiative phase / tracking reference responsible for the gap.
	Reason string
}

// classedStoreSkipReason is the default P0 skip reason for RunClassedStoreTests.
const classedStoreSkipReason = "coordtest: classed-store conformance is a P0 seam skeleton; " +
	"the bd-delegating adapter is wired behind a production seam in P1 " +
	"(engdocs/design/beads-work-infra-split.md, phase P1)"

// graphStoreSkipReason is the default P0 skip reason for RunGraphStoreTests.
const graphStoreSkipReason = "coordtest: GraphStore conformance is a P0 seam skeleton; " +
	"a non-bd graph backend is proven against it at relocation " +
	"(engdocs/design/beads-work-infra-split.md, phase P5)"

// RunClassedStoreTests runs the per-class store conformance suite against a
// store implementation for the given class. The factory must return a fresh,
// empty store for each call. P0 default: skipped (see Options).
//
// The factory is func() beads.Store rather than a per-class interface type
// because every non-graph class seam is a faithful subset of beads.Store
// (coordrouter.WorkStore is the marker alias, the others are segregated subsets),
// so one beads.Store factory exercises all of them; the class argument selects
// which class's representative bead and routing identity to assert. Graph has its
// own suite, RunGraphStoreTests, because its surface is the graph-apply
// capability, not a beads.Store method set.
func RunClassedStoreTests(t *testing.T, class coordclass.Class, newStore func() beads.Store) {
	RunClassedStoreTestsWithOptions(t, class, newStore, Options{Skip: true, Reason: classedStoreSkipReason})
}

// RunClassedStoreTestsWithOptions runs the per-class store conformance suite with
// explicit options (e.g. Skip:false once a backend is ready).
func RunClassedStoreTestsWithOptions(t *testing.T, class coordclass.Class, newStore func() beads.Store, opts Options) {
	t.Helper()
	if opts.Skip {
		t.Skip(opts.Reason)
	}

	t.Run("CreateRoundTripsThisClass", func(t *testing.T) {
		s := newStore()
		created, err := s.Create(representativeBead(class))
		if err != nil {
			t.Fatalf("Create: %v", err)
		}
		if created.ID == "" {
			t.Fatal("Create returned empty ID")
		}
		if got := coordclass.Classify(created); got != class {
			t.Fatalf("Classify(created) = %v, want %v — store must round-trip its own class's beads", got, class)
		}
	})

	t.Run("GetReturnsCreatedBead", func(t *testing.T) {
		s := newStore()
		created, err := s.Create(representativeBead(class))
		if err != nil {
			t.Fatalf("Create: %v", err)
		}
		got, err := s.Get(created.ID)
		if err != nil {
			t.Fatalf("Get: %v", err)
		}
		if got.ID != created.ID {
			t.Fatalf("Get ID = %q, want %q", got.ID, created.ID)
		}
	})

	t.Run("UpdateApplies", func(t *testing.T) {
		s := newStore()
		created, err := s.Create(representativeBead(class))
		if err != nil {
			t.Fatalf("Create: %v", err)
		}
		newTitle := "updated title"
		if err := s.Update(created.ID, beads.UpdateOpts{Title: &newTitle}); err != nil {
			t.Fatalf("Update: %v", err)
		}
		got, err := s.Get(created.ID)
		if err != nil {
			t.Fatalf("Get: %v", err)
		}
		if got.Title != newTitle {
			t.Fatalf("Title = %q, want %q", got.Title, newTitle)
		}
	})

	t.Run("CloseReopenRoundTrip", func(t *testing.T) {
		s := newStore()
		created, err := s.Create(representativeBead(class))
		if err != nil {
			t.Fatalf("Create: %v", err)
		}
		if err := s.Close(created.ID); err != nil {
			t.Fatalf("Close: %v", err)
		}
		closed, err := s.Get(created.ID)
		if err != nil {
			t.Fatalf("Get after Close: %v", err)
		}
		if closed.Status != "closed" {
			t.Fatalf("Status after Close = %q, want closed", closed.Status)
		}
		if err := s.Reopen(created.ID); err != nil {
			t.Fatalf("Reopen: %v", err)
		}
		reopened, err := s.Get(created.ID)
		if err != nil {
			t.Fatalf("Get after Reopen: %v", err)
		}
		if reopened.Status != "open" {
			t.Fatalf("Status after Reopen = %q, want open", reopened.Status)
		}
	})

	t.Run("ListIncludesCreatedBead", func(t *testing.T) {
		s := newStore()
		created, err := s.Create(representativeBead(class))
		if err != nil {
			t.Fatalf("Create: %v", err)
		}
		beadsOut, err := s.List(beads.ListQuery{AllowScan: true})
		if err != nil {
			t.Fatalf("List: %v", err)
		}
		found := false
		for _, b := range beadsOut {
			if b.ID == created.ID {
				found = true
				break
			}
		}
		if !found {
			t.Fatalf("List did not include the created %v bead %q", class, created.ID)
		}
	})
}

// RunGraphStoreTests runs the GraphStore conformance suite against a graph store
// implementation. The factory must return a fresh GraphStore for each call. P0
// default: skipped (see Options).
func RunGraphStoreTests(t *testing.T, newStore func() coordrouter.GraphStore) {
	RunGraphStoreTestsWithOptions(t, newStore, Options{Skip: true, Reason: graphStoreSkipReason})
}

// RunGraphStoreTestsWithOptions runs the GraphStore conformance suite with
// explicit options.
func RunGraphStoreTestsWithOptions(t *testing.T, newStore func() coordrouter.GraphStore, opts Options) {
	t.Helper()
	if opts.Skip {
		t.Skip(opts.Reason)
	}

	t.Run("ApplyResolvesEveryNodeKey", func(t *testing.T) {
		s := newStore()
		plan := &beads.GraphApplyPlan{Nodes: []beads.GraphApplyNode{
			{Key: "root", Title: "root"},
			{Key: "step", Title: "step", ParentKey: "root"},
		}}
		res, err := s.ApplyGraphPlan(context.Background(), plan)
		if err != nil {
			t.Fatalf("ApplyGraphPlan: %v", err)
		}
		if err := beads.ValidateGraphApplyResult(plan, res); err != nil {
			t.Fatalf("apply result must resolve a concrete ID for every node key: %v", err)
		}
	})

	t.Run("ApplyEmptyPlanIsAccepted", func(t *testing.T) {
		s := newStore()
		if _, err := s.ApplyGraphPlan(context.Background(), &beads.GraphApplyPlan{}); err != nil {
			t.Fatalf("ApplyGraphPlan(empty) = %v, want nil", err)
		}
	})
}

// codecSkipReason is the default skip reason for RunCodecTests. Each per-class
// bd-delegating row codec flips Skip:false as it lands in P1.
const codecSkipReason = "coordtest: codec-fidelity conformance is a P1 seam skeleton; " +
	"each per-class bd-delegating row codec is proven against it as it lands " +
	"(engdocs/plans/infra-store-decouple/PLAN.md, phase P1)"

// RowCodec is the bead<->durable-row translation a per-class adapter implements.
// The conformance suite only needs the composed metadata round-trip plus the
// reconstructed metadata union, so the concrete row type stays private to each
// adapter. Under conformance-only validation this is the load-bearing fidelity
// contract: it is the sole guarantee that the bead<->domain<->row translation
// never drops, duplicates, or mutates a metadata key.
type RowCodec interface {
	// RoundTrip encodes a bead to its row form and decodes it back. The returned
	// bead's Metadata must equal the input's, key-for-key, including any key this
	// binary did not enumerate (the unknown passthrough).
	RoundTrip(beads.Bead) (beads.Bead, error)
	// ReconstructMetadata returns the full metadata map the codec rebuilds from a
	// bead's row form as the disjoint union of promoted columns, the typed known
	// fields, and the unknown passthrough. It must error if those parts overlap
	// (the double-write/drift guard).
	ReconstructMetadata(beads.Bead) (map[string]string, error)
}

// Projection is a pure function over a bead (e.g. session ProjectLifecycle)
// whose output must be invariant across the codec round-trip. A nil Projection
// skips the projection-invariance subtest.
type Projection func(beads.Bead) any

// CodecConformance parameterizes RunCodecTests for one class's row codec.
type CodecConformance struct {
	// Class identifies the owning class (for subtest naming and context).
	Class coordclass.Class
	// Codec is the implementation under test.
	Codec RowCodec
	// Projection, when non-nil, is asserted invariant across the round-trip.
	Projection Projection
	// Samples are representative beads of the class. They MUST include beads
	// carrying metadata keys the codec does not promote (the unknown passthrough)
	// and at least one bead with nil/empty metadata.
	Samples []beads.Bead
}

// RunCodecTests runs the codec-fidelity conformance suite for a class's row
// codec. P1 default: skipped (see Options) until the codec lands.
func RunCodecTests(t *testing.T, cc CodecConformance) {
	RunCodecTestsWithOptions(t, cc, Options{Skip: true, Reason: codecSkipReason})
}

// RunCodecTestsWithOptions runs the codec-fidelity suite with explicit options
// (e.g. Skip:false once a codec is ready).
func RunCodecTestsWithOptions(t *testing.T, cc CodecConformance, opts Options) {
	t.Helper()
	if opts.Skip {
		t.Skip(opts.Reason)
	}
	if cc.Codec == nil {
		t.Fatal("coordtest: CodecConformance.Codec is nil")
	}
	if len(cc.Samples) == 0 {
		t.Fatal("coordtest: CodecConformance.Samples is empty; provide representative beads incl. unknown metadata keys")
	}

	// (a) Golden round-trip: the loss detector. Every metadata key — including
	// keys the codec does not enumerate — survives encode→decode unchanged.
	t.Run("GoldenRoundTrip", func(t *testing.T) {
		for i, b := range cc.Samples {
			rt, err := cc.Codec.RoundTrip(b)
			if err != nil {
				t.Fatalf("sample %d: RoundTrip: %v", i, err)
			}
			if !metadataEqual(rt.Metadata, b.Metadata) {
				t.Fatalf("sample %d: metadata not preserved\n got  %v\n want %v", i, rt.Metadata, b.Metadata)
			}
		}
	})

	// (b) Reconstruct-union: the rebuilt metadata is the disjoint union of the
	// codec's parts (no key dropped, duplicated, or invented), and the drift
	// guard fires on overlap (surfaced as an error from ReconstructMetadata).
	t.Run("ReconstructUnion", func(t *testing.T) {
		for i, b := range cc.Samples {
			m, err := cc.Codec.ReconstructMetadata(b)
			if err != nil {
				t.Fatalf("sample %d: ReconstructMetadata: %v (a non-nil error here means the codec's known/unknown parts overlap — the drift guard)", i, err)
			}
			if !metadataEqual(m, b.Metadata) {
				t.Fatalf("sample %d: reconstructed metadata != original\n got  %v\n want %v", i, m, b.Metadata)
			}
		}
	})

	// (e) Projection-invariance: a pure projection over the persisted subset
	// yields identical output before and after the round-trip.
	t.Run("ProjectionInvariance", func(t *testing.T) {
		if cc.Projection == nil {
			t.Skip("no Projection supplied for this class")
		}
		for i, b := range cc.Samples {
			rt, err := cc.Codec.RoundTrip(b)
			if err != nil {
				t.Fatalf("sample %d: RoundTrip: %v", i, err)
			}
			before, after := cc.Projection(b), cc.Projection(rt)
			if !reflect.DeepEqual(before, after) {
				t.Fatalf("sample %d: projection not invariant across round-trip\n before %v\n after  %v", i, before, after)
			}
		}
	})
}

// metadataEqual compares two metadata maps treating nil and empty as equal.
func metadataEqual(a, b map[string]string) bool {
	if len(a) != len(b) {
		return false
	}
	for k, v := range a {
		if bv, ok := b[k]; !ok || bv != v {
			return false
		}
	}
	return true
}

// representativeBead returns a minimal bead that coordclass.Classify maps to the
// given class. It is the seam between "a class" and "a concrete bead of that
// class" for the generic classed-store suite.
func representativeBead(class coordclass.Class) beads.Bead {
	switch class {
	case coordclass.ClassGraph:
		return beads.Bead{Title: "graph node", Labels: []string{"gc:wisp"}}
	case coordclass.ClassMessaging:
		return beads.Bead{Title: "message", Type: "message"}
	case coordclass.ClassSessions:
		return beads.Bead{Title: "session", Type: "session"}
	case coordclass.ClassOrders:
		return beads.Bead{Title: "order tracking", Labels: []string{"order-tracking"}}
	case coordclass.ClassNudges:
		return beads.Bead{Title: "nudge", Labels: []string{"gc:nudge"}}
	default: // coordclass.ClassWork
		return beads.Bead{Title: "work item"}
	}
}
