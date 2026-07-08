package chartest

import (
	"bytes"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"testing"
)

// updateGolden rewrites golden files instead of comparing. Distinct flag name
// so it never collides with other packages' -update flags in a shared test
// binary (cmd/gc imports this package).
var updateGolden = flag.Bool("chartest-update", false, "rewrite chartest golden files")

// Capture is the full observable surface of one command invocation on one lane,
// already canonicalized and deterministically ordered by the harness. It
// serializes to a single golden file so a lane's whole behavior is frozen in
// one place. Human text (Stdout/Stderr) is compared byte-exact; JSON is emitted
// verbatim here and the differ applies the shape+additive policy (0.7) before
// comparison of JSON-bearing goldens.
type Capture struct {
	Exit          int
	Stdout        []byte
	Stderr        []byte
	JSON          []byte   // optional; present only for a --json run
	Events        []string // canonicalized, sorted by the harness
	StoreReadback []string // canonicalized, sorted by the harness
	Counts        []Count  // boundary counts the harness actually measured, in a fixed order
}

// Count is one named boundary measurement (e.g. api_requests=1). Only counts the
// harness genuinely instruments are recorded, so a golden never asserts an
// unmeasured invariant as zero.
type Count struct {
	Name string
	N    int
}

// Golden renders the capture to its deterministic sectioned byte form.
func (c Capture) Golden() []byte {
	var b bytes.Buffer
	fmt.Fprintf(&b, "=== exit ===\n%d\n", c.Exit)
	fmt.Fprintf(&b, "=== stdout ===\n%s", withTrailingNewline(c.Stdout))
	fmt.Fprintf(&b, "=== stderr ===\n%s", withTrailingNewline(c.Stderr))
	if c.JSON != nil {
		fmt.Fprintf(&b, "=== json ===\n%s", withTrailingNewline(c.JSON))
	}
	fmt.Fprintf(&b, "=== events ===\n")
	for _, e := range c.Events {
		fmt.Fprintf(&b, "%s\n", e)
	}
	fmt.Fprintf(&b, "=== store ===\n")
	for _, s := range c.StoreReadback {
		fmt.Fprintf(&b, "%s\n", s)
	}
	fmt.Fprintf(&b, "=== counts ===\n")
	for _, ct := range c.Counts {
		fmt.Fprintf(&b, "%s=%d\n", ct.Name, ct.N)
	}
	return b.Bytes()
}

func withTrailingNewline(b []byte) []byte {
	if len(b) == 0 || b[len(b)-1] == '\n' {
		return b
	}
	return append(b, '\n')
}

// CompareGolden compares got against the golden at path, or rewrites it when
// -chartest-update is set. On mismatch it fails t with a readable diff header.
func CompareGolden(t testing.TB, path string, got []byte) {
	t.Helper()
	if *updateGolden {
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatalf("chartest: mkdir golden dir: %v", err)
		}
		if err := os.WriteFile(path, got, 0o644); err != nil {
			t.Fatalf("chartest: write golden %s: %v", path, err)
		}
		return
	}
	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("chartest: read golden %s: %v (run with -chartest-update to create it)", path, err)
	}
	if !bytes.Equal(got, want) {
		t.Errorf("chartest: golden mismatch for %s\n--- want ---\n%s\n--- got ---\n%s", path, want, got)
	}
}
