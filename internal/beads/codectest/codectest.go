// Package codectest is the cross-domain conformance suite for per-class row
// codecs — the bead<->durable-row translation each infrastructure domain
// (sessions, mail, orders, nudges, convoy) implements when it relocates off bd
// onto SQLite. Under conformance-only validation this is the load-bearing
// fidelity gate: the sole guarantee that a codec round-trip never drops,
// duplicates, mutates, or reorders any part of a bead — its metadata AND the
// behaviorally-significant non-metadata fields the lifecycle projections consume
// (Status, CreatedAt, Labels, ...).
//
// It is a Layer-0 helper (standard library + internal/beads + internal/beads/extras
// only) and deliberately lives OUTSIDE internal/coordrouter so it survives the
// Router's deletion (engdocs/plans/infra-store-decouple/DESIGN.md, P6). Every
// domain's tests depend on this suite; none should depend on a package scheduled
// for removal.
package codectest

import (
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
)

// codecSkipReason is the default skip reason. Each per-class row codec flips
// Skip:false as it lands (mail at P1; the SQLite codecs at their cutover).
const codecSkipReason = "codectest: codec-fidelity conformance is skipped until a real row codec is wired " +
	"for this class (engdocs/plans/infra-store-decouple/PLAN.md, phases P1/P4/P5)"

// Options controls the suite, mirroring beadstest.Options: a whole-suite skip
// whose Reason MUST name the responsible phase so a skipped codec is documented,
// never silently hidden.
type Options struct {
	Skip   bool
	Reason string
}

// RowCodec is the bead<->durable-row translation a per-class adapter implements.
// The suite needs only the composed round-trip plus the reconstructed metadata
// union, so the concrete row type stays private to each adapter.
type RowCodec interface {
	// RoundTrip encodes a bead to its row form and decodes it back. The returned
	// bead must equal the input across ALL behaviorally-significant fields — its
	// metadata key-for-key (including keys this binary did not enumerate, the
	// unknown passthrough) and the non-metadata fields a projection may read.
	//
	// Equality treats a nil map/slice as equal to an empty one (and compares
	// timestamps with time.Equal): a codec may normalize nil<->empty freely. A
	// class whose projection branches on key PRESENCE vs empty-value must add a
	// sample that pins the distinction explicitly rather than relying on this
	// leniency.
	RoundTrip(beads.Bead) (beads.Bead, error)
	// ReconstructMetadata returns the full metadata map rebuilt from a bead's row
	// form as the disjoint union of promoted columns, the typed known fields, and
	// the unknown passthrough. It must error if those parts overlap (the
	// double-write/drift guard).
	ReconstructMetadata(beads.Bead) (map[string]string, error)
}

// Projection is a pure function over a bead (e.g. session ProjectLifecycle)
// whose output must be invariant across the codec round-trip. It is REQUIRED:
// projection-invariance is the check that catches a codec which silently changes
// a field the projection reads. A domain with no meaningful projection must
// supply an explicit identity projection and document why.
type Projection func(beads.Bead) any

// CodecConformance parameterizes RunCodecTests for one class's row codec.
type CodecConformance struct {
	// Name labels the subtests (e.g. "sessions", "mail"). Plain string — this
	// package has no dependency on any class taxonomy.
	Name string
	// Codec is the implementation under test.
	Codec RowCodec
	// Projection is asserted invariant across the round-trip (required).
	Projection Projection
	// Samples are representative beads. They MUST include beads carrying metadata
	// keys the codec does not promote (the unknown passthrough), at least one bead
	// with nil/empty metadata, and beads that vary the non-metadata fields
	// (Status, CreatedAt, Labels) so full-bead invariance is genuinely exercised.
	Samples []beads.Bead
}

// RunCodecTests runs the codec-fidelity suite, skipped by default until the codec
// lands.
func RunCodecTests(t *testing.T, cc CodecConformance) {
	RunCodecTestsWithOptions(t, cc, Options{Skip: true, Reason: codecSkipReason})
}

// RunCodecTestsWithOptions runs the suite with explicit options (Skip:false once
// a codec is ready).
func RunCodecTestsWithOptions(t *testing.T, cc CodecConformance, opts Options) {
	t.Helper()
	if opts.Skip {
		t.Skip(opts.Reason)
	}
	if cc.Codec == nil {
		t.Fatal("codectest: CodecConformance.Codec is nil")
	}
	if len(cc.Samples) == 0 {
		t.Fatal("codectest: CodecConformance.Samples is empty; provide representative beads incl. unknown metadata keys and varied non-metadata fields")
	}
	if cc.Projection == nil {
		t.Fatal("codectest: CodecConformance.Projection is nil; projection-invariance is mandatory — supply the class's pure projection (e.g. ProjectLifecycle), or an explicit identity projection with a documented rationale")
	}

	// (a) Golden round-trip — the loss detector. Every field of the bead survives
	// encode->decode unchanged: metadata key-for-key (incl. the unknown
	// passthrough) AND the non-metadata fields a projection may read.
	t.Run("GoldenRoundTrip", func(t *testing.T) {
		for i, b := range cc.Samples {
			rt, err := cc.Codec.RoundTrip(b)
			if err != nil {
				t.Fatalf("sample %d: RoundTrip: %v", i, err)
			}
			if ok, field := beadFieldsEqual(b, rt); !ok {
				t.Fatalf("sample %d: field %q not preserved across round-trip\n in  %+v\n out %+v", i, field, b, rt)
			}
		}
	})

	// (b) Reconstruct-union — the rebuilt metadata is the disjoint union of the
	// codec's parts (no key dropped, duplicated, or invented); a known/unknown
	// overlap surfaces as an error (the drift guard).
	t.Run("ReconstructUnion", func(t *testing.T) {
		for i, b := range cc.Samples {
			m, err := cc.Codec.ReconstructMetadata(b)
			if err != nil {
				t.Fatalf("sample %d: ReconstructMetadata: %v (a non-nil error means the codec's known/unknown parts overlap — the drift guard)", i, err)
			}
			if !metadataEqual(m, b.Metadata) {
				t.Fatalf("sample %d: reconstructed metadata != original\n got  %v\n want %v", i, m, b.Metadata)
			}
		}
	})

	// (e) Projection-invariance — a pure projection yields identical output before
	// and after the round-trip.
	t.Run("ProjectionInvariance", func(t *testing.T) {
		for i, b := range cc.Samples {
			rt, err := cc.Codec.RoundTrip(b)
			if err != nil {
				t.Fatalf("sample %d: RoundTrip: %v", i, err)
			}
			before, after := cc.Projection(b), cc.Projection(rt)
			if !projectionEqual(before, after) {
				t.Fatalf("sample %d: projection not invariant across round-trip\n before %v\n after  %v", i, before, after)
			}
		}
	})
}
