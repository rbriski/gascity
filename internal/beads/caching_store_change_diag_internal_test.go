package beads

import (
	"bytes"
	"log"
	"strings"
	"testing"
	"time"
)

// The beadChanged diff-log is diagnostic-only and default-on in a deployed
// binary, but the reconcile differential/oracle suites drive beadChanged across
// ~12k seeded cases — logging each would flood the test output. Disable it for
// the whole test binary; the field-detection core (beadChangeField) and the
// value/rate-limit helpers are unit-tested directly below without the log path.
func init() { beadChangeDiagEnabled = false }

func TestBeadChangeFieldMatchesBeadChanged(t *testing.T) {
	t.Parallel()

	prio7, prio8 := 7, 8
	blockedT, blockedF := true, false
	t1 := time.Date(2026, 7, 12, 1, 2, 3, 0, time.UTC)
	t2 := time.Date(2026, 7, 12, 4, 5, 6, 0, time.UTC)

	base := Bead{
		ID:          "gc-1",
		Title:       "t",
		Status:      "open",
		Type:        "task",
		Priority:    &prio7,
		CreatedAt:   t1,
		Assignee:    "a",
		From:        "f",
		ParentID:    "p",
		Ref:         "r",
		Description: "d",
		Metadata:    map[string]string{"k": "v"},
		Labels:      []string{"l1"},
		Needs:       []string{"n1"},
		Dependencies: []Dep{
			{IssueID: "gc-1", DependsOnID: "gc-9", Type: "blocks"},
		},
	}

	cases := []struct {
		name  string
		field string
		mut   func(b *Bead)
	}{
		{"title", "title", func(b *Bead) { b.Title = "t2" }},
		{"status", "status", func(b *Bead) { b.Status = "closed" }},
		{"type", "type", func(b *Bead) { b.Type = "bug" }},
		{"priority", "priority", func(b *Bead) { b.Priority = &prio8 }},
		{"created_at", "created_at", func(b *Bead) { b.CreatedAt = t2 }},
		{"assignee", "assignee", func(b *Bead) { b.Assignee = "b" }},
		{"from", "from", func(b *Bead) { b.From = "g" }},
		{"parent_id", "parent_id", func(b *Bead) { b.ParentID = "p2" }},
		{"ref", "ref", func(b *Bead) { b.Ref = "r2" }},
		{"description", "description", func(b *Bead) { b.Description = "d2" }},
		{"ephemeral", "ephemeral", func(b *Bead) { b.Ephemeral = true }},
		{"defer_until", "defer_until", func(b *Bead) { b.DeferUntil = &t2 }},
		{"is_blocked_set", "is_blocked", func(b *Bead) { b.IsBlocked = &blockedT }},
		{"metadata", "metadata", func(b *Bead) { b.Metadata = map[string]string{"k": "v2"} }},
		{"labels", "labels", func(b *Bead) { b.Labels = []string{"l1", "l2"} }},
		{"needs", "needs", func(b *Bead) { b.Needs = []string{"n2"} }},
		{"dependencies", "dependencies", func(b *Bead) {
			b.Dependencies = []Dep{{IssueID: "gc-1", DependsOnID: "gc-9", Type: "tracks"}}
		}},
	}

	_ = blockedF
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			fresh := cloneBead(base)
			tc.mut(&fresh)
			gotField := beadChangeField(base, fresh, false)
			if gotField != tc.field {
				t.Fatalf("beadChangeField = %q, want %q", gotField, tc.field)
			}
			if !beadChanged(base, fresh, false) {
				t.Fatalf("beadChanged = false, want true when %s changed", tc.field)
			}
		})
	}
}

// TestBeadChangeFieldUnchangedReturnsEmpty verifies the detection core reports
// no field (and beadChanged is false) for two order-shuffled-but-equal beads.
func TestBeadChangeFieldUnchangedReturnsEmpty(t *testing.T) {
	t.Parallel()

	old := Bead{
		ID:           "gc-1",
		Metadata:     map[string]string{"blob": `{"a":1,"b":2}`},
		Labels:       []string{"x", "y"},
		Needs:        []string{"n1", "n2"},
		Dependencies: []Dep{{IssueID: "gc-1", DependsOnID: "gc-2", Type: "blocks"}, {IssueID: "gc-1", DependsOnID: "gc-3", Type: "tracks"}},
	}
	fresh := Bead{
		ID:           "gc-1",
		Metadata:     map[string]string{"blob": `{"b":2,"a":1}`},
		Labels:       []string{"y", "x"},
		Needs:        []string{"n2", "n1"},
		Dependencies: []Dep{{IssueID: "gc-1", DependsOnID: "gc-3", Type: "tracks"}, {IssueID: "gc-1", DependsOnID: "gc-2", Type: "blocks"}},
	}
	if f := beadChangeField(old, fresh, false); f != "" {
		t.Fatalf("beadChangeField = %q, want \"\" for equal-but-reordered beads", f)
	}
	if beadChanged(old, fresh, false) {
		t.Fatalf("beadChanged = true, want false for equal-but-reordered beads")
	}
}

// TestBeadChangeFieldSkipLabels verifies skipLabels hides only the labels check.
func TestBeadChangeFieldSkipLabels(t *testing.T) {
	t.Parallel()

	old := Bead{ID: "gc-1", Labels: []string{"a"}}
	fresh := Bead{ID: "gc-1", Labels: []string{"a", "b"}}
	if f := beadChangeField(old, fresh, true); f != "" {
		t.Fatalf("beadChangeField(skipLabels=true) = %q, want \"\"", f)
	}
	if f := beadChangeField(old, fresh, false); f != "labels" {
		t.Fatalf("beadChangeField(skipLabels=false) = %q, want labels", f)
	}
}

func TestMetadataDiffDetail(t *testing.T) {
	t.Parallel()

	key, ov, fv := metadataDiffDetail(
		map[string]string{"same": "x", "changed": "1"},
		map[string]string{"same": "x", "changed": "2"},
	)
	if key != "changed" || ov != "1" || fv != "2" {
		t.Fatalf("metadataDiffDetail = (%q,%q,%q), want (changed,1,2)", key, ov, fv)
	}

	key, ov, fv = metadataDiffDetail(
		map[string]string{"only": "a"},
		map[string]string{},
	)
	if key != "only" || ov != "a" || fv != "<absent>" {
		t.Fatalf("metadataDiffDetail (removed) = (%q,%q,%q), want (only,a,<absent>)", key, ov, fv)
	}

	key, ov, fv = metadataDiffDetail(
		map[string]string{},
		map[string]string{"added": "b"},
	)
	if key != "added" || ov != "<absent>" || fv != "b" {
		t.Fatalf("metadataDiffDetail (added) = (%q,%q,%q), want (added,<absent>,b)", key, ov, fv)
	}
}

func TestTruncateDiag(t *testing.T) {
	t.Parallel()

	short := "hello"
	if got := truncateDiag(short); got != short {
		t.Fatalf("truncateDiag(short) = %q, want unchanged", got)
	}
	long := strings.Repeat("x", 200)
	got := truncateDiag(long)
	if len(got) <= beadChangeDiagValueMax {
		t.Fatalf("truncateDiag(long) length = %d, want > cap with length suffix", len(got))
	}
	if !strings.HasPrefix(got, strings.Repeat("x", beadChangeDiagValueMax)) {
		t.Fatalf("truncateDiag(long) prefix not preserved")
	}
	if !strings.Contains(got, "(200)") {
		t.Fatalf("truncateDiag(long) = %q, want original length annotation", got)
	}
}

// TestLogBeadChangeDiagEmitsLine exercises the full log path with the diag
// enabled (as it is in a deployed binary) and asserts the emitted line names
// the tripping field and old/fresh values. Not parallel: it toggles the global
// diag flag and the shared standard logger output, restoring both.
func TestLogBeadChangeDiagEmitsLine(t *testing.T) {
	prevEnabled := beadChangeDiagEnabled
	beadChangeDiagEnabled = true
	defer func() { beadChangeDiagEnabled = prevEnabled }()

	var buf bytes.Buffer
	prevOut := log.Writer()
	prevFlags := log.Flags()
	log.SetOutput(&buf)
	log.SetFlags(0)
	defer func() {
		log.SetOutput(prevOut)
		log.SetFlags(prevFlags)
	}()

	old := Bead{ID: "gcg-wisp-diagprobe", Description: "old-desc"}
	fresh := Bead{ID: "gcg-wisp-diagprobe", Description: "new-desc"}
	if !beadChanged(old, fresh, false) {
		t.Fatalf("beadChanged should report a change")
	}
	out := buf.String()
	for _, want := range []string{
		"beadChanged DIAG",
		"id=gcg-wisp-diagprobe",
		"field=description",
		`old="old-desc"`,
		`fresh="new-desc"`,
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("diag line missing %q; got %q", want, out)
		}
	}
}

func TestBeadChangeDiagShouldLogRateLimits(t *testing.T) {
	t.Parallel()

	// Use a fresh isolated view of the rate limiter by exercising a unique id.
	id := "gcg-wisp-ratelimit-probe"
	now := time.Now()
	if !beadChangeDiagShouldLog(id, now) {
		t.Fatalf("first call should log")
	}
	if beadChangeDiagShouldLog(id, now.Add(time.Second)) {
		t.Fatalf("second call within interval should be suppressed")
	}
	if !beadChangeDiagShouldLog(id, now.Add(beadChangeDiagPerIDInterval+time.Millisecond)) {
		t.Fatalf("call after interval should log again")
	}
}
