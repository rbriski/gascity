package bdflags

import (
	"os/exec"
	"regexp"
	"sort"
	"strings"
	"testing"
)

// flagNameRE matches the flag-declaration prefix of a cobra --help flag
// line, e.g. "  -a, --assignee string   Assignee" or
// "      --claim                Atomically claim...". It is anchored to
// the start of the line so mentions of "--flag" inside another flag's
// description text (which always follow on the same line, never at the
// start of one) are not mistaken for a declaration.
var flagNameRE = regexp.MustCompile(`(?m)^\s*(?:-([A-Za-z0-9]), )?--([A-Za-z0-9][A-Za-z0-9-]*)`)

// parseHelpFlagNames extracts every long (--flag) and short (-f) flag name
// declared in a bd --help transcript. The "Flags:" and "Global Flags:"
// sections are both plain flag-declaration lines and are matched the same
// way, so the result already includes global flags alongside the
// subcommand's own.
func parseHelpFlagNames(help string) map[string]bool {
	names := make(map[string]bool)
	for _, line := range strings.Split(help, "\n") {
		m := flagNameRE.FindStringSubmatch(line)
		if m == nil {
			continue
		}
		if m[1] != "" {
			names["-"+m[1]] = true
		}
		names["--"+m[2]] = true
	}
	return names
}

// TestBdFlagManifestCurrent guards against the bd CLI's flags drifting out
// from under this package's hardcoded manifest. It shells the real
// installed bd binary's --help output per known subcommand and fails
// loudly — fail-closed, the same posture as bdMutationWriteIDs in
// cmd/gc/cmd_bd.go — if the manifest and the live CLI disagree on flag
// names in either direction. If bd is not in PATH, the test is skipped
// with a clear message rather than failing, since manifest currency can't
// be checked without a bd binary to check it against.
func TestBdFlagManifestCurrent(t *testing.T) {
	bdPath, err := exec.LookPath("bd")
	if err != nil {
		t.Skip("bd not found in PATH; skipping flag-manifest freshness check")
	}

	for _, sub := range Subcommands() {
		t.Run(sub, func(t *testing.T) {
			args := append(strings.Fields(sub), "--help")
			out, _ := exec.Command(bdPath, args...).CombinedOutput()
			live := parseHelpFlagNames(string(out))
			if len(live) == 0 {
				t.Fatalf("parsed zero flags from `bd %s --help`; output format may have changed:\n%s", sub, out)
			}

			manifest := mergeFlagSets(ValueFlags(sub), BoolFlags(sub))

			var missingFromManifest, staleInManifest []string
			for f := range live {
				if !manifest[f] {
					missingFromManifest = append(missingFromManifest, f)
				}
			}
			for f := range manifest {
				if !live[f] {
					staleInManifest = append(staleInManifest, f)
				}
			}
			sort.Strings(missingFromManifest)
			sort.Strings(staleInManifest)

			if len(missingFromManifest) > 0 || len(staleInManifest) > 0 {
				t.Errorf("bd %s flag manifest has drifted from `bd %s --help`:\n  new flags not in manifest: %v\n  manifest flags no longer in --help: %v\nUpdate internal/bdflags/bdflags.go with a fresh dated-provenance comment.",
					sub, sub, missingFromManifest, staleInManifest)
			}
		})
	}
}
