package effectinventory

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestCompileCanonicalRegistryAcceptsCleanProductionHead(t *testing.T) {
	workingDir, err := os.Getwd()
	if err != nil {
		t.Fatalf("Getwd: %v", err)
	}
	repoRoot := filepath.Clean(filepath.Join(workingDir, "../../.."))
	snapshot, err := probeGitRepository(context.Background(), repoRoot)
	if err != nil {
		t.Fatalf("probeGitRepository: %v", err)
	}
	if snapshot.dirty {
		t.Skip("production canonical compile requires an exact clean Git head")
	}
	registry, err := CanonicalRegistry()
	if err != nil {
		t.Fatalf("CanonicalRegistry: %v", err)
	}

	_, _, err = CompileCanonicalRegistry(context.Background(), CanonicalCompileRequest{
		RepoRoot:            snapshot.repoRoot,
		ExpectedGitRevision: snapshot.revision,
		Registry:            registry,
		AsOf:                time.Date(2026, time.July, 15, 0, 0, 0, 0, time.UTC),
	})
	if err != nil {
		t.Fatalf("CompileCanonicalRegistry: %v", err)
	}
}
