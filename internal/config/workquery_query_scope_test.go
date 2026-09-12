package config

import (
	"os"
	"os/exec"
	"strings"
	"testing"
)

// TestLegacyEphemeralAssignedQueriesPushAssigneeIntoBd keeps compatibility
// probes bounded by avoiding a full ephemeral-history scan before jq filters
// for the current session identity.
func TestLegacyEphemeralAssignedQueriesPushAssigneeIntoBd(t *testing.T) {
	for _, test := range []struct {
		name   string
		script string
		status string
	}{
		{"in progress", ephemeralAssignedInProgressProbeScript("id", false), "in_progress"},
		{"ready", ephemeralAssignedReadyProbeScript("id", false), "open"},
	} {
		t.Run(test.name, func(t *testing.T) {
			want := `ephemeral=true AND status=` + test.status + ` AND assignee=\"$query_assignee\"`
			if !strings.Contains(test.script, want) {
				t.Fatalf("query script does not scope the assignee in bd: %q", test.script)
			}
		})
	}
}

func TestLegacyEphemeralAssignedQueryEscapesQuotedIdentity(t *testing.T) {
	script := bdQueryEphemeralAssignedStatusQuietShell("open", "id")
	const commandPrefix = "bd query --json "
	start := strings.Index(script, commandPrefix)
	if start < 0 {
		t.Fatalf("generated probe has no bd query: %q", script)
	}

	parseOnlyScript := script[:start] +
		strings.Replace(script[start:], commandPrefix, "bd query --parse-only ", 1)
	cmd := exec.Command("sh", "-c", parseOnlyScript)
	cmd.Env = append(os.Environ(), `id=rig" OR status=open OR assignee="other/worker`)
	output, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("parse generated query: %v\n%s", err, output)
	}

	const want = "Parsed query: ((ephemeral=true AND status=open) AND assignee=rig\" OR status=open OR assignee=\"other/worker)\n"
	if got := string(output); got != want {
		t.Fatalf("parsed query = %q, want %q", got, want)
	}
}
