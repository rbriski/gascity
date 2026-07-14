// Package doltorphan implements a symptom-based fallback sweep for orphaned
// Dolt store directories: directories that carry a .dolt marker, are no
// longer held open by any process, and are old enough to be safely assumed
// abandoned. It ports the heuristic proven in production by
// gc-test-dolt-reaper.sh section 4, which stopped a recurring ENOSPC
// incident (2026-07-06, /tmp filled twice by leaked stores) into a
// reusable, testable Go primitive with no shell-out to that script.
package doltorphan

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

// DefaultMinAge is the minimum candidate age before an orphaned Dolt store
// directory is eligible for removal, ported from gc-test-dolt-reaper.sh's
// STORE_AGE_MIN=10. This is deliberately shorter than this codebase's
// general 60-minute orphan-process age floor (AGE_MIN in that same script,
// used for reaping orphaned dolt sql-server processes and stale build
// dirs): a fast test-loop leak creates stores faster than a 60-minute gate
// can ever age them out, which is why that incident recurred ("the mop
// never caught the tap", gc-test-dolt-reaper.sh comment). Callers under
// disk-space pressure should pass 0 instead, mirroring that script's
// adaptive floor (STORE_AGE_MIN drops to 0 once /tmp is >=85% full) --
// SweepConfig.MinAge has no such threshold built in, since deciding what
// counts as "space pressure" is a caller policy choice, not something this
// primitive should hardcode.
const DefaultMinAge = 10 * time.Minute

// doltMarkerMaxDepth mirrors gc-test-dolt-reaper.sh's
// `find "$d" -maxdepth 3 -type d -name '.dolt'`.
const doltMarkerMaxDepth = 3

// lsofTimeout mirrors gc-test-dolt-reaper.sh's `timeout 30 lsof -w`.
const lsofTimeout = 30 * time.Second

// SweepConfig configures a Sweep pass.
type SweepConfig struct {
	// Roots are scanned for immediate child directories as removal
	// candidates, mirroring gc-test-dolt-reaper.sh's
	// `find $root -maxdepth 1 -type d`. Unlike that script, candidates are
	// NOT filtered by a `tmp.*` name prefix: age + .dolt marker +
	// lsof-unheld are the only signals that establish "safe to remove," so
	// any directory directly under a root is a candidate. This broadening
	// is deliberate -- see the doltorphan package-level design note in
	// bead ga-ntbpyb.2: the confirmed leak exemplar
	// (TestReaperWorkflowRootCleanupRealDoltSemantics) leaks under
	// t.TempDir()-style naming (/tmp/Test.../001/dolt), which does not
	// carry a tmp.* prefix and would be invisible to the original filter.
	Roots []string

	// MinAge is the minimum directory age (mtime) before a candidate is
	// considered. It has no implicit default: zero is a legitimate,
	// distinct value meaning "reap any lsof-free, markered candidate
	// regardless of age" (the script's own space-pressure behavior).
	// Callers that want the production default should pass DefaultMinAge
	// explicitly.
	MinAge time.Duration

	// Now returns the current time. Defaults to time.Now.
	Now func() time.Time
	// RunLsof returns the raw combined output of a single system-wide
	// lsof scan (mirroring the script's one `lsof -w` call reused across
	// all candidates). Defaults to invoking the real `lsof -w` binary
	// with a 30s timeout. Any error -- including "lsof not installed" --
	// fails CLOSED: Sweep removes nothing on that pass, since "is
	// anything open under this directory" cannot be established. This
	// intentionally does NOT mirror gc-test-dolt-reaper.sh literally:
	// that script's `inuse` variable silently goes empty on lsof failure
	// (treating a probe failure the same as "nothing is open," which is
	// unsafe) -- fixed here per this package's fail-closed requirement.
	RunLsof func() (string, error)
	// RemoveAll removes a candidate directory tree. Defaults to
	// os.RemoveAll.
	RemoveAll func(path string) error
}

// RemoveError records a single candidate's removal failure without aborting
// the rest of the sweep.
type RemoveError struct {
	Path string
	Err  error
}

// Result reports what a Sweep pass did.
type Result struct {
	// Removed lists candidate directories that were successfully removed.
	Removed []string
	// RemoveErrors lists candidates that passed all safety checks but
	// failed to remove.
	RemoveErrors []RemoveError
	// LsofUnavailable is true when RunLsof errored, in which case no
	// removals were attempted this pass (fail closed).
	LsofUnavailable bool
}

// Sweep scans cfg.Roots for orphaned Dolt store directories and removes
// those that are old enough, carry a .dolt marker within 3 levels, and are
// not currently referenced by any open file per RunLsof. See SweepConfig
// for the exact semantics of each signal and the deliberate deviations from
// gc-test-dolt-reaper.sh section 4, which this ports.
func Sweep(cfg SweepConfig) Result {
	now := cfg.Now
	if now == nil {
		now = time.Now
	}
	runLsof := cfg.RunLsof
	if runLsof == nil {
		runLsof = runRealLsof
	}
	removeAll := cfg.RemoveAll
	if removeAll == nil {
		removeAll = os.RemoveAll
	}

	var result Result

	candidates := discoverCandidates(cfg.Roots, now(), cfg.MinAge)
	if len(candidates) == 0 {
		return result
	}

	lsofOutput, err := runLsof()
	if err != nil {
		result.LsofUnavailable = true
		return result
	}

	for _, dir := range candidates {
		if !hasDoltMarker(dir) {
			continue
		}
		if pathIsOpen(lsofOutput, dir) {
			continue
		}
		if err := removeAll(dir); err != nil {
			result.RemoveErrors = append(result.RemoveErrors, RemoveError{Path: dir, Err: err})
			continue
		}
		result.Removed = append(result.Removed, dir)
	}
	return result
}

// discoverCandidates lists immediate child directories of each root whose
// mtime is older than minAge. Roots that cannot be read are silently
// skipped, mirroring gc-test-dolt-reaper.sh's `find ... 2>/dev/null`
// tolerance of races and missing paths.
func discoverCandidates(roots []string, now time.Time, minAge time.Duration) []string {
	var candidates []string
	for _, root := range roots {
		entries, err := os.ReadDir(root)
		if err != nil {
			continue
		}
		for _, entry := range entries {
			if !entry.IsDir() {
				continue
			}
			path := filepath.Join(root, entry.Name())
			info, err := os.Stat(path)
			if err != nil {
				continue
			}
			if now.Sub(info.ModTime()) > minAge {
				candidates = append(candidates, path)
			}
		}
	}
	return candidates
}

// hasDoltMarker reports whether a directory named ".dolt" exists within
// doltMarkerMaxDepth levels of dir, mirroring
// `find "$dir" -maxdepth 3 -type d -name '.dolt'`.
func hasDoltMarker(dir string) bool {
	return markerWithin(dir, 0)
}

func markerWithin(dir string, depth int) bool {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return false
	}
	childDepth := depth + 1
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		if entry.Name() == ".dolt" {
			return true
		}
		if childDepth < doltMarkerMaxDepth {
			if markerWithin(filepath.Join(dir, entry.Name()), childDepth) {
				return true
			}
		}
	}
	return false
}

// pathIsOpen reports whether any whitespace-delimited field in lsofOutput
// names a path equal to or nested under dir. This generalizes
// gc-test-dolt-reaper.sh's `lsof -w | grep -oE '/tmp/tmp\.[A-Za-z0-9]+'`
// (which only recognizes the tmp.* name shape) to the broadened candidate
// set this package uses -- see SweepConfig.Roots.
func pathIsOpen(lsofOutput, dir string) bool {
	dir = filepath.Clean(dir)
	prefix := dir + string(filepath.Separator)
	for _, field := range strings.Fields(lsofOutput) {
		if field == dir || strings.HasPrefix(field, prefix) {
			return true
		}
	}
	return false
}

// runRealLsof is the production default for SweepConfig.RunLsof: a single
// system-wide `lsof -w` invocation bounded by lsofTimeout, mirroring
// gc-test-dolt-reaper.sh's `timeout 30 lsof -w`.
func runRealLsof() (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), lsofTimeout)
	defer cancel()
	out, err := exec.CommandContext(ctx, "lsof", "-w").Output()
	if err != nil {
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) {
			return "", fmt.Errorf("running lsof -w: %w (stderr: %s)", err, exitErr.Stderr)
		}
		return "", fmt.Errorf("running lsof -w: %w", err)
	}
	return string(out), nil
}
