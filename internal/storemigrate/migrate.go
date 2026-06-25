// Package storemigrate copies beads of a relocated coordination class from a
// source store (the bd/Dolt work store) into a destination store (the class's
// embedded SQLite store), ID-preserving and idempotent, so the read path can stop
// querying Dolt for that class. It is the engine behind the `gc` migration
// command (the user-facing "migrate all dolt-backed beads that have a new home in
// sqlite" operation), distinct from the clean drain-then-switch path: this copies
// existing history so the cutover is immediate rather than waiting for drain.
package storemigrate

import (
	"errors"
	"fmt"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/coordclass"
)

// Report summarizes a migration run.
type Report struct {
	// Scanned is the number of source beads matching the selector.
	Scanned int
	// Migrated is the number copied into the destination this run.
	Migrated int
	// Skipped is the number already present in the destination (idempotent re-runs).
	Skipped int
}

// Migrate copies every bead in src for which selects(b) is true into dst,
// preserving IDs and all fields (dst.Create honors a caller-pinned ID). It is
// idempotent: a bead already present in dst (by ID) is skipped, so re-running is
// safe. It scans both storage tiers and includes closed beads so the copy is
// complete. It does not delete from src — the caller decides when to retire the
// source rows (after verifying the cutover).
func Migrate(src, dst beads.Store, selects func(beads.Bead) bool) (Report, error) {
	var rep Report
	if src == nil || dst == nil {
		return rep, errors.New("storemigrate: nil store")
	}
	if selects == nil {
		return rep, errors.New("storemigrate: nil selector")
	}
	all, err := src.List(beads.ListQuery{AllowScan: true, IncludeClosed: true, TierMode: beads.TierBoth})
	if err != nil {
		return rep, fmt.Errorf("storemigrate: list source: %w", err)
	}
	for _, b := range all {
		if !selects(b) {
			continue
		}
		rep.Scanned++
		switch _, getErr := dst.Get(b.ID); {
		case getErr == nil:
			rep.Skipped++
			continue
		case !errors.Is(getErr, beads.ErrNotFound):
			return rep, fmt.Errorf("storemigrate: probing destination for %q: %w", b.ID, getErr)
		}
		if _, err := dst.Create(b); err != nil {
			return rep, fmt.Errorf("storemigrate: creating %q in destination: %w", b.ID, err)
		}
		rep.Migrated++
	}
	return rep, nil
}

// TypeSelector selects beads by exact type (e.g. "message" for the mail cutover,
// which migrates mail without extmsg — extmsg is a deferred follow-on).
func TypeSelector(beadType string) func(beads.Bead) bool {
	return func(b beads.Bead) bool { return b.Type == beadType }
}

// ClassSelector selects beads whose coordination class (per coordclass.Classify,
// the single routing source of truth) matches className (e.g. "messaging",
// "sessions", "orders", "nudges"). An unrecognized class name selects nothing.
func ClassSelector(className string) func(beads.Bead) bool {
	target, ok := classByName(className)
	if !ok {
		return func(beads.Bead) bool { return false }
	}
	return func(b beads.Bead) bool { return coordclass.Classify(b) == target }
}

// classByName resolves a class name to its coordclass.Class via the stable
// String() contract.
func classByName(name string) (coordclass.Class, bool) {
	for _, c := range coordclass.Classes() {
		if c.String() == name {
			return c, true
		}
	}
	return coordclass.ClassWork, false
}
