//go:build integration

package dashport_test

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/gastownhall/gascity/internal/runtime"
	"github.com/gastownhall/gascity/internal/testutil"
)

func TestStructuredTranscriptBrowser(t *testing.T) {
	if os.Getenv("GC_DASHPORT_BROWSER_E2E") != "1" {
		t.Skip("set GC_DASHPORT_BROWSER_E2E=1 or run make dashboard-e2e-play")
	}
	chromium := os.Getenv("PLAYWRIGHT_CHROMIUM_EXECUTABLE")
	if chromium != "" {
		if _, err := os.Stat(chromium); err != nil {
			t.Fatalf("stat Chromium executable %q: %v", chromium, err)
		}
	}

	h := newHarness(t)
	info, err := h.sessionManager.Get(h.sessionID)
	if err != nil {
		t.Fatalf("get seeded session: %v", err)
	}
	h.sessionProvider.SetPendingInteraction(info.SessionName, &runtime.PendingInteraction{
		RequestID: "approval-browser",
		Kind:      "approval",
		Prompt:    "Approve browser transcript update?",
		Options:   []string{"Approve", "Deny"},
	})
	repoRoot, err := filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		t.Fatalf("resolve repository root: %v", err)
	}
	frontendDir := filepath.Join(repoRoot, "internal", "api", "dashboardspa", "web", "frontend")
	artifactRoot := filepath.Join(repoRoot, ".cache")
	if err := os.MkdirAll(artifactRoot, 0o755); err != nil {
		t.Fatalf("create Playwright artifact root: %v", err)
	}
	artifactDir, err := os.MkdirTemp(artifactRoot, "dashport-playwright-")
	if err != nil {
		t.Fatalf("create Playwright artifact directory: %v", err)
	}
	removeArtifacts := true
	defer func() {
		if removeArtifacts {
			if err := os.RemoveAll(artifactDir); err != nil {
				t.Errorf("remove successful Playwright artifacts %q: %v", artifactDir, err)
			}
		}
	}()

	ctx, cancel := context.WithTimeout(context.Background(), 12*testutil.ExecRaceTimeout)
	defer cancel()
	output, err := runStructuredTranscriptBrowser(ctx, browserProcessConfig{
		dir: frontendDir,
		env: []string{
			"DASHPORT_BASE_URL=" + h.server.URL,
			"DASHPORT_CITY_NAME=" + h.cityName,
			"DASHPORT_SESSION_ID=" + h.sessionID,
			"DASHPORT_TRANSCRIPT_PATH=" + h.transcriptPath,
			"PLAYWRIGHT_CHROMIUM_EXECUTABLE=" + chromium,
			"PLAYWRIGHT_OUTPUT_DIR=" + artifactDir,
		},
	})
	if err != nil {
		removeArtifacts = false
		if ctx.Err() != nil {
			t.Fatalf("Playwright timed out after %s: %v (artifacts: %s)\n%s", 12*testutil.ExecRaceTimeout, ctx.Err(), artifactDir, output)
		}
		t.Fatalf("Playwright structured transcript e2e: %v (artifacts: %s)\n%s", err, artifactDir, output)
	}
	t.Logf("Playwright structured transcript e2e passed:\n%s", output)
}
