//go:build integration

package integration

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
)

// TestGraphStoreSQLiteDeployedCityPour stands up a REAL deployed city on disk
// with [beads] graph_store="sqlite" (no Dolt), slings a graph.v2 formula through
// the running controller, and asserts the molecule's graph beads (the workflow
// root) are poured into the on-disk <scope>/.gc/beads.sqlite file — proving the
// deployed-city store wiring (openStoreResultAtForCity -> routedPolicyStore ->
// registerGraphStoreBackend) routes graph-class beads to the embedded SQLite
// store end to end, in a literal `gc start` city (not a hand-built Router).
func TestGraphStoreSQLiteDeployedCityPour(t *testing.T) {
	env := newIsolatedCommandEnv(t, false)
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
`, cityName, agentScript("one-shot.sh"))
	configPath := filepath.Join(t.TempDir(), "m2-pour.toml")
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

	// Build a convoy and sling the built-in graph.v2 formula.
	id1 := mustBdCreateID(t, cityDir, "scoped work part one")
	id2 := mustBdCreateID(t, cityDir, "scoped work part two")
	convoyOut, err := gc(cityDir, "convoy", "create", "scoped work", id1, id2, "--json")
	if err != nil {
		t.Fatalf("gc convoy create failed: %v\noutput: %s", err, convoyOut)
	}
	var convoy struct {
		ConvoyID string `json:"convoy_id"`
	}
	if err := json.Unmarshal([]byte(extractJSONPayload(convoyOut)), &convoy); err != nil || convoy.ConvoyID == "" {
		t.Fatalf("parse convoy id: %v\noutput: %s", err, convoyOut)
	}
	if slingOut, err := gc(cityDir, "sling", "worker", convoy.ConvoyID, "--on=mol-scoped-work"); err != nil {
		t.Fatalf("gc sling failed: %v\noutput: %s", err, slingOut)
	}

	// The molecule's graph beads must be resident in the on-disk SQLite graph
	// store. Wait for the workflow root, then assert the compiler-generated
	// workflow-finalize control bead (an unambiguously graph-class bead) is also
	// there — proving the whole molecule topology, not just the root, poured to
	// SQLite.
	sqliteBeads := pollForGraphMolecule(t, cityDir, 30*time.Second)
	root := firstBeadByKind(sqliteBeads, "workflow")
	if root == "" {
		t.Fatalf("no graph.v2 workflow root in the on-disk SQLite graph store (beads: %d)", len(sqliteBeads))
	}
	finalize := firstBeadByKind(sqliteBeads, "workflow-finalize")
	if finalize == "" {
		t.Fatalf("no workflow-finalize control bead in the on-disk SQLite graph store (beads: %d)", len(sqliteBeads))
	}
	t.Logf("deployed graph_store=sqlite city poured the molecule to on-disk SQLite: root=%s finalize=%s (%d graph beads)", root, finalize, len(sqliteBeads))

	// The graph-class control bead must NOT be in the file work store. Its
	// gc.kind ("workflow-finalize") is an unambiguous, graph-only marker that the
	// Dolt/file work store would never legitimately hold — and it sidesteps the
	// gc-N id-namespace collision between the two stores.
	if data, err := os.ReadFile(filepath.Join(cityDir, ".gc", "beads.json")); err == nil {
		if strings.Contains(string(data), "workflow-finalize") {
			t.Fatalf("graph control bead (workflow-finalize) leaked into the file work store .gc/beads.json")
		}
	}
}

func firstBeadByKind(list []beads.Bead, kind string) string {
	for _, b := range list {
		if b.Metadata["gc.kind"] == kind {
			return b.ID
		}
	}
	return ""
}

func mustBdCreateID(t *testing.T, cityDir, title string) string {
	t.Helper()
	out, err := bd(cityDir, "create", "--json", title)
	if err != nil {
		t.Fatalf("bd create %q failed: %v\noutput: %s", title, err, out)
	}
	// The file-store bd path emits "Created bead: <id>" rather than JSON; the
	// Dolt path emits {"id": "..."}. Accept either.
	var created struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal([]byte(extractJSONPayload(out)), &created); err == nil && created.ID != "" {
		return created.ID
	}
	if _, after, ok := strings.Cut(out, "Created bead:"); ok {
		if id := strings.TrimSpace(after); id != "" {
			return strings.Fields(id)[0]
		}
	}
	t.Fatalf("could not parse a bead id from bd create output: %s", out)
	return ""
}

// pollForGraphMolecule opens the on-disk SQLite graph store directly and waits
// until the slung molecule has poured in (signalled by the workflow root
// appearing), returning every bead resident in the SQLite store.
func pollForGraphMolecule(t *testing.T, cityDir string, timeout time.Duration) []beads.Bead {
	t.Helper()
	sqliteDir := filepath.Join(cityDir, ".gc")
	deadline := time.Now().Add(timeout)
	var lastErr error
	for time.Now().Before(deadline) {
		list, err := scanSQLiteGraphStore(sqliteDir)
		if err == nil {
			for _, b := range list {
				if b.Metadata["gc.kind"] == "workflow" {
					return list
				}
			}
			lastErr = fmt.Errorf("no workflow root among %d sqlite beads", len(list))
		} else {
			lastErr = err
		}
		time.Sleep(300 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for the molecule to pour into %s/beads.sqlite: %v", sqliteDir, lastErr)
	return nil
}

func scanSQLiteGraphStore(sqliteDir string) ([]beads.Bead, error) {
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
	return store.List(beads.ListQuery{AllowScan: true})
}
