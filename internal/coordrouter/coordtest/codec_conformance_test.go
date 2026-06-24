package coordtest

import (
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/beads/extras"
	"github.com/gastownhall/gascity/internal/coordclass"
)

// refKnown models a per-class adapter's enumerated, non-column fields. The
// reference codec promotes the "state" metadata key into this typed struct and
// passes every other key through extras.Unknown — the exact shape every real
// bd-delegating row codec follows.
type refKnown struct {
	State string `json:"state"`
}

var refKnownKeys = []string{"state"}

// refCodec is a faithful reference RowCodec used to prove RunCodecTests is not
// vacuous: it round-trips a bead's metadata through the extras envelope, so it
// must pass every codec-fidelity subtest. calls (when non-nil) records method
// entries so the skip-by-default test can prove the suite never touches the
// codec before skipping.
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
	return []beads.Bead{
		{
			Type:   "session",
			Status: "open",
			Metadata: map[string]string{
				"state":              "active",
				"continuation_epoch": "3",
				"future_unknown_key": "carried verbatim",
			},
		},
		{
			Type:   "session",
			Status: "open",
			Metadata: map[string]string{
				"alias":        "sess-a",
				"mcp_snapshot": `{"k":1}`,
			},
		},
		{Type: "session", Status: "open"}, // nil metadata: must round-trip to empty, not panic
	}
}

func refConformance(calls *int) CodecConformance {
	return CodecConformance{
		Class:      coordclass.ClassSessions,
		Codec:      refCodec{calls: calls},
		Projection: func(b beads.Bead) any { return b.Metadata["state"] + "|" + b.Metadata["continuation_epoch"] },
		Samples:    refSamples(),
	}
}

// TestCodecSuiteRunsNonVacuously proves the codec-fidelity suite executes and
// passes when run Skip:false against a faithful reference codec.
func TestCodecSuiteRunsNonVacuously(t *testing.T) {
	RunCodecTestsWithOptions(t, refConformance(nil), Options{Skip: false})
}

// TestCodecSuiteSkipsByDefault proves the P1 default skips before the suite ever
// touches the codec.
func TestCodecSuiteSkipsByDefault(t *testing.T) {
	calls := 0
	t.Run("default", func(t *testing.T) {
		RunCodecTests(t, refConformance(&calls))
	})
	if calls != 0 {
		t.Fatalf("default codec suite invoked the codec %d time(s); expected skip before any subtest", calls)
	}
}

// TestCodecSuiteCatchesLossyCodec proves the suite has teeth: the exact check the
// GoldenRoundTrip subtest applies (metadataEqual over the round-tripped bead)
// must reject a codec that drops unknown metadata keys. Asserted directly rather
// than by running the suite as a child t.Run, because a failing child would
// (correctly) mark this parent test failed too.
func TestCodecSuiteCatchesLossyCodec(t *testing.T) {
	bad := beads.Bead{Metadata: map[string]string{"state": "active", "dropme": "x"}}
	rt, err := lossyCodec{}.RoundTrip(bad)
	if err != nil {
		t.Fatalf("RoundTrip: %v", err)
	}
	if metadataEqual(rt.Metadata, bad.Metadata) {
		t.Fatal("the fidelity check (metadataEqual) failed to detect a dropped unknown key; the suite has no teeth")
	}
}

// lossyCodec deliberately keeps only the promoted "state" key and drops every
// unknown metadata key — the failure mode the golden round-trip must catch.
type lossyCodec struct{}

func (lossyCodec) RoundTrip(b beads.Bead) (beads.Bead, error) {
	m, err := lossyCodec{}.ReconstructMetadata(b)
	if err != nil {
		return beads.Bead{}, err
	}
	out := b
	out.Metadata = m
	return out, nil
}

func (lossyCodec) ReconstructMetadata(b beads.Bead) (map[string]string, error) {
	m := map[string]string{}
	if s := b.Metadata["state"]; s != "" {
		m["state"] = s
	}
	return m, nil
}
