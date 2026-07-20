package main

import (
	"encoding/json"
	"fmt"
	"io"
	"path/filepath"
	"strings"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/events"
	"github.com/gastownhall/gascity/internal/git"
	"github.com/gastownhall/gascity/internal/pathutil"
	"github.com/gastownhall/gascity/internal/sling"
)

// reapClosedBeadWorktrees scans registered git worktrees for each rig and
// removes any that are associated with a closed bead and pass all safety
// gates (no uncommitted work, no unpushed commits, no stashes). Named
// session home directories are never removed. Returns the number of
// worktrees successfully removed.
//
// Worktrees are discovered via `git worktree list` against each rig's own
// repo root, not by walking cityPath/.gc/worktrees/<rig>/ one level deep.
// An agent's work_dir template supplies arbitrary intermediate directory
// segments between the rig and the per-bead worktree (e.g.
// rig/polecats/<agent-name>/<bead-id>-slug), so a shallow directory walk
// misses real worktrees nested below role/agent-name directories — asking
// git directly is nesting-agnostic and matches the SDK's zero-hardcoded-role
// rule (this package never assumes a "polecats" or "refinery" path segment).
// Worktree removal also runs from the rig's repo root: cityPath is a
// directory that contains per-rig checkouts, never a git repo itself, so
// `git worktree remove` scoped to cityPath always fails.
func reapClosedBeadWorktrees(
	cityPath string,
	cfg *config.City,
	rigBeadStores map[string]beads.Store,
	rec events.Recorder,
	stderr io.Writer,
) int {
	if stderr == nil {
		stderr = io.Discard
	}
	if rec == nil {
		rec = events.Discard
	}
	if cfg == nil || len(rigBeadStores) == 0 {
		return 0
	}

	// Build a guard set of session home names so agent template directories
	// are never touched.
	sessionHomes := make(map[string]bool, len(cfg.Agents))
	for i := range cfg.Agents {
		if name := cfg.Agents[i].BindingQualifiedName(); name != "" {
			sessionHomes[name] = true
		}
	}

	wtRoot := filepath.Join(cityPath, ".gc", "worktrees")
	reaped := 0

	for rigName, store := range rigBeadStores {
		if store == nil {
			continue
		}
		rigRoot := rigRootForName(rigName, cfg.Rigs)
		if rigRoot == "" {
			continue
		}
		mainRepo := git.New(rigRoot)
		if !mainRepo.IsRepo() {
			continue
		}
		worktrees, err := mainRepo.WorktreeList()
		if err != nil {
			fmt.Fprintf(stderr, "reapClosedBeadWorktrees: listing worktrees for rig %s: %v\n", rigName, err) //nolint:errcheck
			continue
		}
		for _, wt := range worktrees {
			worktreePath := wt.Path

			// Scope gate: only act on paths strictly under the worktree
			// root — never the rig's own primary checkout or a worktree
			// registered elsewhere.
			if !isStrictlyUnderDir(wtRoot, worktreePath) {
				continue
			}

			name := filepath.Base(worktreePath)

			// Session home guard: never touch agent template directories.
			if sessionHomes[name] {
				continue
			}

			// Extract a bead ID candidate from the worktree's leaf directory name.
			beadID := extractBeadIDFromWorktreeName(cfg, name)
			if beadID == "" {
				continue
			}

			// Confirm the bead exists and is closed in this rig's store.
			bead, err := store.Get(beadID)
			if err != nil || bead.Status != "closed" {
				// ErrNotFound, transient error, or bead not yet closed — skip.
				continue
			}

			// Safety checks: run from the worktree directory so git status
			// and stash list apply to the worktree's branch.
			wg := git.New(worktreePath)
			hasUncommitted := wg.HasUncommittedWork()
			hasUnpushed, _ := wg.HasUnpushedCommitsResult()
			hasStashes, _ := wg.HasStashesResult()

			if hasUncommitted || hasUnpushed || hasStashes {
				reason := fmt.Sprintf("uncommitted=%v unpushed=%v stashes=%v", hasUncommitted, hasUnpushed, hasStashes)
				fmt.Fprintf(stderr, //nolint:errcheck
					"reapClosedBeadWorktrees: skipping %s (bead %s closed but unsafe: %s)\n",
					worktreePath, beadID, reason,
				)
				if raw, err := json.Marshal(events.BeadWorktreeReapSkippedPayload{
					BeadID: beadID,
					Path:   worktreePath,
					Rig:    rigName,
					Reason: reason,
				}); err == nil {
					rec.Record(events.Event{
						Type:    events.BeadWorktreeReapSkipped,
						Actor:   "gc",
						Subject: beadID,
						Payload: raw,
					})
				}
				continue
			}

			// Capture branch before removal — the worktree dir will be gone after.
			branch, _ := wg.CurrentBranch()

			// Remove the worktree. git worktree remove must be run from the
			// main repo root, not from within the worktree being removed.
			if err := mainRepo.WorktreeRemove(worktreePath, false); err != nil {
				fmt.Fprintf(stderr, "reapClosedBeadWorktrees: removing %s: %v\n", worktreePath, err) //nolint:errcheck
				continue
			}
			fmt.Fprintf(stderr, //nolint:errcheck
				"reapClosedBeadWorktrees: removed worktree %s for closed bead %s\n",
				worktreePath, beadID,
			)
			if raw, err := json.Marshal(events.BeadWorktreeReapedPayload{
				BeadID: beadID,
				Path:   worktreePath,
				Rig:    rigName,
				Branch: branch,
			}); err == nil {
				rec.Record(events.Event{
					Type:    events.BeadWorktreeReaped,
					Actor:   "gc",
					Subject: beadID,
					Payload: raw,
				})
			}
			reaped++
		}
	}
	return reaped
}

// extractBeadIDFromWorktreeName scans consecutive dash-separated segment pairs
// in name for one that LooksLikeConfiguredBeadID. Returns the first match, or
// "" if none. Handles names like "builder-ga-34q3ss-pr2738" → "ga-34q3ss" and
// bare "ga-06kfi6" → "ga-06kfi6".
func extractBeadIDFromWorktreeName(cfg *config.City, name string) string {
	if name == "" || cfg == nil {
		return ""
	}
	parts := strings.Split(name, "-")
	for i := 0; i+1 < len(parts); i++ {
		candidate := parts[i] + "-" + parts[i+1]
		if sling.LooksLikeConfiguredBeadID(cfg, candidate) {
			return candidate
		}
	}
	return ""
}

// isStrictlyUnderDir reports whether path is strictly contained within dir
// (i.e., it is not dir itself and has dir as a prefix component). Both
// paths are symlink-resolved before comparison: worktree paths returned by
// `git worktree list` are canonicalized (e.g. macOS /tmp -> /private/tmp),
// so a lexical-only comparison against an un-resolved dir would spuriously
// reject worktrees actually inside it.
func isStrictlyUnderDir(dir, path string) bool {
	dir = pathutil.NormalizePathForCompare(dir)
	path = pathutil.NormalizePathForCompare(path)
	if dir == "" || path == "" {
		return false
	}
	rel, err := filepath.Rel(dir, path)
	if err != nil {
		return false
	}
	return rel != "." && !strings.HasPrefix(rel, "..")
}
