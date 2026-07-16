package main

import (
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/fsys"
	"github.com/gastownhall/gascity/internal/hooks"
	"github.com/gastownhall/gascity/internal/runtime"
)

// staleCodexOverlayHooks is a provider overlay .codex/hooks.json in the
// unbound template form that packs ship: matcher "" and a SessionStart command
// with no explicit --city binding. When this overlay is merged over a city-bound
// managed hooks file, the generic overlay merge keys the entry on its matcher
// (""), which does not collide with the managed "startup" entry, so the stale
// entry is appended — reintroducing managed-hook drift (ga-lk0).
const staleCodexOverlayHooks = `{
  "hooks": {
    "SessionStart": [
      {
        "matcher": "",
        "hooks": [
          {
            "type": "command",
            "command": "export PATH=\"$HOME/go/bin:$HOME/.local/bin:$PATH\" && GC_MANAGED_SESSION_HOOK=1 GC_HOOK_EVENT_NAME=SessionStart gc prime --hook --hook-format codex"
          }
        ]
      }
    ],
    "PreCompact": [
      {
        "matcher": "",
        "hooks": [
          {
            "type": "command",
            "command": "export PATH=\"$HOME/go/bin:$HOME/.local/bin:$PATH\" && gc handoff --auto --hook-format codex \"context cycle\""
          }
        ]
      }
    ]
  }
}
`

// TestPrepareTemplateResolution_CodexOverlayDoesNotDowngradeManagedHooks is the
// ga-lk0 regression guard. A Codex agent relies on the provider overlay for its
// hooks (no explicit install_agent_hooks). gc doctor --fix binds the managed
// Codex hooks to the city root; the controller then reconciles, staging the
// stale provider overlay over the bound file. Before the fix the post-staging
// hook re-install was gated on install_agent_hooks alone, so the projection
// reintroduced the unbound managed command every tick and gc doctor reported
// codex-hooks-drift again "within minutes". After the fix the projection
// re-binds the managed Codex hooks whenever the Codex overlay was staged, so
// the managed hooks stay current across reconciliation cycles.
func TestPrepareTemplateResolution_CodexOverlayDoesNotDowngradeManagedHooks(t *testing.T) {
	cityPath := t.TempDir()

	// Stale provider overlay: the pack ships the unbound managed-hook template.
	packOverlay := filepath.Join(cityPath, "packs", "core", "overlay")
	overlayHook := filepath.Join(packOverlay, "per-provider", "codex", ".codex", "hooks.json")
	if err := os.MkdirAll(filepath.Dir(overlayHook), 0o755); err != nil {
		t.Fatalf("MkdirAll(overlay): %v", err)
	}
	if err := os.WriteFile(overlayHook, []byte(staleCodexOverlayHooks), 0o644); err != nil {
		t.Fatalf("WriteFile(overlay hook): %v", err)
	}

	base := "builtin:codex"
	cfg := &config.City{
		Workspace: config.Workspace{Name: "test-city"},
		Agents: []config.Agent{{
			Name:     "coder",
			Provider: "codex",
			WorkDir:  "worker",
			// Intentionally no InstallAgentHooks: the agent relies on the
			// codex provider overlay for its managed hooks, which is the
			// configuration the ga-lk0 drift affected.
		}},
		Providers: map[string]config.ProviderSpec{
			// Plain codex: cataloged as required by this fork, inheriting the
			// builtin codex command (no Command override, so it is not treated
			// as a resume-wrapper). BuiltinAncestor=codex, so the launch-family
			// overlay per-provider/codex/ is staged.
			"codex": {Base: &base},
		},
		PackOverlayDirs: []string{packOverlay},
	}

	workDir := filepath.Join(cityPath, "worker")
	codexHooks := filepath.Join(workDir, ".codex", "hooks.json")

	// Simulate `gc doctor --fix`: bind the managed Codex hooks to the city root.
	if err := hooks.Install(fsys.OSFS{}, cityPath, workDir, []string{"codex"}); err != nil {
		t.Fatalf("hooks.Install (simulating doctor --fix): %v", err)
	}
	assertCodexHooksCurrent(t, codexHooks, cityPath, "after doctor --fix")

	bp := newAgentBuildParams("test-city", cityPath, cfg, runtime.NewFake(), time.Now().UTC(), nil, io.Discard)
	// Resolve the builtin codex provider without requiring a codex binary on
	// PATH: the projection under test is the overlay staging + hook re-bind,
	// not provider discovery.
	bp.lookPath = func(string) (string, error) { return "/bin/echo", nil }

	// Two controller reconciliation cycles must not downgrade the managed hooks.
	for cycle := 1; cycle <= 2; cycle++ {
		prepareTemplateResolution(bp, &cfg.Agents[0], "coder", io.Discard)
		assertCodexHooksCurrent(t, codexHooks, cityPath, "after reconcile cycle "+string(rune('0'+cycle)))
	}
}

// assertCodexHooksCurrent fails the test if the managed Codex hooks at path are
// stale (would be upgraded by gc doctor), if the city binding is missing, or if
// a second managed SessionStart entry was appended by the overlay merge.
func assertCodexHooksCurrent(t *testing.T, path, cityPath, when string) {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading managed codex hooks %s: %v", when, err)
	}
	if hooks.CodexHooksNeedManagedUpgrade(data, cityPath) {
		t.Fatalf("managed codex hooks drifted %s (gc doctor would report codex-hooks-drift):\n%s", when, data)
	}
	if !strings.Contains(string(data), "--city") {
		t.Fatalf("managed codex hooks lost --city binding %s:\n%s", when, data)
	}
	if n := codexSessionStartEntryCount(t, data); n != 1 {
		t.Fatalf("expected exactly one managed SessionStart entry %s, got %d:\n%s", when, n, data)
	}
}

// codexSessionStartEntryCount returns the number of entries in the SessionStart
// hook array of a Codex hooks document.
func codexSessionStartEntryCount(t *testing.T, data []byte) int {
	t.Helper()
	var doc struct {
		Hooks map[string][]json.RawMessage `json:"hooks"`
	}
	if err := json.Unmarshal(data, &doc); err != nil {
		t.Fatalf("unmarshaling codex hooks: %v", err)
	}
	return len(doc.Hooks["SessionStart"])
}

// TestInstallHooksAfterOverlayStaging locks in the decision logic that closes
// the ga-lk0 gap: the Codex family is re-installed after staging only when the
// Codex provider overlay was staged, and never duplicated.
func TestInstallHooksAfterOverlayStaging(t *testing.T) {
	base := "builtin:codex"
	providers := map[string]config.ProviderSpec{
		"codex-mini": {Base: &base},
	}
	tests := []struct {
		name             string
		installHooks     []string
		overlayProviders []string
		providers        map[string]config.ProviderSpec
		want             []string
	}{
		{
			name:             "non-codex overlay leaves hooks untouched",
			installHooks:     []string{"gemini"},
			overlayProviders: []string{"claude"},
			want:             []string{"gemini"},
		},
		{
			name:             "codex overlay without configured hook adds codex",
			installHooks:     nil,
			overlayProviders: []string{"codex"},
			want:             []string{"codex"},
		},
		{
			name:             "codex overlay preserves other configured hooks",
			installHooks:     []string{"gemini"},
			overlayProviders: []string{"codex"},
			want:             []string{"gemini", "codex"},
		},
		{
			name:             "codex already configured is not duplicated",
			installHooks:     []string{"codex"},
			overlayProviders: []string{"codex"},
			want:             []string{"codex"},
		},
		{
			name:             "codex-family wrapper overlay adds codex",
			installHooks:     nil,
			overlayProviders: []string{"codex-mini"},
			providers:        providers,
			want:             []string{"codex"},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := installHooksAfterOverlayStaging(tc.installHooks, tc.overlayProviders, tc.providers)
			if !slices.Equal(got, tc.want) {
				t.Fatalf("installHooksAfterOverlayStaging(%v, %v) = %v, want %v",
					tc.installHooks, tc.overlayProviders, got, tc.want)
			}
		})
	}
}
