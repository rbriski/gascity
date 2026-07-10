package main

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/gastownhall/gascity/internal/graphstore"
	"github.com/gastownhall/gascity/internal/lumen/ir"
)

// Shared fixtures for the Lumen controller-loop and enqueue tests: a pool route,
// a graph-scoped temp city, a graph-store opener, and a do-only IR document.

const tbHookRoute = "rig/claude" // a pool route string

// tbHookGraphCity creates a temp city with a graph scope marker so
// cityHasGraphScope / cachedCityGraphJournal / openGraphStore all resolve.
func tbHookGraphCity(t *testing.T) string {
	t.Helper()
	cityPath := t.TempDir()
	graphBeads := filepath.Join(cityPath, ".gc", "graph", ".beads")
	if err := os.MkdirAll(graphBeads, 0o755); err != nil {
		t.Fatalf("mkdir graph scope: %v", err)
	}
	if err := os.WriteFile(filepath.Join(graphBeads, "config.yaml"), []byte("backend: sqlite\n"), 0o644); err != nil {
		t.Fatalf("write graph scope marker: %v", err)
	}
	return cityPath
}

// tbHookOpenStore opens the city's graph journal store.
func tbHookOpenStore(t *testing.T, cityPath string) *graphstore.Store {
	t.Helper()
	backend, err := loadGraphJournalBackendConfig(cityPath)
	if err != nil {
		t.Fatalf("load graph backend: %v", err)
	}
	gs, err := backend.openGraphStore(context.Background(), cityPath)
	if err != nil {
		t.Fatalf("open graph store: %v", err)
	}
	return gs
}

// tbHookDoc is a single-do-node IR document ("hello" says "Say hello.").
func tbHookDoc(t *testing.T) *ir.IR {
	t.Helper()
	const doc = `{
      "contract": {"name": "lumen.ir", "version": "0.2.5", "producer": "test"},
      "name": "greet",
      "input": {"name": "main.input", "fields": [], "origin": {"uri": "t", "line": 0, "col": 0}},
      "origin": {"uri": "t", "line": 0, "col": 0},
      "nodes": [
        {"kind": "block", "id": "block_1", "after": [], "origin": {"uri": "t", "line": 1, "col": 0},
         "members": [
           {"kind": "do", "id": "hello", "name": "hello", "after": [],
            "origin": {"uri": "t", "line": 1, "col": 0},
            "source": {"kind": "prompt"},
            "interpreter": {"kind": "agent", "mode": {"kind": "do"}, "origin": {"uri": "t", "line": 1, "col": 0}},
            "body": {"raw": "Say hello.", "language": "markdown", "source": {"kind": "inline"}, "origin": {"uri": "t", "line": 1, "col": 0}}}
         ]}
      ]
    }`
	d, err := ir.Decode([]byte(doc))
	if err != nil {
		t.Fatalf("decode IR: %v", err)
	}
	return d
}
