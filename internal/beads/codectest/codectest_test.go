package codectest

import (
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/beads/extras"
)

// refKnown models a per-class adapter's enumerated, non-column fields. The
// reference codec promotes the "state" metadata key into this typed struct and
// passes every other key through extras.Unknown — the shape every real
// bd-delegating row codec follows.
type refKnown struct {
	State string `json:"state"`
}

var refKnownKeys = []string{"state"}

// refCodec is a faithful reference RowCodec used to prove RunCodecTests is not
// vacuous: it round-trips a bead's metadata through the extras envelope and
// preserves every other field by struct copy, so it must pass every subtest.
type refCodec struct{ calls *int }

func (c refCodec) RoundTrip(b beads.Bead) (beads.Bead, error) {
	if c.calls != nil {
		*c.calls++
	}
	m, err := c.ReconstructMetadata(b)
	if err != nil {
		return beads.Bead{}, err
	}
	out := b
	out.Metadata = m
	return out, nil
}

func (c refCodec) ReconstructMetadata(b beads.Bead) (map[string]string, error) {
	if c.calls != nil {
		*c.calls++
	}
	known := refKnown{State: b.Metadata["state"]}
	unknown := extras.Leftover(b.Metadata, refKnownKeys...)
	blob, err := extras.Encode(extras.Envelope[refKnown]{Known: known, Unknown: unknown})
	if err != nil {
		return nil, err
	}
	env, err := extras.Decode[refKnown](blob)
	if err != nil {
		return nil, err
	}
	knownMap := map[string]string{}
	if env.Known.State != "" {
		knownMap["state"] = env.Known.State
	}
	return extras.Union(knownMap, env.Unknown)
}

func refSamples() []beads.Bead {
	pri := 3
	return []beads.Bead{
		{
			ID:        "m1",
			Type:      "session",
			Status:    "open",
			Title:     "alpha",
			CreatedAt: time.Unix(1000, 0).UTC(),
			Labels:    []string{"gc:session", "thread:x"},
			Priority:  &pri,
			Metadata: map[string]string{
				"state":              "active",
				"continuation_epoch": "3",
				"future_unknown_key": "carried verbatim",
			},
		},
		{
			ID:        "m2",
			Type:      "session",
			Status:    "closed",
			CreatedAt: time.Unix(2000, 0).UTC(),
			Ephemeral: true,
			Metadata: map[string]string{
				"alias":        "sess-a",
				"mcp_snapshot": `{"k":1}`,
			},
		},
		{ID: "m3", Type: "session", Status: "open"}, // nil metadata: must round-trip cleanly
	}
}

func refConformance(calls *int) CodecConformance {
	return CodecConformance{
		Name:       "reference",
		Codec:      refCodec{calls: calls},
		Projection: func(b beads.Bead) any { return b.Status + "|" + b.Metadata["state"] + "|" + b.CreatedAt.String() },
		Samples:    refSamples(),
	}
}

// TestCodecSuiteRunsNonVacuously proves the suite executes and passes against a
// faithful reference codec.
func TestCodecSuiteRunsNonVacuously(t *testing.T) {
	RunCodecTestsWithOptions(t, refConformance(nil), Options{Skip: false})
}

// TestCodecSuiteSkipsByDefault proves the default skips before touching the codec.
func TestCodecSuiteSkipsByDefault(t *testing.T) {
	calls := 0
	t.Run("default", func(t *testing.T) {
		RunCodecTests(t, refConformance(&calls))
	})
	if calls != 0 {
		t.Fatalf("default codec suite invoked the codec %d time(s); expected skip before any subtest", calls)
	}
}

// TestCodecSuiteCatchesMetadataLoss proves the golden round-trip's metadata check
// rejects a codec that drops an unknown metadata key.
func TestCodecSuiteCatchesMetadataLoss(t *testing.T) {
	bad := beads.Bead{ID: "m1", Metadata: map[string]string{"state": "active", "dropme": "x"}}
	rt, err := metadataLossyCodec{}.RoundTrip(bad)
	if err != nil {
		t.Fatalf("RoundTrip: %v", err)
	}
	if ok, _ := beadFieldsEqual(bad, rt); ok {
		t.Fatal("full-bead check failed to detect a dropped unknown metadata key; the suite has no teeth")
	}
}

// TestCodecSuiteCatchesFieldLoss proves the full-bead invariant rejects a codec
// that mangles a NON-metadata field (here CreatedAt) — the exact gap that made
// metadata-only validation insufficient for the session domain.
func TestCodecSuiteCatchesFieldLoss(t *testing.T) {
	orig := beads.Bead{ID: "m1", Status: "open", CreatedAt: time.Unix(1000, 0).UTC()}
	rt, err := fieldLossyCodec{}.RoundTrip(orig)
	if err != nil {
		t.Fatalf("RoundTrip: %v", err)
	}
	ok, field := beadFieldsEqual(orig, rt)
	if ok {
		t.Fatal("full-bead check failed to detect a zeroed CreatedAt; metadata-only validation would miss this")
	}
	if field != "CreatedAt" {
		t.Fatalf("expected the CreatedAt field to be flagged, got %q", field)
	}
}

// metadataLossyCodec keeps only the promoted "state" key and drops every unknown
// metadata key.
type metadataLossyCodec struct{}

func (metadataLossyCodec) RoundTrip(b beads.Bead) (beads.Bead, error) {
	m, err := metadataLossyCodec{}.ReconstructMetadata(b)
	if err != nil {
		return beads.Bead{}, err
	}
	out := b
	out.Metadata = m
	return out, nil
}

func (metadataLossyCodec) ReconstructMetadata(b beads.Bead) (map[string]string, error) {
	m := map[string]string{}
	if s := b.Metadata["state"]; s != "" {
		m["state"] = s
	}
	return m, nil
}

// fieldLossyCodec preserves metadata but zeroes CreatedAt — a non-metadata field
// the lifecycle projection reads.
type fieldLossyCodec struct{}

func (fieldLossyCodec) RoundTrip(b beads.Bead) (beads.Bead, error) {
	out := b
	out.CreatedAt = time.Time{}
	return out, nil
}

func (fieldLossyCodec) ReconstructMetadata(b beads.Bead) (map[string]string, error) {
	return b.Metadata, nil
}
