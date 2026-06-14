//go:build integration

package integration

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
)

// TestGraphStoreSQLiteDeployedCityConverges is the full M2 deployed-city proof
// for the work/graph store split: a REAL `gc start` city on disk with
// [beads] provider="file" + graph_store="sqlite" (no Dolt) runs a graph.v2
// formula sling THROUGH THE ENTIRE PROCESS — pour -> discover -> worker complete
// -> controller converge -> terminal — and the molecule FINISHES (the
// control-dispatcher auto-closes the workflow root to gc.outcome=pass) with
// every graph-class bead resident in the on-disk <scope>/.gc/beads.sqlite, never
// the file work store.
//
// It extends TestGraphStoreSQLiteDeployedCityPour (which proves only the pour)
// by installing the gc bd-shim as the city's `bd` so BOTH the controller's
// `bd ready` control-bead discovery AND a no-LLM scripted worker reach the
// embedded SQLite store, and by slinging a MINIMAL graph.v2 formula (root + one
// work step; the compiler adds workflow-finalize) rather than the heavy
// worktree-bound mol-scoped-work. The worker performs only Router-routable
// mutations and never `bd update --claim` / `gc hook --claim` / `bd mol|gate`
// (all of which would bypass or be refused by the shim under graph_store=sqlite).
func TestGraphStoreSQLiteDeployedCityConverges(t *testing.T) {
	env := newGraphStoreSQLiteShimEnv(t)

	cityName := uniqueCityName()
	cityDir := filepath.Join(t.TempDir(), cityName)
	cityToml := fmt.Sprintf(`[workspace]
name = %q

[beads]
provider = "file"
graph_store = "sqlite"

[session]
provider = "subprocess"

[daemon]
formula_v2 = true
patrol_interval = "100ms"

[[agent]]
name = "worker"
max_active_sessions = 1
start_command = "bash %s"

[[named_session]]
template = "worker"
mode = "always"
`, cityName, agentScript("graph-store-sqlite-worker.sh"))
	configPath := filepath.Join(t.TempDir(), "m2-converge.toml")
	if err := os.WriteFile(configPath, []byte(cityToml), 0o644); err != nil {
		t.Fatalf("writing city config: %v", err)
	}

	out, err := runGCWithEnv(env, "", "init", "--skip-provider-readiness", "--file", configPath, cityDir)
	if err != nil {
		t.Fatalf("gc init failed: %v\noutput: %s", err, out)
	}
	registerCityCommandEnv(cityDir, env)
	t.Cleanup(func() {
		unregisterCityCommandEnv(cityDir)
		if out, err := runGCWithEnv(env, "", "supervisor", "stop", "--wait"); err != nil {
			t.Logf("cleanup: gc supervisor stop --wait: %v\n%s", err, out)
		}
		cleanupTestCityDir(cityDir)
	})
	waitForControllerReady(t, cityDir, 30*time.Second)

	// Stage a minimal graph.v2 formula in the city's Layer-2 formulas dir so
	// `gc sling worker <convoy> --on=slingdemo` resolves it. Root + one work
	// step; the compiler emits the workflow-finalize control bead.
	formulasDir := filepath.Join(cityDir, "formulas")
	if err := os.MkdirAll(formulasDir, 0o755); err != nil {
		t.Fatalf("creating formulas dir: %v", err)
	}
	const slingdemo = `formula = "slingdemo"

[requires]
formula_compiler = ">=2.0.0"

[[steps]]
id = "work"
title = "Work"
`
	if err := os.WriteFile(filepath.Join(formulasDir, "slingdemo.toml"), []byte(slingdemo), 0o644); err != nil {
		t.Fatalf("writing slingdemo formula: %v", err)
	}

	// Build a 1-member convoy and sling the minimal formula through the running
	// controller. slingdemo references no convoy_id/scope, so one member is fine.
	id1 := mustBdCreateID(t, cityDir, "slingdemo demo work")
	convoyOut, err := gc(cityDir, "convoy", "create", "slingdemo work", id1, "--json")
	if err != nil {
		t.Fatalf("gc convoy create failed: %v\noutput: %s", err, convoyOut)
	}
	var convoy struct {
		ConvoyID string `json:"convoy_id"`
	}
	if err := json.Unmarshal([]byte(extractJSONPayload(convoyOut)), &convoy); err != nil || convoy.ConvoyID == "" {
		t.Fatalf("parse convoy id: %v\noutput: %s", err, convoyOut)
	}
	if slingOut, err := gc(cityDir, "sling", "worker", convoy.ConvoyID, "--on=slingdemo"); err != nil {
		t.Fatalf("gc sling failed: %v\noutput: %s", err, slingOut)
	}

	// CONVERGE. The worker completes the discovered work step (routable
	// set-metadata+close -> SQLite); the controller's control-dispatcher then
	// finalizes the molecule. Wait for the workflow root to close with
	// gc.outcome=pass in the ON-DISK SQLite graph store.
	root := waitForGraphRootConverged(t, cityDir, graphStoreSQLiteConvergeTimeout())

	// The whole molecule is terminal in SQLite: the workflow root and the
	// compiler-generated workflow-finalize control bead are both closed there.
	sqliteBeads := mustScanSQLiteGraphStore(t, cityDir)
	finalize := beadByKind(sqliteBeads, "workflow-finalize")
	if finalize == nil {
		t.Fatalf("no workflow-finalize control bead in SQLite (beads: %d)", len(sqliteBeads))
	}
	if finalize.Status != "closed" {
		t.Fatalf("workflow-finalize in SQLite = status %q, want closed", finalize.Status)
	}
	workStep := firstClosedWorkStep(sqliteBeads)
	if workStep == nil {
		t.Fatalf("no closed actionable work step in SQLite (beads: %d)", len(sqliteBeads))
	}
	if got := workStep.Metadata["gc.outcome"]; got != "pass" {
		t.Fatalf("work step %s outcome = %q, want pass", workStep.ID, got)
	}
	t.Logf("deployed graph_store=sqlite city CONVERGED in on-disk SQLite: root=%s (outcome=%s) finalize=%s workStep=%s (%d graph beads)",
		root.ID, root.Metadata["gc.outcome"], finalize.ID, workStep.ID, len(sqliteBeads))

	// The graph-class control bead never leaked into the file work store. Its
	// gc.kind ("workflow-finalize") is an unambiguous graph-only marker, immune
	// to the gc-N id collision between the two stores.
	if data, err := os.ReadFile(filepath.Join(cityDir, ".gc", "beads.json")); err == nil {
		if strings.Contains(string(data), "workflow-finalize") {
			t.Fatalf("graph control bead (workflow-finalize) leaked into the file work store .gc/beads.json")
		}
	}
}

// graphStoreSQLiteConvergeTimeout bounds how long to wait for the deployed
// molecule to finish. A minimal graph.v2 sling with a 100ms patrol interval
// converges in a few seconds; the generous bound absorbs subprocess session
// spawn + controller serve latency without flaking.
func graphStoreSQLiteConvergeTimeout() time.Duration { return 90 * time.Second }

// newGraphStoreSQLiteShimEnv returns an isolated, no-Dolt command env whose `bd`
// resolves to the gc bd-shim (gc invoked as `bd`) for BOTH the controller's
// `bd ready` discovery subprocess AND spawned worker sessions, with the
// integration filebdshim as GC_BD_REAL for passthrough/work-only ops.
//
// The decisive mechanism: every managed session's PATH is re-fronted by
// prependGCBinDirToPATH(env, env["GC_BIN"]) where GC_BIN is the controller's
// os.Executable(). So a separate bdshim dir merely prepended to PATH is shadowed
// by whatever `bd` lives in the GC_BIN dir. We therefore run the supervisor (and
// hence every city controller it spawns via os.Executable()) from a per-test bin
// dir that holds a real `gc` copy AND a `bd` -> gc symlink: GC_BIN becomes that
// dir, prependGCBinDirToPATH fronts it, and `bd` resolves to gc-as-shim. This
// touches no shared global binary on disk; it only swaps the package-global
// gcBinary for the test (restored in cleanup), mirroring the existing
// binary-swap pattern in TestIntegrationEnvForUsesIsolatedHome.
func newGraphStoreSQLiteShimEnv(t *testing.T) []string {
	t.Helper()

	gcHome, _, env := newIsolatedEnvRoot(t, false)
	root := filepath.Dir(gcHome)

	// systemctl/launchctl no-op shims (suppress OS service integration).
	shimDir := filepath.Join(root, "bin")
	if err := os.MkdirAll(shimDir, 0o755); err != nil {
		t.Fatalf("creating isolated shim dir: %v", err)
	}
	for _, name := range []string{"systemctl", "launchctl"} {
		if err := os.WriteFile(filepath.Join(shimDir, name), []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
			t.Fatalf("writing %s shim: %v", name, err)
		}
	}

	// Per-test bin dir: a real gc copy (so os.Executable() resolves here, not a
	// symlink target) + `bd` -> gc so the shim wins wherever GC_BIN's dir lands
	// on PATH.
	gcBinDir := filepath.Join(root, "gcbin")
	if err := os.MkdirAll(gcBinDir, 0o755); err != nil {
		t.Fatalf("creating gc bin dir: %v", err)
	}
	gcCopy := filepath.Join(gcBinDir, "gc")
	if err := copyExecutable(gcBinary, gcCopy); err != nil {
		t.Fatalf("copying gc binary: %v", err)
	}
	if err := os.Symlink(gcCopy, filepath.Join(gcBinDir, "bd")); err != nil {
		t.Fatalf("symlinking bd -> gc: %v", err)
	}

	// Swap the package-global gcBinary so the supervisor (and the controllers it
	// spawns via os.Executable()) run from gcCopy -> GC_BIN = gcCopy ->
	// prependGCBinDirToPATH fronts gcBinDir -> `bd` resolves to gc-as-shim.
	prevGCBinary := gcBinary
	gcBinary = gcCopy
	t.Cleanup(func() { gcBinary = prevGCBinary })

	// Front gcBinDir + shimDir on the foreground env PATH too (so direct
	// supervisor/controller bd reads resolve the shim before the supervisor's
	// own prepend), and set GC_BD_REAL (a GC_-prefixed var, forwarded to sessions
	// by passthroughEnv) to the absolute filebdshim for passthrough/work ops.
	envMap := parseEnvList(env)
	env = replaceEnv(env, "PATH", prependPath(gcBinDir, shimDir, envMap["PATH"]))
	env = replaceEnv(env, "GC_BD_REAL", bdBinary)

	startIsolatedSupervisor(t, env, gcHome)
	return env
}

// copyExecutable copies src to dst with 0o755 perms. dst must be a real file
// (not a symlink) so os.Executable() on a process started from it resolves to
// dst rather than src.
func copyExecutable(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close() //nolint:errcheck // read-only close
	out, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o755)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close() //nolint:errcheck // best-effort on error path
		return err
	}
	return out.Close()
}

// waitForGraphRootConverged polls the on-disk SQLite graph store until the
// graph.v2 workflow root is closed with gc.outcome=pass, returning it.
func waitForGraphRootConverged(t *testing.T, cityDir string, timeout time.Duration) beads.Bead {
	t.Helper()
	deadline := time.Now().Add(timeout)
	var last string
	for time.Now().Before(deadline) {
		list, err := scanSQLiteGraphStoreWithClosed(filepath.Join(cityDir, ".gc"))
		if err != nil {
			last = err.Error()
		} else if root := beadByKind(list, "workflow"); root != nil {
			if root.Status == "closed" && root.Metadata["gc.outcome"] == "pass" {
				return *root
			}
			last = fmt.Sprintf("root %s status=%q outcome=%q; %s", root.ID, root.Status, root.Metadata["gc.outcome"], graphBeadSummary(list))
		} else {
			last = fmt.Sprintf("no workflow root among %d beads; %s", len(list), graphBeadSummary(list))
		}
		time.Sleep(300 * time.Millisecond)
	}
	trace, _ := os.ReadFile(filepath.Join(cityDir, "graph-store-worker-trace.log"))
	t.Fatalf("graph.v2 molecule did not converge in SQLite within %s: %s\nworker trace:\n%s", timeout, last, string(trace))
	return beads.Bead{}
}

// graphBeadSummary renders a compact id/status/kind/assignee line per bead, for
// diagnosing a stalled convergence.
func graphBeadSummary(list []beads.Bead) string {
	parts := make([]string, 0, len(list))
	for _, b := range list {
		parts = append(parts, fmt.Sprintf("%s[%s,kind=%s,assignee=%s]", b.ID, b.Status, b.Metadata["gc.kind"], b.Assignee))
	}
	return "beads: " + strings.Join(parts, " ")
}

// scanSQLiteGraphStoreWithClosed lists every bead in the on-disk SQLite graph
// store INCLUDING closed beads — the form the convergence assertions need, since
// a converged molecule's root, work step, and finalizer are all CLOSED. The
// durable graph.v2 workflow lives in the main tier, so the default tier read
// suffices (no wisp-tier opt-in).
func scanSQLiteGraphStoreWithClosed(sqliteDir string) ([]beads.Bead, error) {
	if _, err := os.Stat(filepath.Join(sqliteDir, "beads.sqlite")); err != nil {
		return nil, fmt.Errorf("sqlite graph store not present yet: %w", err)
	}
	store, err := beads.OpenSQLiteStore(sqliteDir)
	if err != nil {
		return nil, fmt.Errorf("open sqlite graph store: %w", err)
	}
	defer func() {
		if s, ok := store.(*beads.SQLiteStore); ok {
			_ = s.CloseStore()
		}
	}()
	return store.List(beads.ListQuery{AllowScan: true, IncludeClosed: true})
}

// mustScanSQLiteGraphStore returns every bead resident in the city's on-disk
// SQLite graph store (including closed), failing the test on error.
func mustScanSQLiteGraphStore(t *testing.T, cityDir string) []beads.Bead {
	t.Helper()
	list, err := scanSQLiteGraphStoreWithClosed(filepath.Join(cityDir, ".gc"))
	if err != nil {
		t.Fatalf("scanning SQLite graph store: %v", err)
	}
	return list
}

// beadByKind returns a pointer to the first bead whose gc.kind matches, or nil.
func beadByKind(list []beads.Bead, kind string) *beads.Bead {
	for i := range list {
		if list[i].Metadata["gc.kind"] == kind {
			return &list[i]
		}
	}
	return nil
}

// firstClosedWorkStep returns the first closed, non-control actionable step.
func firstClosedWorkStep(list []beads.Bead) *beads.Bead {
	control := map[string]bool{
		"workflow": true, "workflow-finalize": true, "scope": true,
		"scope-check": true, "gate": true,
	}
	for i := range list {
		b := &list[i]
		if b.Status != "closed" {
			continue
		}
		if control[b.Metadata["gc.kind"]] {
			continue
		}
		return b
	}
	return nil
}
