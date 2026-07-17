package sling

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/formulatest"
	"github.com/gastownhall/gascity/internal/graphv2"
)

// These tests pin the base_branch precedence contract for workspace setup
// (ga-3j9): the rendered workflow base_branch is authoritative, and the live
// origin-HEAD probe applies only when no base branch is configured anywhere —
// not on the work bead, not on the rig, and not as a formula-declared
// default. On rigs cloned from a local non-bare origin, origin/HEAD follows
// whatever branch the origin working copy has checked out, so letting the
// probe outrank a formula-declared default cut worktrees and polecat
// branches from the wrong base.

// writeBaseBranchTestFormula writes a minimal v1-style formula named name
// into dir. varsBlock is appended verbatim after the header, so tests can
// declare (or omit) a [vars.base_branch] default.
func writeBaseBranchTestFormula(t *testing.T, dir, name, varsBlock string) {
	t.Helper()
	content := "formula = \"" + name + "\"\nversion = 1\n" + varsBlock + `
[[steps]]
id = "workspace-setup"
title = "Set up workspace"
description = "git worktree add wt --detach origin/{{base_branch}}"
`
	if err := os.WriteFile(filepath.Join(dir, name+".toml"), []byte(content), 0o644); err != nil {
		t.Fatalf("writing formula fixture %s: %v", name, err)
	}
}

// probedOriginHeadBranch is what the stubbed live probe reports as the
// repo's origin-HEAD default branch in these tests — standing in for the
// checked-out branch of a local non-bare origin.
const probedOriginHeadBranch = "scratch"

// baseBranchTestDeps builds a city with one rig (prefix SC) whose formula
// search path is dir, plus a live-probe stub that reports
// probedOriginHeadBranch as the repo's origin-HEAD default branch.
func baseBranchTestDeps(dir, rigDefaultBranch string) (SlingDeps, config.Agent) {
	cfg := &config.City{
		Workspace: config.Workspace{Name: "test"},
		Rigs: []config.Rig{
			{Name: "scamper", Path: "/scamper", Prefix: "SC", DefaultBranch: rigDefaultBranch},
		},
		FormulaLayers: config.FormulaLayers{City: []string{dir}},
	}
	deps := SlingDeps{
		Cfg:      cfg,
		Store:    beads.NewMemStore(),
		Branches: fixedBranchResolver{branch: probedOriginHeadBranch},
	}
	return deps, config.Agent{Name: "polecat", Dir: "scamper"}
}

func TestBuildSlingFormulaVarsFormulaDefaultSuppressesOriginHeadProbe(t *testing.T) {
	dir := t.TempDir()
	writeBaseBranchTestFormula(t, dir, "mol-polecat-work", `
[vars]
[vars.base_branch]
description = "base branch"
default = "main"
`)
	deps, a := baseBranchTestDeps(dir, "")

	vars := BuildSlingFormulaVars("mol-polecat-work", "SC-1", nil, a, deps)
	if got, ok := vars["base_branch"]; ok {
		t.Fatalf("base_branch = %q injected over formula-declared default; want no injection so the declared default applies", got)
	}
}

func TestBuildSlingFormulaVarsProbeAppliesWhenFormulaDeclaresNoDefault(t *testing.T) {
	dir := t.TempDir()
	writeBaseBranchTestFormula(t, dir, "mol-polecat-plain", "")
	deps, a := baseBranchTestDeps(dir, "")

	vars := BuildSlingFormulaVars("mol-polecat-plain", "SC-1", nil, a, deps)
	if got := vars["base_branch"]; got != probedOriginHeadBranch {
		t.Fatalf("base_branch = %q, want %q (live probe remains the fallback when the formula declares no default)", got, probedOriginHeadBranch)
	}
}

func TestBuildSlingFormulaVarsRigStoredDefaultBeatsFormulaDefault(t *testing.T) {
	dir := t.TempDir()
	writeBaseBranchTestFormula(t, dir, "mol-polecat-work", `
[vars]
[vars.base_branch]
default = "main"
`)
	deps, a := baseBranchTestDeps(dir, "master")

	vars := BuildSlingFormulaVars("mol-polecat-work", "SC-1", nil, a, deps)
	if got := vars["base_branch"]; got != "master" {
		t.Fatalf("base_branch = %q, want %q (rig stored default_branch stays authoritative for hosted rigs)", got, "master")
	}
}

func TestBuildSlingFormulaVarsBeadTargetBeatsFormulaDefault(t *testing.T) {
	dir := t.TempDir()
	writeBaseBranchTestFormula(t, dir, "mol-polecat-work", `
[vars]
[vars.base_branch]
default = "main"
`)
	deps, a := baseBranchTestDeps(dir, "")
	bead, err := deps.Store.Create(beads.Bead{Metadata: map[string]string{"target": "release/v2"}})
	if err != nil {
		t.Fatalf("seeding bead: %v", err)
	}

	vars := BuildSlingFormulaVars("mol-polecat-work", bead.ID, nil, a, deps)
	if got := vars["base_branch"]; got != "release/v2" {
		t.Fatalf("base_branch = %q, want %q (work bead metadata.target stays the top per-bead override)", got, "release/v2")
	}
}

func TestBuildSlingFormulaVarsEmptyDeclaredDefaultKeepsProbe(t *testing.T) {
	dir := t.TempDir()
	writeBaseBranchTestFormula(t, dir, "mol-polecat-work", `
[vars]
[vars.base_branch]
default = ""
`)
	deps, a := baseBranchTestDeps(dir, "")

	vars := BuildSlingFormulaVars("mol-polecat-work", "SC-1", nil, a, deps)
	if got := vars["base_branch"]; got != probedOriginHeadBranch {
		t.Fatalf("base_branch = %q, want %q (an empty declared default cannot drive origin/<branch>; probe must still apply)", got, probedOriginHeadBranch)
	}
}

// TestGraphV2EffectiveVarsHonorFormulaBaseBranchDefault drives the full
// graph.v2 chain the LifeOS canary hit: sling builds routing vars, then
// PrepareInvocation overlays them onto formula defaults. The workflow
// declares base_branch=main; the rig has no stored default and the live
// probe reports the local non-bare origin's checked-out branch. The
// effective runtime vars — the values {{base_branch}} renders from — must
// say main.
func TestGraphV2EffectiveVarsHonorFormulaBaseBranchDefault(t *testing.T) {
	formulatest.EnableV2ForTest(t)
	dir := t.TempDir()
	content := `formula = "mol-polecat-work"
version = 2
contract = "graph.v2"

[vars]
[vars.base_branch]
description = "base branch"
default = "main"

[[steps]]
id = "workspace-setup"
title = "Set up workspace"
description = "git worktree add wt --detach origin/{{base_branch}}"
`
	if err := os.WriteFile(filepath.Join(dir, "mol-polecat-work.toml"), []byte(content), 0o644); err != nil {
		t.Fatalf("writing formula fixture: %v", err)
	}
	deps, a := baseBranchTestDeps(dir, "")

	vars := buildGraphV2SlingFormulaVars("mol-polecat-work", "SC-1", nil, a, deps)
	inv, err := graphv2.PrepareInvocation(context.Background(), deps.Store, "mol-polecat-work", SlingFormulaSearchPaths(deps, a), "", vars)
	if err != nil {
		t.Fatalf("PrepareInvocation: %v", err)
	}
	if got := inv.Vars["base_branch"]; got != "main" {
		t.Fatalf("effective base_branch = %q, want %q (workflow-declared base_branch is authoritative over origin-HEAD discovery)", got, "main")
	}
}
