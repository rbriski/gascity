package execenv

import (
	"strings"
	"testing"
)

func TestFilterInheritedStripsSensitiveEnv(t *testing.T) {
	got := FilterInherited([]string{
		"PATH=/bin",
		"GITHUB_TOKEN=ghs_secret",
		"OPENAI_API_KEY=sk-secret",
		"GC_INSTANCE_TOKEN=fence",
		"HOME=/tmp/home",
	})
	joined := strings.Join(got, "\n")
	for _, secret := range []string{"GITHUB_TOKEN", "OPENAI_API_KEY", "GC_INSTANCE_TOKEN", "ghs_secret", "sk-secret", "fence"} {
		if strings.Contains(joined, secret) {
			t.Fatalf("FilterInherited leaked %q in %q", secret, joined)
		}
	}
	if !strings.Contains(joined, "PATH=/bin") || !strings.Contains(joined, "HOME=/tmp/home") {
		t.Fatalf("FilterInherited dropped non-sensitive env: %q", joined)
	}
}

func TestIsSensitiveKeyTreatsDSNAsSensitive(t *testing.T) {
	for _, key := range []string{"GC_GRAPH_PG_DSN", "DSN", "MY_dsn", "postgres_dsn"} {
		if !IsSensitiveKey(key) {
			t.Errorf("IsSensitiveKey(%q) = false, want true (a DSN embeds a DB password)", key)
		}
	}
	// A DSN-carried password must not survive the inherited-env filter into a child
	// process (bd subprocess, session spawns), nor appear in redacted log text.
	filtered := strings.Join(FilterInherited([]string{
		"PATH=/bin",
		"GC_GRAPH_PG_DSN=postgres://user:SUPERSECRET@db.example/city",
	}), "\n")
	if strings.Contains(filtered, "SUPERSECRET") || strings.Contains(filtered, "GC_GRAPH_PG_DSN") {
		t.Fatalf("FilterInherited leaked a DSN password into a child env: %q", filtered)
	}
	if !strings.Contains(filtered, "PATH=/bin") {
		t.Fatalf("FilterInherited dropped a non-sensitive var: %q", filtered)
	}
	// RedactText scrubs a DSN assignment even without the env being supplied.
	red := RedactText("connecting GC_GRAPH_PG_DSN=postgres://user:SUPERSECRET@db.example/city now")
	if strings.Contains(red, "SUPERSECRET") {
		t.Fatalf("RedactText leaked a DSN password: %q", red)
	}
}

func TestMergeMapPreservesExplicitSensitiveOverrides(t *testing.T) {
	got := MergeMap([]string{
		"PATH=/bin",
		"GC_DOLT_PASSWORD=stale",
		"GITHUB_TOKEN=ambient",
	}, map[string]string{
		"GC_DOLT_PASSWORD": "required",
		"BEADS_DIR":        "/city/.beads",
	})
	joined := strings.Join(got, "\n")
	if strings.Contains(joined, "GITHUB_TOKEN") || strings.Contains(joined, "ambient") || strings.Contains(joined, "stale") {
		t.Fatalf("MergeMap leaked inherited secret: %q", joined)
	}
	if !strings.Contains(joined, "GC_DOLT_PASSWORD=required") {
		t.Fatalf("MergeMap did not preserve explicit secret override: %q", joined)
	}
}

func TestRedactTextRedactsEnvValuesAndAssignments(t *testing.T) {
	got := RedactText(
		"token=literal-secret GITHUB_TOKEN=ghs_secret output ghs_secret --password hunter2",
		[]string{"GITHUB_TOKEN=ghs_secret"},
	)
	for _, secret := range []string{"literal-secret", "ghs_secret", "hunter2"} {
		if strings.Contains(got, secret) {
			t.Fatalf("RedactText leaked %q in %q", secret, got)
		}
	}
	if strings.Count(got, Redacted) < 3 {
		t.Fatalf("RedactText redactions = %q, want at least three", got)
	}
}
