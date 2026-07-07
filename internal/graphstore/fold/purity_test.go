package fold_test

import (
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// TestFoldPackagePurity_DETT11 is the R-PURE / DET-T-11 purity tripwire: the fold
// package must stay I/O-free so a fold is a pure function of its events. It
// asserts that every non-test source file in this package imports ONLY the
// standard-library packages the pure driver needs (errors, fmt, sync) — no time,
// os, database/sql, filesystem, network, or store import can sneak in. A future
// impure edit fails CI here rather than silently coupling the fold to a clock,
// the store, or the outside world. Mirrors the allowlist shape of
// cmd/gc/worker_boundary_import_test.go.
func TestFoldPackagePurity_DETT11(t *testing.T) {
	_, currentFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller failed")
	}
	dir := filepath.Dir(currentFile)

	allowed := map[string]struct{}{
		"errors": {},
		"fmt":    {},
		"sync":   {},
	}

	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("ReadDir(%q): %v", dir, err)
	}
	fset := token.NewFileSet()
	sawSource := false
	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		sawSource = true
		path := filepath.Join(dir, name)
		f, err := parser.ParseFile(fset, path, nil, parser.ImportsOnly)
		if err != nil {
			t.Fatalf("parse %q: %v", path, err)
		}
		for _, imp := range f.Imports {
			p := strings.Trim(imp.Path.Value, `"`)
			if _, ok := allowed[p]; !ok {
				t.Fatalf("%s imports %q, which is not in the fold purity allowlist (errors, fmt, sync). "+
					"The fold package must stay I/O-free (R-PURE / DET-T-11).", name, p)
			}
		}
	}
	if !sawSource {
		t.Fatal("no non-test source files found in the fold package — purity tripwire is vacuous")
	}
}
