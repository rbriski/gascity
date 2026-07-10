package rollout

import (
	"bufio"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// depsEnvValue returns the value bound to key in a dotenv file ("" + false when
// absent). It is the read-side of the graduation forcing function: when the beads
// CAS gate's VersionAnchor (BD_CONDITIONAL_WRITES_MIN_VERSION) lands in deps.env
// with a concrete value, the gate has graduated past "pending".
func depsEnvValue(path, key string) (value string, present bool, err error) {
	f, err := os.Open(path)
	if err != nil {
		return "", false, err
	}
	defer func() { _ = f.Close() }()
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		if k, v, ok := strings.Cut(line, "="); ok && strings.TrimSpace(k) == key {
			return strings.TrimSpace(v), true, nil
		}
	}
	return "", false, sc.Err()
}

func writeDotenv(t *testing.T, content string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "deps.env")
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		t.Fatalf("write dotenv: %v", err)
	}
	return p
}

// TestConditionalWritesGraduation proves the graduation forcing function on
// SYNTHETIC deps.env fixtures: DORMANT when the version anchor is absent (today's
// real state, beads#4682 untagged — see TestBeadsVersionAnchorPending), ARMED with
// a concrete version when it lands. When armed, the gate has graduated and S4-T4
// must flip the Default Off->Auto (FlipDueBy); this test is the seam that arms
// that work — it exercises the reader on both states without depending on the real
// (still pending) deps.env.
func TestConditionalWritesGraduation(t *testing.T) {
	t.Parallel()
	anchor := specByKey(keyBeadsConditionalWrites).VersionAnchor
	if anchor == "" {
		t.Fatal("beads CAS gate has no VersionAnchor")
	}

	dormant := writeDotenv(t, "BD_VERSION=v1.1.0\nDOLT_VERSION=2.1.7\n")
	if v, present, err := depsEnvValue(dormant, anchor); err != nil || present {
		t.Errorf("dormant fixture: value=%q present=%v err=%v, want absent (graduation dormant)", v, present, err)
	}

	armed := writeDotenv(t, "BD_VERSION=v1.2.0\n"+anchor+"=v1.2.0\n")
	v, present, err := depsEnvValue(armed, anchor)
	if err != nil {
		t.Fatalf("armed fixture: %v", err)
	}
	if !present {
		t.Fatal("armed fixture: anchor absent, want present (graduation armed)")
	}
	if v != "v1.2.0" {
		t.Errorf("armed anchor value = %q, want v1.2.0", v)
	}
	if !strings.HasPrefix(v, "v") {
		t.Errorf("armed anchor value %q is not a version tag; the graduation forcing function needs a concrete flip target", v)
	}
}

// TestTerminalSpecsCarryExpiryShape is the merge-CI half of the expiry teeth:
// every rollout/migration gate must carry a well-formed Expires (shape only — no
// time.Now(), which would make merge CI a flaky clock). Wall-clock staleness is a
// doctor WARN (runtime), not a merge gate. ValidateSpecs enforces this too; this
// test pins the intent so a future gate edit can't quietly drop the date.
func TestTerminalSpecsCarryExpiryShape(t *testing.T) {
	t.Parallel()
	for _, s := range Specs() {
		if s.Category != InfraRollout && s.Category != InfraMigration {
			continue
		}
		if !isYYYYMMDD(s.Expires) {
			t.Errorf("gate %s (%s): Expires %q is not a well-formed YYYY-MM-DD date", s.Key, s.Category, s.Expires)
		}
	}
}
