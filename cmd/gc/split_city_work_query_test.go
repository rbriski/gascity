package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/config"
)

// markSplitCity writes the infra-scope activation marker so cityHasInfraStore
// reports true for a plain temp dir, without standing up a real store.
func markSplitCity(t *testing.T) string {
	t.Helper()
	cityPath := t.TempDir()
	marker := filepath.Join(infraScopeRoot(cityPath), ".beads")
	if err := os.MkdirAll(marker, 0o755); err != nil {
		t.Fatalf("mkdir infra scope: %v", err)
	}
	if err := os.WriteFile(filepath.Join(marker, "config.yaml"), []byte("backend: dolt\n"), 0o644); err != nil {
		t.Fatalf("write infra marker: %v", err)
	}
	if !cityHasInfraStore(cityPath) {
		t.Fatal("cityHasInfraStore false after writing the marker")
	}
	return cityPath
}

func TestSplitCityWorkQuerySwapsReadyBinary(t *testing.T) {
	agent := &config.Agent{Name: "worker"}
	beadsCfg := config.BeadsConfig{}

	split := markSplitCity(t)
	got := splitCityWorkQuery(split, agent, beadsCfg)

	if strings.Contains(got, "bd ready") {
		t.Errorf("split-city work_query still shells `bd ready`:\n%s", got)
	}
	if !strings.Contains(got, "gc ready") {
		t.Errorf("split-city work_query does not use `gc ready`:\n%s", got)
	}
	// The assigned in-progress crash-recovery tier must also switch so an infra
	// step whose worker died is resumable.
	if strings.Contains(got, "bd list --status in_progress") {
		t.Errorf("split-city work_query still shells `bd list --status in_progress`:\n%s", got)
	}
	if !strings.Contains(got, "gc ready --status in_progress") {
		t.Errorf("split-city work_query missing `gc ready --status in_progress`:\n%s", got)
	}
	// Legacy-ephemeral fallback stays single-store (retirement window).
	if !strings.Contains(got, "bd query") {
		t.Errorf("split-city work_query should leave the `bd query` legacy tier intact:\n%s", got)
	}
}

func TestSplitCityWorkQueryUnchangedOnSingleStore(t *testing.T) {
	agent := &config.Agent{Name: "worker"}
	beadsCfg := config.BeadsConfig{}
	single := t.TempDir() // no infra marker

	got := splitCityWorkQuery(single, agent, beadsCfg)
	want := agent.EffectiveWorkQueryForBeads(beadsCfg)
	if got != want {
		t.Fatalf("single-store work_query must be byte-identical to EffectiveWorkQueryForBeads.\n got: %s\nwant: %s", got, want)
	}
	if !strings.Contains(got, "bd ready") {
		t.Errorf("single-store work_query should still use `bd ready`:\n%s", got)
	}
}

func TestSplitCityWorkQueryLeavesCustomQueryAlone(t *testing.T) {
	agent := &config.Agent{Name: "worker", WorkQuery: "bd ready --assignee=me --json"}
	split := markSplitCity(t)
	got := splitCityWorkQuery(split, agent, config.BeadsConfig{})
	if got != agent.WorkQuery {
		t.Fatalf("custom work_query must be returned unchanged even on a split city.\n got: %s\nwant: %s", got, agent.WorkQuery)
	}
}

func TestSplitCityPoolDemandQuerySwapsReadyBinary(t *testing.T) {
	agent := &config.Agent{Name: "worker"}
	split := markSplitCity(t)
	got := splitCityPoolDemandQuery(split, agent, config.BeadsConfig{})
	if strings.Contains(got, "bd ready") {
		t.Errorf("split-city count-form still shells `bd ready`:\n%s", got)
	}
	if !strings.Contains(got, "gc ready") {
		t.Errorf("split-city count-form does not use `gc ready`:\n%s", got)
	}
	// Symmetric with the worker work_query rewrite (spawn demand ↔ claim read).
	single := splitCityPoolDemandQuery(t.TempDir(), agent, config.BeadsConfig{})
	if !strings.Contains(single, "bd ready") {
		t.Errorf("single-store count-form should still use `bd ready`:\n%s", single)
	}
}
