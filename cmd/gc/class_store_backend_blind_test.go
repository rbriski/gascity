package main

import (
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"testing"
)

// TestClassStoreDispatchIsBackendBlind pins the load-bearing invariant of the
// domain/infra store split (E2.5): resolveClassStore — the single per-class
// dispatch point — must choose which store owns a bead by the store BOUNDARY
// (infra-store presence) and the CLASS name ONLY, never by the backend kind.
//
// This is the graph-split audit's leverage-bug lesson turned into a permanent CI
// invariant: GraphOnlyListFor once gated on "is the backend sqlite", which made
// four shipped fixes silently dead when the backend differed. Class routing that
// reads cfg.Beads.Provider / .Backend / a "sqlite"/"postgres"/"doltlite" literal
// would reintroduce exactly that failure mode — a relocated class silently
// resolving to the wrong store on a backend the author didn't test. Backend kind
// may only choose transport/credentials/lifecycle, never class ownership.
func TestClassStoreDispatchIsBackendBlind(t *testing.T) {
	_, currentFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller failed")
	}
	path := filepath.Join(filepath.Dir(currentFile), "class_store.go")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile(%q): %v", path, err)
	}
	body := extractFuncBody(t, string(data), "resolveClassStore")

	// Strip line comments so a doc/rationale mention of a backend word does not
	// trip the guard; only executable dispatch logic is checked.
	var code strings.Builder
	for _, line := range strings.Split(body, "\n") {
		if idx := strings.Index(line, "//"); idx >= 0 {
			line = line[:idx]
		}
		code.WriteString(line)
		code.WriteString("\n")
	}
	forbidden := []string{
		"Backend", "Provider", "sqlite", "postgres", "doltlite", "graph_store",
	}
	lower := strings.ToLower(code.String())
	for _, needle := range forbidden {
		if strings.Contains(code.String(), needle) || strings.Contains(lower, strings.ToLower(needle)) {
			t.Errorf("resolveClassStore body references backend token %q — class-store dispatch must gate on the infra-store boundary + class name ONLY, never the backend kind (the graph-split audit's leverage bug). Backend kind selects transport/credentials/lifecycle, not which store owns a bead.", needle)
		}
	}
}

// extractFuncBody returns the source text (signature + body) of the named
// top-level function from a Go source string, using brace balancing from the
// signature's opening brace. Sufficient for the single-function guard above.
func extractFuncBody(t *testing.T, src, name string) string {
	t.Helper()
	re := regexp.MustCompile(`(?m)^func ` + regexp.QuoteMeta(name) + `\(`)
	loc := re.FindStringIndex(src)
	if loc == nil {
		t.Fatalf("func %s not found in class_store.go", name)
	}
	rest := src[loc[0]:]
	open := strings.IndexByte(rest, '{')
	if open < 0 {
		t.Fatalf("no opening brace for func %s", name)
	}
	depth := 0
	for i := open; i < len(rest); i++ {
		switch rest[i] {
		case '{':
			depth++
		case '}':
			depth--
			if depth == 0 {
				return rest[:i+1]
			}
		}
	}
	t.Fatalf("unbalanced braces for func %s", name)
	return ""
}
