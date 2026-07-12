package rig

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/fsys"
	"github.com/gastownhall/gascity/internal/git"
)

// Provision runs the full rig-add provisioning against the injected deps.
//
// It is the extracted core of the CLI's doRigAddWithResult: operations are
// ordered so that city.toml is written last — if any earlier step fails,
// config is unchanged. The city config write, the deferred packs.lock commit,
// and the routes regeneration run under a topology snapshot so a failure in
// that window rolls the filesystem back atomically.
//
// Fatal conditions return an error whose Error() is exactly the text the CLI
// prints after the "gc rig add: " prefix. Non-fatal progress and warnings ride
// Deps.OnStep (and ProvisionResult.Steps/Warnings); the caller renders them.
//
// The pre-mutation decision steps (re-add detection, prefix resolution, config
// assembly, store/prefix guards, banners, store init) are factored into focused
// helpers so this orchestrator stays a readable, ordered sequence; each helper
// preserves the exact ordering, text, and behavior of the inlined step.
func Provision(deps Deps, req ProvisionRequest) (config.Rig, ProvisionResult, error) {
	if err := validateDeps(deps); err != nil {
		return config.Rig{}, ProvisionResult{}, err
	}
	if err := validateRequest(req); err != nil {
		return config.Rig{}, ProvisionResult{}, err
	}

	fs := deps.FS
	cfg := deps.Cfg
	cityPath := deps.CityPath
	rigPath := req.Path
	name := req.Name
	tomlPath := filepath.Join(cityPath, "city.toml")

	var result ProvisionResult
	emit := func(step ProvisionStep) {
		result.Steps = append(result.Steps, step)
		if step.Warn {
			result.Warnings = append(result.Warnings, step.Detail)
		}
		if deps.OnStep != nil {
			deps.OnStep(step)
		}
	}
	storeContract := func() bool { return deps.StoreContract != nil && deps.StoreContract(cityPath) }
	doltSkip := func() bool { return deps.DoltSkip != nil && deps.DoltSkip() }

	// Step 1: trim and drop empty --include entries so `--include=` or
	// `--include " "` doesn't persist a blank pack path that downstream
	// resolution reads as the city root.
	includes := req.Includes[:0:0]
	for _, inc := range req.Includes {
		if trimmed := strings.TrimSpace(inc); trimmed != "" {
			includes = append(includes, trimmed)
		}
	}

	// Step 2: stat the rig path. Shared with the CLI's StatRigPath preflight so
	// a bad rig path is reported before a config-load failure, preserving the
	// original ordering.
	rigPathExists, err := StatRigPath(fs, rigPath, req.Adopt)
	if err != nil {
		return config.Rig{}, result, err
	}

	// Steps 2.5-3: materialize the working tree from --git-url when requested,
	// then detect git and resolve the default branch.
	rigPathExists, hasGit, defaultBranchOverride, resolvedDefaultBranch, err := prepareRigWorkingTree(deps, req, fs, rigPath, rigPathExists)
	if err != nil {
		return config.Rig{}, result, err
	}

	// Step 4: canonicalize --include tokens that name a materialized builtin
	// pack so the flag honors its --help promise of "canonical rig imports".
	includes = canonicalizeBuiltinPackIncludes(fs, cityPath, includes, cfg.Packs)

	// Step 5: resolve the explicit bundled rig imports (call #1).
	explicitRigImports, commitRigImports, err := deps.ComposePacks(cityPath, config.BoundImportsFromLegacySources(includes, cfg.Packs))
	if err != nil {
		return config.Rig{}, result, fmt.Errorf("installing bundled rig imports: %w", err)
	}

	// Step 6: re-add detection.
	reAdd, reAddNeedsConfigWrite, existingRigIdx, existingRig, err := detectReAdd(cfg, name, cityPath, rigPath)
	if err != nil {
		return config.Rig{}, result, err
	}

	// Step 7: prefix resolution, collision checks, default-branch backfill.
	prefix, err := resolveRigPrefix(reAdd, existingRig, req, cfg, name)
	if err != nil {
		return config.Rig{}, result, err
	}
	if reAdd && existingRig != nil && existingRig.EffectiveDefaultBranch() == "" && resolvedDefaultBranch != "" {
		reAddNeedsConfigWrite = true
	}

	// Step 8: build nextCfg.
	nextCfg, defaultRigImports, commitRigImports, needsValidation, err := buildNextConfig(
		deps, req, cfg, fs, cityPath, name, rigPath, resolvedDefaultBranch,
		reAdd, reAddNeedsConfigWrite, existingRigIdx, explicitRigImports, commitRigImports)
	if err != nil {
		return config.Rig{}, result, err
	}

	// Step 9: validate rigs before any filesystem mutation.
	if needsValidation {
		if err := config.ValidateRigs(nextCfg.Rigs, config.EffectiveHQPrefix(nextCfg)); err != nil {
			return config.Rig{}, result, err
		}
	}

	// Step 10: create the rig directory when missing.
	if !rigPathExists {
		if err := fs.MkdirAll(rigPath, 0o755); err != nil {
			return config.Rig{}, result, fmt.Errorf("creating %s: %w", rigPath, err)
		}
	}

	// Step 11: adopt validation, prefix-mismatch guard, fresh-add store guard.
	if err := checkStoreAndPrefixGuards(fs, req, reAdd, prefix, name, rigPath); err != nil {
		return config.Rig{}, result, err
	}

	// --- Phase 1: Infrastructure (all fallible, before touching city.toml) ---

	// Step 12: banner + warn lines.
	emitProvisionBanners(emit, reAdd, req, existingRig, cfg, includes, explicitRigImports, defaultRigImports, hasGit, rigPath, prefix, resolvedDefaultBranch, defaultBranchOverride, name)

	// Step 13: beads-store init.
	deferred, err := initBeadsStore(deps, req, emit, cityPath, rigPath, prefix, storeContract, doltSkip)
	if err != nil {
		return config.Rig{}, result, err
	}
	result.Deferred = deferred

	// Steps 14-16: snapshot the topology, write config, commit packs.lock, and
	// regenerate routes as one panic-safe atomic write window (city.toml written
	// last; any failure rolls the filesystem back through the snapshot).
	if err := commitTopology(deps, fs, cityPath, tomlPath, nextCfg, reAdd, reAddNeedsConfigWrite, commitRigImports, emit); err != nil {
		return config.Rig{}, result, err
	}

	// Resolve the returned rig from the post-write config.
	resultRig := resolveResultRig(nextCfg, name, rigPath, strings.ToLower(req.Prefix), resolvedDefaultBranch, req.StartSuspended)

	// Step 17: caller-specific side effects (CLI hooks/formulas/.env/reload).
	// Its error does not roll back — the disk writes are committed — but it is
	// captured so an API caller can surface a failed mutateAndPoke.
	if deps.PostProvision != nil {
		result.PostProvisionErr = deps.PostProvision(ProvisionContext{
			RigPath:  rigPath,
			Rig:      resultRig,
			Deferred: deferred,
			Cfg:      nextCfg,
		})
	}

	switch {
	case reAdd:
		emit(ProvisionStep{Name: "done", Detail: "Rig re-initialized."})
	case req.StartSuspended:
		emit(ProvisionStep{Name: "done", Detail: "Rig added (suspended — use 'gc rig resume' to activate)."})
	default:
		emit(ProvisionStep{Name: "done", Detail: "Rig added."})
	}

	return resultRig, result, nil
}

// commitTopology performs the atomic write window (Provision steps 14-16):
// snapshot the canonical files, write city.toml (surgical append for a fresh
// add, full rewrite for a re-add backfill), commit the deferred packs.lock, and
// regenerate routes — all under a panic-safe rollback that restores the snapshot
// if any injected write (or OnStep) panics before the routes write commits. Once
// routes are written the mutations are committed, so a later PostProvision panic
// in the caller must NOT roll them back — which is why the recover lives here and
// PostProvision runs after this returns.
func commitTopology(deps Deps, fs fsys.FS, cityPath, tomlPath string, nextCfg *config.City, reAdd, reAddNeedsConfigWrite bool, commitRigImports func() error, emit func(ProvisionStep)) error {
	snapshots, err := SnapshotTopologyFiles(fs, cityPath, nextCfg)
	if err != nil {
		return fmt.Errorf("snapshot canonical files: %w", err)
	}

	committed := false
	defer func() {
		if r := recover(); r != nil {
			if !committed {
				_ = RestoreSnapshots(fs, snapshots)
			}
			panic(r)
		}
	}()

	if !reAdd || reAddNeedsConfigWrite {
		if deps.NormalizeScopes == nil {
			return depErr("NormalizeScopes")
		}
		if err := deps.NormalizeScopes(cityPath, nextCfg); err != nil {
			return rollbackError(fs, snapshots, "canonicalizing rig topology", err)
		}

		var writeErr error
		if !reAdd {
			// Surgical append: preserve existing comments by appending only the
			// new [[rigs]] block instead of re-serializing the whole file.
			newRig := nextCfg.Rigs[len(nextCfg.Rigs)-1]
			writeErr = config.AppendRigAndWriteSiteBindingsForEdit(fs, tomlPath, nextCfg, newRig)
		} else {
			writeErr = config.WriteCityAndRigSiteBindingsForEdit(fs, tomlPath, nextCfg)
		}
		if writeErr != nil {
			return rollbackError(fs, snapshots, "writing config", writeErr)
		}
	}

	// packs.lock and bundled rig imports commit only after the city config write
	// succeeds, so the lockfile honors the same "city.toml written last" contract:
	// any earlier failure leaves packs.lock untouched, and a failure here rolls
	// back through the snapshot (which now covers packs.lock).
	if commitRigImports != nil {
		if err := commitRigImports(); err != nil {
			return rollbackError(fs, snapshots, "installing bundled rig imports", err)
		}
	}

	if err := deps.WriteRoutes(cityPath, nextCfg); err != nil {
		return rollbackError(fs, snapshots, "writing routes", err)
	}
	committed = true
	emit(ProvisionStep{Name: "routes", Detail: "  Generated routes.jsonl for cross-rig routing"})
	return nil
}

// prepareRigWorkingTree materializes the rig directory from --git-url when
// requested (Provision step 2.5) and detects git / resolves the default branch
// (step 3). Cloning is guarded by a non-nil Deps.CloneGitURL so the CLI local
// path (nil) stays byte-identical; a successful clone leaves rigPath present
// (with .git), so it flows through the existing git-detect and skips the later
// MkdirAll. The clone error is already URL-redacted by git.Clone and req.GitURL
// is never echoed, so no embedded credential leaks into the returned error. It
// returns whether the working tree now exists, whether it is a git repo, and the
// raw override plus resolved default branch.
func prepareRigWorkingTree(deps Deps, req ProvisionRequest, fs fsys.FS, rigPath string, rigPathExists bool) (exists, hasGit bool, defaultBranchOverride, resolvedDefaultBranch string, err error) {
	if deps.CloneGitURL != nil && strings.TrimSpace(req.GitURL) != "" {
		opts := git.CloneOptions{RecurseSubmodules: req.RecurseSubmodules}
		if cErr := deps.CloneGitURL(context.Background(), req.GitURL, rigPath, opts); cErr != nil {
			return rigPathExists, false, "", "", fmt.Errorf("%w: %w", ErrCloneFailed, cErr)
		}
		rigPathExists = true
	}
	_, gitErr := fs.Stat(filepath.Join(rigPath, ".git"))
	hasGit = gitErr == nil
	defaultBranchOverride = strings.TrimSpace(req.DefaultBranch)
	resolvedDefaultBranch = defaultBranchOverride
	if resolvedDefaultBranch == "" && hasGit && deps.ProbeBranch != nil {
		resolvedDefaultBranch = deps.ProbeBranch(rigPath)
	}
	return rigPathExists, hasGit, defaultBranchOverride, resolvedDefaultBranch, nil
}

// resolveResultRig returns the rig to hand back to the caller: the post-write
// config entry when present (a fresh add stores the possibly-empty prefix, not
// the effective one), else a constructed fallback for the unreachable-in-practice
// miss.
func resolveResultRig(nextCfg *config.City, name, rigPath, storedPrefix, resolvedDefaultBranch string, suspended bool) config.Rig {
	for _, rg := range nextCfg.Rigs {
		if rg.Name == name {
			return rg
		}
	}
	return config.Rig{
		Name:          name,
		Path:          rigPath,
		Prefix:        storedPrefix,
		DefaultBranch: resolvedDefaultBranch,
		Suspended:     suspended,
	}
}

// detectReAdd scans cfg for an existing rig named name (Provision step 6). It
// reports whether this is a re-add, whether that re-add still needs a config
// write (an existing entry with an empty path is being materialized now), and
// the index/pointer of the existing entry. A name match at a DIFFERENT path is
// the fatal "already registered" error the CLI prints verbatim.
func detectReAdd(cfg *config.City, name, cityPath, rigPath string) (reAdd, reAddNeedsConfigWrite bool, existingRigIdx int, existingRig *config.Rig, err error) {
	existingRigIdx = -1
	for i, r := range cfg.Rigs {
		if r.Name != name {
			continue
		}
		existingRigIdx = i
		existingRig = &cfg.Rigs[i]
		existPath := r.Path
		if strings.TrimSpace(existPath) == "" {
			return true, true, existingRigIdx, existingRig, nil
		}
		if !filepath.IsAbs(existPath) {
			existPath = filepath.Join(cityPath, existPath)
		}
		if filepath.Clean(existPath) != filepath.Clean(rigPath) {
			return false, false, existingRigIdx, existingRig, fmt.Errorf("rig %q already registered at %s (not %s)", name, r.Path, rigPath)
		}
		return true, false, existingRigIdx, existingRig, nil
	}
	return false, false, existingRigIdx, existingRig, nil
}

// resolveRigPrefix resolves the rig's bead prefix (Provision step 7): the
// existing rig's prefix on a re-add, an explicit --prefix, or the derived
// default. On a fresh add it enforces the HQ and cross-rig prefix-collision
// guards, returning the fatal collision text the CLI prints verbatim.
func resolveRigPrefix(reAdd bool, existingRig *config.Rig, req ProvisionRequest, cfg *config.City, name string) (string, error) {
	var prefix string
	switch {
	case reAdd:
		prefix = existingRig.EffectivePrefix()
	case req.Prefix != "":
		prefix = strings.ToLower(req.Prefix)
	default:
		prefix = config.DeriveBeadsPrefix(name)
	}
	if !reAdd {
		prefixKey := strings.ToLower(prefix)
		if prefixKey == strings.ToLower(config.EffectiveHQPrefix(cfg)) {
			return "", fmt.Errorf("rig %q: prefix %q collides with HQ. Use --prefix to specify a different prefix.", name, prefixKey) //nolint:revive,staticcheck // byte-identical rig-add collision text (trailing period)
		}
		for _, rg := range cfg.Rigs {
			if prefixKey == strings.ToLower(rg.EffectivePrefix()) {
				return "", fmt.Errorf("rig %q: prefix %q collides with %s. Use --prefix to specify a different prefix.", name, prefixKey, rg.Name) //nolint:revive,staticcheck // byte-identical rig-add collision text (trailing period)
			}
		}
	}
	return prefix, nil
}

// buildNextConfig computes the post-add config (Provision step 8). A re-add that
// still needs a write backfills the existing entry's path and default branch; a
// fresh add appends a new [[rigs]] entry, resolving its bundled imports (explicit
// --include, else root-pack/legacy defaults). It returns the possibly-updated
// commit closure and the default imports the banner phase reports, plus whether
// the result still needs rig validation before any filesystem mutation.
func buildNextConfig(
	deps Deps,
	req ProvisionRequest,
	cfg *config.City,
	fs fsys.FS,
	cityPath, name, rigPath, resolvedDefaultBranch string,
	reAdd, reAddNeedsConfigWrite bool,
	existingRigIdx int,
	explicitRigImports []config.BoundImport,
	commitRigImports func() error,
) (nextCfg *config.City, defaultRigImports []config.BoundImport, outCommit func() error, needsValidation bool, err error) {
	nextCfg = cfg
	outCommit = commitRigImports
	needsValidation = !reAdd || reAddNeedsConfigWrite
	if reAddNeedsConfigWrite {
		next := *cfg
		next.Rigs = append([]config.Rig{}, cfg.Rigs...)
		if strings.TrimSpace(next.Rigs[existingRigIdx].Path) == "" {
			next.Rigs[existingRigIdx].Path = rigPath
		}
		if next.Rigs[existingRigIdx].EffectiveDefaultBranch() == "" && resolvedDefaultBranch != "" {
			next.Rigs[existingRigIdx].DefaultBranch = resolvedDefaultBranch
		}
		nextCfg = &next
		return nextCfg, defaultRigImports, outCommit, needsValidation, nil
	}
	if !reAdd {
		storedPrefix := ""
		if req.Prefix != "" {
			storedPrefix = strings.ToLower(req.Prefix)
		}
		addedRig := config.Rig{
			Name:             name,
			Path:             rigPath,
			Prefix:           storedPrefix,
			DefaultBranch:    resolvedDefaultBranch,
			SuspendedOnStart: req.StartSuspended,
		}
		switch {
		case len(explicitRigImports) > 0:
			addedRig.Imports = boundImportsMap(explicitRigImports)
		default:
			rootDefaultRigImports, lErr := config.LoadRootPackDefaultRigImports(fs, cityPath)
			if lErr != nil {
				return nil, nil, nil, false, fmt.Errorf("loading root pack defaults: %w", lErr)
			}
			// Default-rig imports take the same pin/cache hardening as
			// explicit --include imports: a version-less bundled source
			// arriving from root-pack defaults or legacy
			// default_rig_includes must not persist version-less.
			defaultRigImports, outCommit, err = deps.ComposePacks(cityPath, composeDefaultRigImports(rootDefaultRigImports, cfg.Workspace.LegacyDefaultRigIncludes(), cfg.Packs))
			if err != nil {
				return nil, nil, nil, false, fmt.Errorf("installing bundled rig imports: %w", err)
			}
			if len(defaultRigImports) > 0 {
				addedRig.Imports = boundImportsMap(defaultRigImports)
			}
		}
		next := *cfg
		next.Rigs = append(append([]config.Rig{}, cfg.Rigs...), addedRig)
		nextCfg = &next
	}
	return nextCfg, defaultRigImports, outCommit, needsValidation, nil
}

// checkStoreAndPrefixGuards runs the pre-mutation store/prefix guards (Provision
// step 11): --adopt requires an existing store, a prefix mismatch against an
// existing .beads store is fatal with re-add/adopt/fresh-specific guidance, and
// a fresh non-adopt add refuses to clobber a directory that already holds a
// beads store. Each returned error is the exact text the CLI prints.
func checkStoreAndPrefixGuards(fs fsys.FS, req ProvisionRequest, reAdd bool, prefix, name, rigPath string) error {
	if req.Adopt {
		metaPath := filepath.Join(rigPath, ".beads", "metadata.json")
		if _, err := fs.Stat(metaPath); err != nil {
			return fmt.Errorf("--adopt requires .beads/metadata.json in %s", rigPath)
		}
		if _, ok := ReadBeadsPrefix(fs, rigPath); !ok {
			return fmt.Errorf("--adopt requires a valid issue_prefix in .beads/config.yaml in %s", rigPath)
		}
	}

	if existingPrefix, ok := ReadBeadsPrefix(fs, rigPath); ok && existingPrefix != prefix {
		switch {
		case reAdd:
			// On re-add, --prefix is ignored (we use the existing rig's
			// configured prefix). Direct the user to edit city.toml.
			return fmt.Errorf("rig %q has bead prefix %q but city.toml has %q; "+
				"edit city.toml to set prefix = %q, or remove %s/.beads to reinitialize",
				name, existingPrefix, prefix, existingPrefix, rigPath)
		case req.Adopt:
			// On --adopt, the user explicitly wants the existing store.
			// "Remove .beads to reinitialize" is the wrong recovery here:
			// nudge them toward matching the existing prefix instead.
			return fmt.Errorf("--adopt: rig %q already has bead prefix %q (requested %q); "+
				"use --prefix %s (or omit --prefix) to match the existing store",
				name, existingPrefix, prefix, existingPrefix)
		default:
			return fmt.Errorf("rig %q already has bead prefix %q (requested %q); "+
				"use --prefix %s to match, or remove %s/.beads to reinitialize",
				name, existingPrefix, prefix, existingPrefix, rigPath)
		}
	}

	// Guard: on a fresh add (not a re-add) without --adopt, refuse to run
	// if .beads/ already holds a beads store. Without this, provisioning
	// falls through to bd init against an existing Dolt store and typically
	// dies with "bd init: signal: killed" after the probe times out.
	//
	// We treat .beads/ as a store only when metadata.json or config.yaml is
	// present. A directory that happens to be named .beads/ but contains
	// only unrelated content (e.g. the beads project's own .beads/formulas/
	// convention for formula source files) is not a store, so the init path
	// decides how to create the missing store files in place.
	if !reAdd && !req.Adopt {
		beadsPath := filepath.Join(rigPath, ".beads")
		fi, err := fs.Stat(beadsPath)
		if err != nil && !os.IsNotExist(err) {
			return fmt.Errorf("checking %s: %w", beadsPath, err)
		}
		if err == nil && fi.IsDir() {
			containsStore, containsErr := beadsDirContainsStore(fs, beadsPath)
			if containsErr != nil {
				return containsErr
			}
			if containsStore {
				return fmt.Errorf("%s/.beads already contains a beads store; "+
					"use --adopt to register it, or remove %s/.beads to reinitialize",
					rigPath, rigPath)
			}
		}
	}
	return nil
}

// emitProvisionBanners emits the human-facing banner and the re-add "ignored
// flag" warnings (Provision step 12). It is presentation only — every line rides
// emit — so extracting it keeps Provision's control flow readable while
// preserving the exact step names and warning text.
func emitProvisionBanners(
	emit func(ProvisionStep),
	reAdd bool,
	req ProvisionRequest,
	existingRig *config.Rig,
	cfg *config.City,
	includes []string,
	explicitRigImports, defaultRigImports []config.BoundImport,
	hasGit bool,
	rigPath, prefix, resolvedDefaultBranch, defaultBranchOverride, name string,
) {
	if reAdd {
		emit(ProvisionStep{Name: "banner", Detail: fmt.Sprintf("Re-initializing rig '%s'...", name)})
		if req.StartSuspended && req.StartSuspended != existingRig.EffectiveSuspendedOnStart() {
			emit(ProvisionStep{Name: "start-suspended-ignored", Warn: true, Detail: fmt.Sprintf("warning: --start-suspended ignored (existing: suspended_on_start=%v); edit city.toml to change", existingRig.EffectiveSuspendedOnStart())})
		}
		if len(explicitRigImports) > 0 {
			existingRigImports, err := effectiveRigBoundImports(existingRig, cfg.Packs)
			if err != nil {
				emit(ProvisionStep{Name: "include-ignored", Warn: true, Detail: fmt.Sprintf("warning: --include flags %v ignored; existing rig imports could not be normalized (%v). Edit city.toml to change", includes, err)})
			} else if !slices.Equal(existingRigImports, explicitRigImports) {
				emit(ProvisionStep{Name: "include-ignored", Warn: true, Detail: fmt.Sprintf("warning: --include flags %v ignored (existing imports: %s); edit city.toml to change", includes, formatBoundImports(existingRigImports))})
			}
		}
		if req.Prefix != "" && strings.ToLower(req.Prefix) != existingRig.EffectivePrefix() {
			emit(ProvisionStep{Name: "prefix-ignored", Warn: true, Detail: fmt.Sprintf("warning: --prefix=%s ignored (existing: %s); edit city.toml to change", req.Prefix, existingRig.EffectivePrefix())})
		}
		if defaultBranchOverride != "" &&
			defaultBranchOverride != existingRig.EffectiveDefaultBranch() &&
			(existingRig.EffectiveDefaultBranch() != "" || resolvedDefaultBranch != defaultBranchOverride) {
			emit(ProvisionStep{Name: "default-branch-ignored", Warn: true, Detail: fmt.Sprintf("warning: --default-branch=%s ignored (existing: %s); edit city.toml to change", defaultBranchOverride, existingRig.EffectiveDefaultBranch())})
		}
	} else {
		emit(ProvisionStep{Name: "banner", Detail: fmt.Sprintf("Adding rig '%s'...", name)})
	}
	if hasGit {
		emit(ProvisionStep{Name: "git-detected", Detail: fmt.Sprintf("  Detected git repo at %s", rigPath)})
	}
	emit(ProvisionStep{Name: "prefix", Detail: fmt.Sprintf("  Prefix: %s", prefix)})
	if !reAdd && resolvedDefaultBranch != "" {
		emit(ProvisionStep{Name: "default-branch", Detail: fmt.Sprintf("  Default branch: %s", resolvedDefaultBranch)})
	}
	if !reAdd {
		switch {
		case len(explicitRigImports) > 0:
			emit(ProvisionStep{Name: "imports", Detail: fmt.Sprintf("  Import: %s", formatBoundImports(explicitRigImports))})
		default:
			if len(defaultRigImports) > 0 {
				emit(ProvisionStep{Name: "imports", Detail: fmt.Sprintf("  Import: %s (default)", formatBoundImports(defaultRigImports))})
			}
		}
	}
}

// initBeadsStore initializes (or adopts) the rig's beads store and emits the
// matching progress line (Provision step 13). It returns whether store creation
// was deferred to the controller (the DoltLite fresh-add path), which the caller
// records on the result and threads into PostProvision.
func initBeadsStore(deps Deps, req ProvisionRequest, emit func(ProvisionStep), cityPath, rigPath, prefix string, storeContract, doltSkip func() bool) (bool, error) {
	deferred := false
	var err error
	if req.Adopt {
		if deps.PrepareAdopt != nil {
			if err := deps.PrepareAdopt(cityPath, rigPath); err != nil {
				return false, fmt.Errorf("prepare adopted rig store: %w", err)
			}
		}
		if storeContract() {
			deferred, err = deps.InitStore(cityPath, rigPath, prefix)
			if err != nil {
				return false, err
			}
		}
		emit(ProvisionStep{Name: "beads-init", Detail: "  Adopted existing beads database"})
		return deferred, nil
	}
	deferred, err = deps.InitStore(cityPath, rigPath, prefix)
	if err != nil {
		return false, err
	}
	if deferred {
		if storeContract() && doltSkip() {
			emit(ProvisionStep{Name: "beads-init", Detail: "  Beads init deferred to controller"})
		} else if err := deps.InitAndHook(cityPath, rigPath, prefix); err != nil {
			emit(ProvisionStep{Name: "beads-init", Detail: "  Beads init deferred to controller"})
		} else {
			emit(ProvisionStep{Name: "beads-init", Detail: "  Initialized beads database"})
		}
	} else {
		emit(ProvisionStep{Name: "beads-init", Detail: "  Initialized beads database"})
	}
	return deferred, nil
}

// StatRigPath is the rig-add path preflight. It reports whether rigPath already
// exists as a directory, or returns the fatal error the CLI prints — the
// --adopt-missing, stat-error, and not-a-directory cases. The CLI runs this
// before it loads city.toml so a bad rig path is reported ahead of a
// config-load failure, matching the original doRigAddWithResult ordering;
// Provision calls it as step 2 so the API path enforces the same guard.
func StatRigPath(fs fsys.FS, rigPath string, adopt bool) (exists bool, err error) {
	fi, statErr := fs.Stat(rigPath)
	if statErr != nil {
		if adopt {
			return false, fmt.Errorf("--adopt requires an existing directory: %s", rigPath)
		}
		if !os.IsNotExist(statErr) {
			return false, fmt.Errorf("checking %s: %w", rigPath, statErr)
		}
		return false, nil
	}
	if !fi.IsDir() {
		return false, fmt.Errorf("%s is not a directory", rigPath)
	}
	return true, nil
}

// rollbackError restores the topology snapshot and returns the fatal error the
// caller prints. It mirrors the CLI's writeRigAddRollbackError: on a failed
// restore it appends "(rollback failed: ...)" so the operator sees both faults.
func rollbackError(fs fsys.FS, snapshots []FileSnapshot, action string, cause error) error {
	if restoreErr := RestoreSnapshots(fs, snapshots); restoreErr != nil {
		return fmt.Errorf("%s: %w (rollback failed: %w)", action, cause, restoreErr)
	}
	return fmt.Errorf("%s: %w", action, cause)
}
