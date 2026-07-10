package main

import (
	"path/filepath"
	"testing"

	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
)

// TestPoolTriggerWorkDirLumenBeadUsesConfiguredDir is the HIGH-1 work-dir proof: a
// pool trigger whose work bead carries gc.lumen_run (a real-bead do) resolves to NO
// per-bead <beadID>-<nodeID> scratch dir — it runs in the agent's configured dir — so
// the launch never does `new-session -c <absent>`. An ordinary trigger bead is
// unchanged: it still gets its per-bead <base>/<slug> dir, byte-identical.
func TestPoolTriggerWorkDirLumenBeadUsesConfiguredDir(t *testing.T) {
	cityPath := t.TempDir()
	store := beads.NewMemStore()

	lumenBead, err := store.Create(beads.Bead{
		Type:  "task",
		Title: "hello",
		Metadata: map[string]string{
			beadmeta.LumenRunMetadataKey:        "gcg-run-x",
			beadmeta.LumenActivationMetadataKey: "hello:0",
			beadmeta.RoutedToMetadataKey:        "workers",
		},
	})
	if err != nil {
		t.Fatalf("create lumen bead: %v", err)
	}
	ordinaryBead, err := store.Create(beads.Bead{Type: "task", Title: "hello"})
	if err != nil {
		t.Fatalf("create ordinary bead: %v", err)
	}

	cfgAgent := &config.Agent{Name: "workers"}
	bp := &agentBuildParams{cityPath: cityPath, cityName: "test", beadStore: store}

	base, err := resolveConfiguredWorkDir(cityPath, "test", "workers", cfgAgent, nil)
	if err != nil {
		t.Fatalf("resolveConfiguredWorkDir: %v", err)
	}
	if base == "" {
		t.Fatalf("configured base work dir is empty; the test needs a resolvable base to distinguish the slug fallback")
	}

	// (1) The Lumen do bead: NO per-bead scratch dir — poolTriggerWorkDir suppresses the
	// slug fallback so the launch uses the agent's configured dir.
	lumenReq := SessionRequest{Template: "workers", WorkBeadID: lumenBead.ID, WorkBeadTitle: "hello"}
	if got := poolTriggerWorkDir(bp, cfgAgent, "workers", lumenReq); got != "" {
		scratch := filepath.Join(base, triggerBeadPathSlug(lumenBead.ID, "hello"))
		t.Fatalf("Lumen do bead work dir = %q, want \"\" (configured dir, not the per-bead scratch %q)", got, scratch)
	}

	// (2) The ordinary trigger bead is UNCHANGED: its per-bead <base>/<slug> dir.
	ordReq := SessionRequest{Template: "workers", WorkBeadID: ordinaryBead.ID, WorkBeadTitle: "hello"}
	wantSlug := filepath.Join(base, triggerBeadPathSlug(ordinaryBead.ID, "hello"))
	if got := poolTriggerWorkDir(bp, cfgAgent, "workers", ordReq); got != wantSlug {
		t.Fatalf("ordinary trigger bead work dir = %q, want %q (byte-identical per-bead dir)", got, wantSlug)
	}
}

// TestBindPoolSessionClearsStaleWorkDirForLumenBead proves the re-point case: a pooled
// session previously bound to an ordinary work bead (with a per-bead work_dir stamped)
// is re-pointed onto a Lumen do bead; the bind must CLEAR the stale work_dir keys so
// the launch re-resolves the agent's configured dir instead of an absent per-bead dir.
func TestBindPoolSessionClearsStaleWorkDirForLumenBead(t *testing.T) {
	cityPath := t.TempDir()
	store := beads.NewMemStore()

	lumenBead, err := store.Create(beads.Bead{
		Type:  "task",
		Title: "hello",
		Metadata: map[string]string{
			beadmeta.LumenRunMetadataKey:        "gcg-run-x",
			beadmeta.LumenActivationMetadataKey: "hello:0",
			beadmeta.RoutedToMetadataKey:        "workers",
		},
	})
	if err != nil {
		t.Fatalf("create lumen bead: %v", err)
	}

	// A pooled session bead carrying a STALE per-bead work_dir from a prior ordinary bind.
	session, err := store.Create(beads.Bead{
		Type:  "session",
		Title: "s",
		Metadata: map[string]string{
			beadmeta.WorkDirMetadataKey:       filepath.Join(cityPath, "stale-scratch-dir"),
			beadmeta.LegacyWorkDirMetadataKey: filepath.Join(cityPath, "stale-scratch-dir"),
		},
	})
	if err != nil {
		t.Fatalf("create session bead: %v", err)
	}

	cfgAgent := &config.Agent{Name: "workers"}
	bp := &agentBuildParams{cityPath: cityPath, cityName: "test", beadStore: store}
	req := SessionRequest{Template: "workers", WorkBeadID: lumenBead.ID, WorkBeadTitle: "hello"}

	bound, err := bindPoolSessionTriggerBead(bp, cfgAgent, "workers", session, req)
	if err != nil {
		t.Fatalf("bindPoolSessionTriggerBead: %v", err)
	}
	if got := bound.Metadata[beadmeta.WorkDirMetadataKey]; got != "" {
		t.Fatalf("work_dir after re-point to Lumen bead = %q, want cleared (empty)", got)
	}
	if got := bound.Metadata[beadmeta.LegacyWorkDirMetadataKey]; got != "" {
		t.Fatalf("legacy work_dir after re-point to Lumen bead = %q, want cleared (empty)", got)
	}
}
