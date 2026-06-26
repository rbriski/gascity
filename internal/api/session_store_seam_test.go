package api

import (
	"os"
	"regexp"
	"strings"
	"testing"
)

// TestMaterializeReassignSplitsSessionAndWorkStores guards the two-store split
// in reassignContinuityIneligibleNamedSessionState (the one work-bead
// entanglement on the API session surface). When [beads.classes.sessions]
// relocates, the materialize path's session store no longer holds work beads.
// The retired-session cleanup must therefore reassign WORK and external-message
// beads against the work store, while durable session WAITs (gc:wait, which
// relocate WITH the session class) follow the session store. A naive single
// store reuses the (relocated) session store for the work List+Update, finds no
// work, and silently orphans the retired session's open work — byte-identical
// at the default backend, so no runtime test catches the regression. This
// source-level guard pins the split:
//   - reassignOpenWorkAssignedToSession  -> workStore
//   - extmsg.ReassignSessionBindings     -> workStore
//   - extmsg.ReassignSessionParticipants -> workStore
//   - session.ReassignWaits              -> store (the session store)
//
// and requires workStore to be sourced from the work store (CityBeadStore).
func TestMaterializeReassignSplitsSessionAndWorkStores(t *testing.T) {
	const file = "session_resolution.go"
	data, err := os.ReadFile(file)
	if err != nil {
		t.Fatalf("ReadFile(%q): %v", file, err)
	}
	src := string(data)

	body, ok := funcBody(src, "func (s *Server) reassignContinuityIneligibleNamedSessionState(")
	if !ok {
		t.Fatalf("could not locate reassignContinuityIneligibleNamedSessionState in %s", file)
	}

	if !strings.Contains(body, "workStore := s.state.CityBeadStore()") {
		t.Fatalf("reassignContinuityIneligibleNamedSessionState must derive the work store as "+
			"`workStore := s.state.CityBeadStore()`; body:\n%s", body)
	}

	// First store argument of each reassignment call must match the class of the
	// beads it touches.
	wantWorkStore := map[string]string{
		"reassignOpenWorkAssignedToSession":  "workStore",
		"extmsg.ReassignSessionBindings":     "workStore",
		"extmsg.ReassignSessionParticipants": "workStore",
		"session.ReassignWaits":              "store",
	}
	for fn, wantArg := range wantWorkStore {
		gotArg, ok := firstCallArg(body, fn)
		if !ok {
			t.Fatalf("expected a call to %s in reassignContinuityIneligibleNamedSessionState", fn)
		}
		// extmsg.* pass a ctx first; the store is the second arg there.
		if strings.HasPrefix(fn, "extmsg.") {
			gotArg, ok = nthCallArg(body, fn, 1)
			if !ok {
				t.Fatalf("expected a store argument to %s", fn)
			}
		}
		if gotArg != wantArg {
			t.Fatalf("%s must take %q as its store argument, got %q — the session/work store split is broken",
				fn, wantArg, gotArg)
		}
	}
}

// funcBody returns the brace-balanced body of the function whose signature line
// starts with sig. ok is false when the signature is not found.
func funcBody(src, sig string) (body string, ok bool) {
	idx := strings.Index(src, sig)
	if idx < 0 {
		return "", false
	}
	open := strings.IndexByte(src[idx:], '{')
	if open < 0 {
		return "", false
	}
	start := idx + open
	depth := 0
	for i := start; i < len(src); i++ {
		switch src[i] {
		case '{':
			depth++
		case '}':
			depth--
			if depth == 0 {
				return src[start : i+1], true
			}
		}
	}
	return "", false
}

// firstCallArg returns the first comma/paren-delimited argument of the first
// call to fn within body.
func firstCallArg(body, fn string) (string, bool) {
	return nthCallArg(body, fn, 0)
}

// nthCallArg returns the n-th (0-based) top-level argument of the first call to
// fn within body. Nested parentheses inside an argument are skipped.
func nthCallArg(body, fn string, n int) (string, bool) {
	re := regexp.MustCompile(regexp.QuoteMeta(fn) + `\(`)
	loc := re.FindStringIndex(body)
	if loc == nil {
		return "", false
	}
	i := loc[1] // just past the opening paren
	depth := 0
	arg := strings.Builder{}
	argIdx := 0
	for ; i < len(body); i++ {
		c := body[i]
		switch {
		case c == '(' || c == '[':
			depth++
			arg.WriteByte(c)
		case c == ')' || c == ']':
			if c == ')' && depth == 0 {
				if argIdx == n {
					return strings.TrimSpace(arg.String()), true
				}
				return "", false
			}
			depth--
			arg.WriteByte(c)
		case c == ',' && depth == 0:
			if argIdx == n {
				return strings.TrimSpace(arg.String()), true
			}
			argIdx++
			arg.Reset()
		default:
			arg.WriteByte(c)
		}
	}
	return "", false
}
