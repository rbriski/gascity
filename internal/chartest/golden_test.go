package chartest_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/chartest"
)

func TestCapture_GoldenIsDeterministicAndSectioned(t *testing.T) {
	capt := chartest.Capture{
		Exit:          0,
		Stdout:        []byte("frontend/worker\n"),
		Stderr:        []byte("route=api\n"),
		JSON:          []byte(`{"rig":"BEAD-1"}`),
		Events:        []string{"bead.created BEAD-1"},
		StoreReadback: []string{"BEAD-1 open"},
		APIRequests:   1,
		StoreOpens:    0,
	}
	got := string(capt.Golden())
	for _, section := range []string{
		"=== exit ===\n0\n",
		"=== stdout ===\nfrontend/worker\n",
		"=== stderr ===\nroute=api\n",
		"=== json ===\n{\"rig\":\"BEAD-1\"}\n",
		"=== events ===\nbead.created BEAD-1\n",
		"=== store ===\nBEAD-1 open\n",
		"=== counts ===\napi_requests=1 store_opens=0\n",
	} {
		if !strings.Contains(got, section) {
			t.Errorf("golden missing section %q in:\n%s", section, got)
		}
	}
	// Deterministic: same capture renders identically.
	if string(capt.Golden()) != got {
		t.Fatal("Golden() not deterministic")
	}
}

func TestCapture_GoldenOmitsJSONSectionWhenAbsent(t *testing.T) {
	capt := chartest.Capture{Exit: 1, Stdout: []byte("x")}
	if strings.Contains(string(capt.Golden()), "=== json ===") {
		t.Fatal("json section should be omitted when JSON is nil")
	}
}

func TestCompareGolden_MatchAndMismatch(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "sample.golden")
	content := []byte("=== exit ===\n0\n")
	if err := os.WriteFile(path, content, 0o644); err != nil {
		t.Fatal(err)
	}

	// Match: no failure recorded on a fresh sub-test.
	t.Run("match", func(t *testing.T) {
		chartest.CompareGolden(t, path, content)
	})

	// Mismatch: CompareGolden must fail. Use a recording TB.
	rec := &recordingTB{TB: t}
	chartest.CompareGolden(rec, path, []byte("=== exit ===\n1\n"))
	if !rec.failed {
		t.Fatal("CompareGolden did not fail on mismatch")
	}
}

// recordingTB records whether Errorf/Fatalf fired without aborting the parent.
type recordingTB struct {
	testing.TB
	failed bool
}

func (r *recordingTB) Errorf(string, ...any) { r.failed = true }
func (r *recordingTB) Fatalf(string, ...any) { r.failed = true }
func (r *recordingTB) Helper()               {}
