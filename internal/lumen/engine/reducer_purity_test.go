package engine_test

import (
	"go/parser"
	"go/token"
	"strconv"
	"strings"
	"testing"
)

// TestReducerFilesArePure is the R-PURE tripwire for the v2 fold: the reducer
// source (reducer.go, reducer_state.go, vocab.go) must not import any package
// that performs I/O, reads a clock, or touches the store — so the fold stays a
// pure function of the event stream (DROP+refold byte-identity, DET-T-17). It
// mirrors the fold package's own purity_test.go, scoped to the reducer files.
//
// The executor (engine.go, plan.go) legitimately imports the store and time and
// is deliberately out of scope here.
func TestReducerFilesArePure(t *testing.T) {
	reducerFiles := []string{"reducer.go", "reducer_state.go", "vocab.go"}
	forbidden := []string{
		"time", "os", "database/sql", "net", "io/ioutil", "os/exec",
		"internal/graphstore\"", // the store itself; the pure fold view lives in graphstore/fold
	}
	for _, file := range reducerFiles {
		fset := token.NewFileSet()
		f, err := parser.ParseFile(fset, file, nil, parser.ImportsOnly)
		if err != nil {
			t.Fatalf("parse %s: %v", file, err)
		}
		for _, imp := range f.Imports {
			path, err := strconv.Unquote(imp.Path.Value)
			if err != nil {
				t.Fatalf("%s: bad import literal %s", file, imp.Path.Value)
			}
			for _, bad := range forbidden {
				needle := strings.TrimSuffix(bad, "\"")
				if path == needle || (strings.HasSuffix(bad, "\"") && strings.HasSuffix(path, needle)) {
					t.Errorf("%s imports %q, which breaks reducer purity (no I/O, clock, or store in the fold)", file, path)
				}
			}
		}
	}
}
