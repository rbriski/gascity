package rig

import (
	"errors"
	"fmt"

	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/fsys"
)

// Deps carries everything the provisioning core needs. It follows the
// internal/sling.SlingDeps discipline: a small set of required infra fields plus
// nil-optional injected funcs, so both the CLI (cmd/gc) and the API-side
// controllerState can drive the same core without internal/rig importing
// package main.
//
// The three infra fields (FS, CityPath, Cfg) are required and checked by
// validateDeps. The injected funcs are validated lazily at the step that needs
// them (documented per field), because not every caller exercises every step —
// this matches sling's "nil = skip" convention.
type Deps struct {
	// FS is the filesystem seam. cmd/gc passes fsys.OSFS{}; tests pass a fake.
	FS fsys.FS
	// CityPath is the absolute path to the city directory.
	CityPath string
	// Cfg is the city config the caller loaded for edit.
	Cfg *config.City

	// InitStore initializes the rig's bead store (cmd/gc initDirIfReady). It
	// returns deferred=true when live init is punted to the controller/startup.
	InitStore func(cityPath, dir, prefix string) (deferred bool, err error)
	// InitAndHook is the deferred-fallback deeper store init (cmd/gc initAndHookDir).
	InitAndHook func(cityPath, dir, prefix string) error
	// ComposePacks resolves the rig's bundled imports and returns a commit closure
	// that writes packs.lock only AFTER the city.toml append (cmd/gc
	// ensureBundledRigImportsInstalled), preserving the "city.toml written last"
	// atomicity invariant.
	ComposePacks func(cityPath string, imports []config.BoundImport) (pinned []config.BoundImport, commit func() error, err error)
	// WriteRoutes regenerates every rig's routes.jsonl (cmd/gc
	// collectRigRoutes + writeAllRoutes).
	WriteRoutes func(cityPath string, cfg *config.City) error
	// ProbeBranch returns the rig's git default branch, or "" when unknown.
	// nil = skip the probe.
	ProbeBranch func(rigPath string) string
	// PostProvision runs caller-specific side effects after the core writes
	// succeed (CLI: hooks/formulas/.env/reload; API: the mutateAndPoke config
	// commit + reconciler Poke). nil = skip.
	PostProvision func(pc ProvisionContext) error

	// OnStep receives incremental provisioning progress. The CLI renders strings;
	// the API emits typed events (G20). nil = no-op. This push seam is the one
	// deliberate departure from SlingDeps, whose warnings ride the return struct.
	OnStep func(step ProvisionStep)
}

// ProvisionRequest is the caller's rig-add intent. It mirrors the current
// doRigAddWithResult parameters minus the fs and the io.Writers.
type ProvisionRequest struct {
	Name           string
	Path           string // resolved rig path; the caller does any CWD-relative resolution
	Prefix         string // explicit prefix override; "" derives from Name
	DefaultBranch  string // explicit override; "" probes via Deps.ProbeBranch
	Includes       []string
	StartSuspended bool
	Adopt          bool
}

// ProvisionResult carries the structured outcome the caller renders (CLI
// strings) or projects onto events/JSON (API). It replaces the stdout/stderr
// writers the CLI function used to take.
type ProvisionResult struct {
	// Deferred reports that bead-store init was punted to the controller.
	Deferred bool
	// Warnings holds warn-and-continue messages (non-fatal steps).
	Warnings []string
	// Steps is the ordered progress trace (also delivered live via Deps.OnStep).
	Steps []ProvisionStep
}

// ProvisionStep is one unit of provisioning progress
// (e.g. "beads-init", "packs", "config", "routes").
type ProvisionStep struct {
	Name   string // stable machine name
	Detail string // human-readable detail
	Warn   bool   // true when the step reports a warn-and-continue condition
}

// ProvisionContext is handed to Deps.PostProvision after the core writes succeed.
type ProvisionContext struct {
	RigPath  string
	Rig      config.Rig
	Deferred bool
}

// ErrNotImplemented is returned by the C2.0 Provision stub; the orchestration
// lands in C2.2.
var ErrNotImplemented = errors.New("rig: Provision not yet implemented")

// Provision runs the full rig-add provisioning against the injected deps.
//
// C2.0 stub: it validates deps and returns ErrNotImplemented. The orchestration
// (steps 1-17 of the extracted doRigAddWithResult) is filled in at C2.2.
func Provision(deps Deps, req ProvisionRequest) (config.Rig, ProvisionResult, error) {
	if err := validateDeps(deps); err != nil {
		return config.Rig{}, ProvisionResult{}, err
	}
	if err := validateRequest(req); err != nil {
		return config.Rig{}, ProvisionResult{}, err
	}
	return config.Rig{}, ProvisionResult{}, ErrNotImplemented
}

// validateRequest rejects a structurally-invalid rig-add request before any
// provisioning work. Name and Path are always required; the caller is
// responsible for resolving Path to an absolute location.
func validateRequest(req ProvisionRequest) error {
	if req.Name == "" {
		return errors.New("rig: ProvisionRequest.Name is required")
	}
	if req.Path == "" {
		return errors.New("rig: ProvisionRequest.Path is required")
	}
	return nil
}

// validateDeps enforces the required infra fields. Injected funcs are validated
// lazily at their step (see the Deps field docs), matching sling's validateDeps.
func validateDeps(d Deps) error {
	if d.FS == nil {
		return depErr("FS")
	}
	if d.CityPath == "" {
		return depErr("CityPath")
	}
	if d.Cfg == nil {
		return depErr("Cfg")
	}
	return nil
}

// depErr is the error shape validateDeps returns for a missing required field.
func depErr(field string) error {
	return fmt.Errorf("rig: Deps.%s is required", field)
}
