package main

import (
	"errors"
	"fmt"
	"io"
	"sort"
	"strings"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/coordclass"
)

// This file is E3 of the domain/infra store split: the in-place migration of an
// EXISTING comingled single-Dolt-db city to the two-store layout E2 gives a
// freshly-initialized city. It is an opt-in, owner-gated, stop-the-world command
// (`gc migrate infra-store`), never an auto-migration at gc start.
//
// North star (E3-MIGRATION-DESIGN.md §"North Star"): after migration the city's
// data is indistinguishable from a city born split under E2 — infra-class beads
// live in the infra store (keeping their HQ/rig-era ids — never re-minted), work
// beads are untouched, and cross-boundary dependency edges sit co-resident with
// their SOURCE bead (dangling across the boundary, resolved by the E1.6 Go-side
// seams). Because the CLI-sling split-brain created rig-prefixed molecules in rig
// stores too, the migration sweeps EVERY domain store (city + all rigs), not just
// the HQ store.
//
// The single authority for "which side does this bead belong on" is
// coordclass.Classify — never a type list — so the migration boundary can never
// drift from the router the rest of the system uses.

// migrateLedger is the accounting emitted by a migration run (as --json or a
// human summary). It is the reconciliation proof: work_after == work_before −
// moved and infra_after == infra_before + moved must hold.
type migrateLedger struct {
	DryRun bool `json:"dry_run"`
	// Moved is the number of infra-class beads copied into the infra store this
	// run (0 on a fully-converged re-run).
	Moved int `json:"moved"`
	// AlreadyPresent is the number of infra-class beads found already present in
	// the infra store at plan time (a crash-resume or re-run case): copy skipped,
	// edges reasserted idempotently, delete still performed.
	AlreadyPresent int `json:"already_present"`
	// Deleted is the number of moved beads removed from the domain stores after
	// verification (0 in --dry-run).
	Deleted int `json:"deleted"`
	// EdgesAdded is the number of dependency edges (re)asserted on the infra store.
	EdgesAdded int `json:"edges_added"`
	// InfraBefore/InfraAfter and per-store work counts are the reconciliation
	// figures. Work counts are keyed by store ref ("city" or a rig name).
	InfraBefore int            `json:"infra_before"`
	InfraAfter  int            `json:"infra_after"`
	WorkBefore  map[string]int `json:"work_before"`
	WorkAfter   map[string]int `json:"work_after,omitempty"`
	// CrossBoundaryBlockingEdges inventories work→infra blocking edges that become
	// dangling after the move (risk #2): they stop blocking in bd ready. Recorded
	// for operator visibility, not acted on.
	CrossBoundaryBlockingEdges []string `json:"cross_boundary_blocking_edges,omitempty"`
	// Stores lists the domain store refs the migration swept.
	Stores []string `json:"stores"`
}

// migrateStore pairs a domain store with the ref label used in the ledger
// (the HQ/city store is "city"; each rig is its config name).
type migrateStore struct {
	ref   string
	store beads.Store
}

// moveEntry is one infra-class bead found in a domain store, tagged with the
// index of the domain store it came from (into the ordered domainStores slice).
type moveEntry struct {
	bead       beads.Bead
	storeIndex int
}

// cityNeedsInfraStoreMigration reports whether cityPath is an existing single-db
// (or partially-migrated) city that the two-store migration can and should run
// on. It reads config shape and LIVE state, never a marker file:
//
//   - can-migrate: the city uses the bd/Dolt store contract AND is not backed by
//     an external/hosted Dolt (tenancy: one db per project — refuse external).
//   - needs: either the infra scope does not exist yet (!cityHasInfraStore), or a
//     domain store still holds an infra-class bead (the crash-resume / re-run
//     case where the scope exists but the move did not finish).
//
// A city that cannot migrate returns false. The live-state arm only runs when the
// scope already exists, so a not-yet-split city (the common case) is detected by
// the cheap !cityHasInfraStore check without opening any store. The
// domain-holds-infra probe is best-effort: an open/list error is treated as "no
// evidence of remaining infra beads" so a transient failure never wedges the
// detector into perpetually reporting "needs migration".
func cityNeedsInfraStoreMigration(cityPath string) bool {
	if strings.TrimSpace(cityPath) == "" {
		return false
	}
	if !cityUsesBdStoreContract(cityPath) || isExternalDolt(cityPath) {
		return false
	}
	if !cityHasInfraStore(cityPath) {
		return true
	}
	// The scope exists: only "needs" if a domain store still holds infra beads.
	return domainStoresHoldInfraBeads(cityPath)
}

// domainStoresHoldInfraBeads reports whether any of the city's domain stores
// still holds an infrastructure-class bead — the resumable/re-run signal. It is
// best-effort: any open/list failure yields false (no evidence), so a transient
// error never falsely reports remaining work.
func domainStoresHoldInfraBeads(cityPath string) bool {
	cfg, err := loadCityConfig(cityPath, io.Discard)
	if err != nil {
		return false
	}
	resolveRigPaths(cityPath, cfg.Rigs)
	stores, closeStores, err := openDomainStoresForMigration(cityPath, cfg)
	if err != nil {
		return false
	}
	defer closeStores()
	for _, ds := range stores {
		list, err := ds.store.List(beads.ListQuery{IncludeClosed: true, TierMode: beads.TierBoth, AllowScan: true})
		if err != nil {
			continue
		}
		for _, b := range list {
			if coordclass.Classify(b).IsInfrastructure() {
				return true
			}
		}
	}
	return false
}

// openDomainStoresForMigration opens the city (HQ) store plus every configured
// rig store through the production open path, returning them in a deterministic
// order (city first, then rigs in cfg order) alongside a closer that releases
// every handle the function opened. It never returns the infra store.
func openDomainStoresForMigration(cityPath string, cfg *config.City) ([]migrateStore, func(), error) {
	var opened []beads.Store
	closeAll := func() {
		for _, s := range opened {
			_ = closeBeadStoreHandle(s)
		}
	}

	cityStore, err := openCityStoreAt(cityPath)
	if err != nil {
		closeAll()
		return nil, nil, fmt.Errorf("opening city work store: %w", err)
	}
	opened = append(opened, cityStore)
	stores := []migrateStore{{ref: "city", store: cityStore}}

	if cfg != nil {
		for i := range cfg.Rigs {
			path := strings.TrimSpace(cfg.Rigs[i].Path)
			if path == "" {
				continue
			}
			rigStore, err := openStoreAtForCity(path, cityPath)
			if err != nil {
				closeAll()
				return nil, nil, fmt.Errorf("opening rig %q work store: %w", cfg.Rigs[i].Name, err)
			}
			opened = append(opened, rigStore)
			stores = append(stores, migrateStore{ref: cfg.Rigs[i].Name, store: rigStore})
		}
	}
	return stores, closeAll, nil
}

// doMigrateInfraStore runs the in-place domain/infra store migration for cityPath.
// It is idempotent, resumable, and crash-safe (§4): it recomputes the entire plan
// from live store state on every run, writes no status file, and orders its work
// GLOBALLY (all copies, then all edges, then verify, then all deletes) so no edge
// ever precedes its endpoints and no delete ever precedes verification. A re-run
// on a fully-migrated city is a convergent no-op (moved == 0).
//
// Preflight (§1): it refuses to run while a controller is alive (stop the city
// first — dual-store writes during migration are unsafe), refuses external/hosted
// Dolt, and brings the managed Dolt server up + waits for readiness so the infra
// scope's database can be created and written.
func doMigrateInfraStore(cityPath string, dryRun bool, stderr io.Writer) (*migrateLedger, error) {
	if stderr == nil {
		stderr = io.Discard
	}
	cityPath = strings.TrimSpace(cityPath)
	if cityPath == "" {
		return nil, errors.New("migrate infra-store: no city path")
	}
	if !cityUsesBdStoreContract(cityPath) {
		return nil, errors.New("migrate infra-store: city does not use the bd/Dolt store contract; " +
			"the infra store split applies only to bd-backed cities")
	}
	if isExternalDolt(cityPath) {
		return nil, errors.New("migrate infra-store: city is backed by an external/hosted Dolt endpoint; " +
			"the two-store split is not supported for hosted Dolt (one database per project)")
	}
	if pid := controllerAlive(cityPath); pid != 0 {
		return nil, fmt.Errorf("migrate infra-store: a controller is running (pid %d); "+
			"stop the city first (gc stop) — migrating while the city is live is unsafe", pid)
	}

	// Bring managed Dolt up and wait for readiness so the infra scope's database
	// can be created and both stores opened. Mirrors initDirIfReadyManagedDolt.
	if err := initDirIfReadyEnsureBeadsProvider(cityPath); err != nil {
		return nil, fmt.Errorf("migrate infra-store: starting bead store provider: %w", err)
	}
	if err := initDirIfReadyWaitForManagedDolt(cityPath, managedDoltInitReadyTimeout); err != nil {
		return nil, fmt.Errorf("migrate infra-store: waiting for managed Dolt: %w", err)
	}

	cfg, err := loadCityConfig(cityPath, io.Discard)
	if err != nil {
		return nil, fmt.Errorf("migrate infra-store: loading city config: %w", err)
	}
	resolveRigPaths(cityPath, cfg.Rigs)

	// §2 create the infra scope (idempotent) and write routes so a same-prefix
	// cross-boundary DepAdd (gcy-cv1 → gcy-45) can resolve the read-only target in
	// the HQ db. Both steps are the exact E2.5 calls plus the new routes step.
	if !dryRun {
		fmt.Fprintln(stderr, "migrate infra-store: creating infra scope and routes...") //nolint:errcheck // best-effort progress
		if err := ensureInfraScopeForMigration(cityPath, cfg); err != nil {
			return nil, err
		}
	} else {
		fmt.Fprintln(stderr, "migrate infra-store: dry run (no writes)") //nolint:errcheck // best-effort progress
	}

	// Open every domain store plus the infra store.
	domainStores, closeDomain, err := openDomainStoresForMigration(cityPath, cfg)
	if err != nil {
		return nil, fmt.Errorf("migrate infra-store: %w", err)
	}
	defer closeDomain()

	var infraStore beads.Store
	if dryRun {
		// A dry run must make no writes and may run before the infra scope exists,
		// so open the infra store only if it is already present; otherwise leave it
		// nil and report the plan against an empty infra store.
		if s, present, err := openCityInfraStoreAt(cityPath); err != nil {
			return nil, fmt.Errorf("migrate infra-store: opening infra store: %w", err)
		} else if present {
			infraStore = s
			defer func() { _ = closeBeadStoreHandle(infraStore) }()
		}
	} else {
		s, present, err := openCityInfraStoreAt(cityPath)
		if err != nil {
			return nil, fmt.Errorf("migrate infra-store: opening infra store: %w", err)
		}
		if !present || s == nil {
			return nil, errors.New("migrate infra-store: infra scope created but the infra store did not open")
		}
		infraStore = s
		defer func() { _ = closeBeadStoreHandle(infraStore) }()
	}

	return runInfraStoreMigration(domainStores, infraStore, dryRun)
}

// ensureInfraScopeForMigration performs §2: seed the infra scope config
// (seedInitInfraScope), bd-init its database (initAndHookDir), and write the
// infra scope's routes.jsonl so cross-boundary dependency targets resolve. All
// three are idempotent, so a resume/re-run re-applies them harmlessly.
func ensureInfraScopeForMigration(cityPath string, cfg *config.City) error {
	if err := seedInitInfraScope(cityPath); err != nil {
		return fmt.Errorf("migrate infra-store: seeding infra scope: %w", err)
	}
	if !cityHasInfraStore(cityPath) {
		return errors.New("migrate infra-store: infra scope seed did not activate the split " +
			"(cityHasInfraStore false); is this a bd-backed managed-Dolt city?")
	}
	if err := initAndHookDir(cityPath, infraScopeRoot(cityPath), config.InfraScopePrefix); err != nil {
		return fmt.Errorf("migrate infra-store: initializing infra store database: %w", err)
	}
	if err := writeInfraScopeRoutes(cityPath, cfg); err != nil {
		return fmt.Errorf("migrate infra-store: writing infra scope routes: %w", err)
	}
	return nil
}

// writeInfraScopeRoutes writes routes.jsonl for every scope after a migration so
// bd's prefix routing resolves cross-boundary dependency targets read-only in the
// store they live in, in BOTH directions: the infra scope learns the domain
// prefixes (a same-prefix cross-boundary edge gcy-cv1 → gcy-45, both HQ-prefixed
// but now in different stores, needs the infra scope to reach the HQ db), and the
// domain scopes learn "gcg" → .gc/infra. collectRigRoutes already includes the
// infra scope on a split city (cityHasInfraStore is true by the time migration
// reaches here), and writeAllRoutes writes a per-scope file mapping every entry,
// so this is the same bidirectional route set a fresh two-store `gc start` emits.
func writeInfraScopeRoutes(cityPath string, cfg *config.City) error {
	rigs := collectRigRoutes(cityPath, cfg)
	hasInfra := false
	for _, r := range rigs {
		if r.Prefix == config.InfraScopePrefix {
			hasInfra = true
			break
		}
	}
	if !hasInfra {
		return fmt.Errorf("migrate infra-store: infra scope missing from route set (cityHasInfraStore false?)")
	}
	if err := writeAllRoutes(rigs); err != nil {
		return fmt.Errorf("writing infra + domain scope routes.jsonl: %w", err)
	}
	return nil
}

// runInfraStoreMigration executes the global-phase-ordered plan against already-
// opened stores. It is separated from doMigrateInfraStore so the fast-tier test
// can drive it directly with MemStore-backed stores (no Dolt, no controller, no
// scope creation), while the integration test drives the full doMigrateInfraStore
// entry point.
//
// Phase ordering is GLOBAL (§4): every copy across every domain store, THEN every
// edge, THEN verify, THEN every delete. This guarantees an edge is never asserted
// before both endpoints exist and a delete never precedes verification, so a crash
// in any phase leaves only re-runnable states.
func runInfraStoreMigration(domainStores []migrateStore, infraStore beads.Store, dryRun bool) (*migrateLedger, error) {
	ledger := &migrateLedger{
		DryRun:     dryRun,
		WorkBefore: map[string]int{},
	}
	for _, ds := range domainStores {
		ledger.Stores = append(ledger.Stores, ds.ref)
	}

	// Snapshot the infra store's current contents = the idempotency oracle. A bead
	// already present here was copied on a prior (crashed) run: skip its copy,
	// reassert its edges idempotently, and still delete it from the domain store.
	infraByID := map[string]beads.Bead{}
	if infraStore != nil {
		list, err := infraStore.List(beads.ListQuery{IncludeClosed: true, TierMode: beads.TierBoth, AllowScan: true})
		if err != nil {
			return nil, fmt.Errorf("migrate infra-store: listing infra store: %w", err)
		}
		for _, b := range list {
			infraByID[b.ID] = b
		}
	}
	ledger.InfraBefore = len(infraByID)

	// Build the move plan from live state: enumerate every domain store, classify
	// each bead, and partition the infra-class beads into copy (work-only) vs
	// skip-copy (already in infra, crash-resume). moveSet is the full set of
	// infra-class beads found in domain stores, keyed by source store index.
	var toCopy []moveEntry  // infra-class, not yet in infra store
	var moveSet []moveEntry // all infra-class beads found in domain stores (copy + already-present)

	for si, ds := range domainStores {
		list, err := ds.store.List(beads.ListQuery{IncludeClosed: true, TierMode: beads.TierBoth, AllowScan: true})
		if err != nil {
			return nil, fmt.Errorf("migrate infra-store: listing domain store %q: %w", ds.ref, err)
		}
		ledger.WorkBefore[ds.ref] = len(list)
		for _, b := range list {
			if !coordclass.Classify(b).IsInfrastructure() {
				continue
			}
			moveSet = append(moveSet, moveEntry{bead: b, storeIndex: si})
			if _, present := infraByID[b.ID]; present {
				ledger.AlreadyPresent++
				continue
			}
			toCopy = append(toCopy, moveEntry{bead: b, storeIndex: si})
		}
	}

	// Deterministic order for reproducible ledgers and stable dry-run output.
	sort.SliceStable(toCopy, func(i, j int) bool { return toCopy[i].bead.ID < toCopy[j].bead.ID })
	sort.SliceStable(moveSet, func(i, j int) bool { return moveSet[i].bead.ID < moveSet[j].bead.ID })

	if dryRun {
		// Zero writes: report the plan only. Work-after/infra-after are projected.
		ledger.Moved = len(toCopy)
		ledger.InfraAfter = ledger.InfraBefore + len(toCopy)
		ledger.WorkAfter = map[string]int{}
		movedPerStore := map[int]int{}
		for _, e := range moveSet {
			movedPerStore[e.storeIndex]++
		}
		for si, ds := range domainStores {
			ledger.WorkAfter[ds.ref] = ledger.WorkBefore[ds.ref] - movedPerStore[si]
		}
		ledger.CrossBoundaryBlockingEdges = inventoryCrossBoundaryBlockingEdges(domainStores, moveSet2ids(moveSet))
		return ledger, nil
	}

	if infraStore == nil {
		return nil, errors.New("migrate infra-store: no infra store to migrate into")
	}

	// The id-set of every bead that is (or will be) in the infra store — used to
	// tell an intra-set edge (both endpoints local to infra) from a cross-boundary
	// edge (target read-only in a domain store via routes).
	infraSet := map[string]struct{}{}
	for id := range infraByID {
		infraSet[id] = struct{}{}
	}
	for _, e := range moveSet {
		infraSet[e.bead.ID] = struct{}{}
	}

	// ── Phase M1: copy every not-yet-present infra bead into the infra store ──
	for _, e := range toCopy {
		if err := copyBeadPreservingID(infraStore, e.bead); err != nil {
			return nil, fmt.Errorf("migrate infra-store: copying %q into infra store: %w", e.bead.ID, err)
		}
		ledger.Moved++
	}

	// ── Phase M2: (re)assert every moved bead's OUTgoing dependency edges ──
	// Edges are co-resident with their SOURCE, so we add the outbound edges of each
	// moved (now-infra) bead on the infra store. Reads come from the SOURCE domain
	// store. Skip a pair already present in infra (dependencies PK is (issue,
	// depends_on)) and skip dotted parent-child implied by id nesting. A target
	// that is another moved/infra bead is local; a target still in a domain store
	// is cross-boundary and resolves read-only via routes (no FK on depends_on_id,
	// so a dangling target is fine).
	existingInfraDeps, err := loadInfraStoreDeps(infraStore, moveSet2ids(moveSet))
	if err != nil {
		return nil, fmt.Errorf("migrate infra-store: reading existing infra deps: %w", err)
	}
	for _, e := range moveSet {
		deps, err := outgoingDepsForBead(domainStores[e.storeIndex].store, e.bead)
		if err != nil {
			return nil, fmt.Errorf("migrate infra-store: reading deps for %q: %w", e.bead.ID, err)
		}
		for _, d := range deps {
			if d.IssueID != e.bead.ID {
				continue // only outbound edges belong with this source bead
			}
			if depAlreadyPresent(existingInfraDeps[d.IssueID], d) {
				continue
			}
			if isDottedParentChild(d.IssueID, d.DependsOnID) {
				continue
			}
			if err := infraStore.DepAdd(d.IssueID, d.DependsOnID, d.Type); err != nil {
				return nil, fmt.Errorf("migrate infra-store: adding dep %s→%s (%s): %w",
					d.IssueID, d.DependsOnID, d.Type, err)
			}
			existingInfraDeps[d.IssueID] = append(existingInfraDeps[d.IssueID], d)
			ledger.EdgesAdded++
		}
	}

	// ── Phase M3: verify BEFORE any delete. The infra store must now hold every
	// moved id with a matching projection, and every moved bead must be
	// classified as infrastructure (it always is by construction, but re-check to
	// gate deletion on proven state).
	if err := verifyMovedBeadsPresent(infraStore, moveSet2entriesBeads(moveSet)); err != nil {
		return nil, fmt.Errorf("migrate infra-store: verification failed (no beads deleted): %w", err)
	}

	// ── Phase M4: delete moved beads from their domain stores, but ONLY those
	// proven present in the infra store. Use the orphan-preserving BATCH delete so
	// inbound edges from staying work beads survive as dangling rows (never
	// stripped/text-rewritten). Never single-id-Delete a multi-bead move set.
	movedIDsByStore := map[int][]string{}
	for _, e := range moveSet {
		if _, present := infraByID[e.bead.ID]; present {
			// Already in infra before this run (crash-resume): delete-safe.
			movedIDsByStore[e.storeIndex] = append(movedIDsByStore[e.storeIndex], e.bead.ID)
			continue
		}
		// Freshly copied this run: it passed M3 verification, so it is safe to delete.
		movedIDsByStore[e.storeIndex] = append(movedIDsByStore[e.storeIndex], e.bead.ID)
	}
	for si, ids := range movedIDsByStore {
		ids = dedupeStrings(ids)
		if len(ids) == 0 {
			continue
		}
		deleted, err := deleteFromDomainStoreOrphaning(domainStores[si].store, ids)
		if err != nil {
			return nil, fmt.Errorf("migrate infra-store: deleting moved beads from %q: %w",
				domainStores[si].ref, err)
		}
		ledger.Deleted += deleted
	}

	// Re-verify AFTER delete (§5): every domain store holds no infra bead, the
	// infra store holds no domain bead, and the counts reconcile.
	if err := verifyStoreClassBoundaryAfterMigration(domainStores, infraStore); err != nil {
		return nil, fmt.Errorf("migrate infra-store: post-delete boundary verification failed: %w", err)
	}

	// Final reconciliation figures.
	ledger.WorkAfter = map[string]int{}
	for _, ds := range domainStores {
		list, err := ds.store.List(beads.ListQuery{IncludeClosed: true, TierMode: beads.TierBoth, AllowScan: true})
		if err != nil {
			return nil, fmt.Errorf("migrate infra-store: counting domain store %q: %w", ds.ref, err)
		}
		ledger.WorkAfter[ds.ref] = len(list)
	}
	infraAfter, err := infraStore.List(beads.ListQuery{IncludeClosed: true, TierMode: beads.TierBoth, AllowScan: true})
	if err != nil {
		return nil, fmt.Errorf("migrate infra-store: counting infra store: %w", err)
	}
	ledger.InfraAfter = len(infraAfter)
	ledger.CrossBoundaryBlockingEdges = inventoryCrossBoundaryBlockingEdges(domainStores, moveSet2ids(moveSet))

	return ledger, nil
}

// copyBeadPreservingID copies src into dst KEEPING its id (never re-minting), so
// legacy infra beads keep their HQ/rig-era prefix. §3.2: it clears the create-time
// relationship fields (Needs, ParentID, Dependencies) so the Create only creates
// the node — edges are restored separately in Phase M2, where cross-boundary
// targets resolve via routes. The infra store's policy wrapper only mints a
// reserved-prefix id when b.ID is empty, so a non-empty id passes through
// verbatim. After the node create, status is restored: a closed bead is closed on
// dst (Close reads metadata.close_reason → --reason), any other non-open status is
// set via Update.
//
// Accepted, documented losses (§3.2): created_at/updated_at are re-stamped by dst;
// bd events/comments are not carried; is_blocked is recomputed by the store.
func copyBeadPreservingID(dst beads.Store, src beads.Bead) error {
	node := src
	node.Needs = nil
	node.ParentID = ""
	node.Dependencies = nil
	// Preserve the tier: an ephemeral (wisp) source must land in the wisp tier so
	// its classification and TTL behavior are unchanged.
	//
	// The infra store's Dolt database has its own reserved prefix (gcg), so bd
	// rejects a create whose --id carries a foreign HQ/rig-era prefix unless
	// forced. Prefer the ForeignIDCreator capability, which adds --force, so the
	// legacy id is kept verbatim; fall back to plain Create for stores without
	// prefix rules (a fresh in-sandbox MemStore where the capability is present
	// anyway, or any store that honors the id directly).
	created, err := createPreservingForeignID(dst, node)
	if err != nil {
		return fmt.Errorf("creating node: %w", err)
	}
	if created.ID != src.ID {
		return fmt.Errorf("infra store re-minted id %q → %q (stable ids must be preserved)", src.ID, created.ID)
	}
	switch src.Status {
	case "", "open":
		return nil
	case "closed":
		// Close reads metadata.close_reason off the just-created bead and forwards
		// it as --reason; the reason traveled with the copied metadata.
		if err := dst.Close(src.ID); err != nil {
			return fmt.Errorf("restoring closed status: %w", err)
		}
	default:
		status := src.Status
		if err := dst.Update(src.ID, beads.UpdateOpts{Status: &status}); err != nil {
			return fmt.Errorf("restoring status %q: %w", src.Status, err)
		}
	}
	return nil
}

// createPreservingForeignID creates b in dst while keeping its exact id, using
// the ForeignIDCreator (forced) capability when the store exposes it so a legacy
// HQ/rig-prefixed id is accepted by the infra store's differently-prefixed Dolt
// database. A store without the capability falls back to plain Create (which the
// id-honoring MemStore and any prefix-free store accept directly).
func createPreservingForeignID(dst beads.Store, b beads.Bead) (beads.Bead, error) {
	if creator, ok := dst.(beads.ForeignIDCreator); ok {
		return creator.CreateWithForeignID(b)
	}
	return dst.Create(b)
}

// outgoingDepsForBead returns the outbound ("down") dependency edges of bead b
// from its source store. It prefers the batch capability (DepListBatch) for a
// single subprocess call, falling back to DepList. The returned edges have
// IssueID == b.ID.
func outgoingDepsForBead(store beads.Store, b beads.Bead) ([]beads.Dep, error) {
	if batch, ok := store.(interface {
		DepListBatch(ids []string) (map[string][]beads.Dep, error)
	}); ok {
		m, err := batch.DepListBatch([]string{b.ID})
		if err != nil {
			return nil, err
		}
		return m[b.ID], nil
	}
	return store.DepList(b.ID, "down")
}

// loadInfraStoreDeps returns the existing outbound dependency edges of ids as they
// stand in the infra store, so Phase M2 can skip edges already present (the
// dependencies PK is (issue, depends_on)). It prefers DepListBatch.
func loadInfraStoreDeps(infraStore beads.Store, ids []string) (map[string][]beads.Dep, error) {
	if len(ids) == 0 {
		return map[string][]beads.Dep{}, nil
	}
	if batch, ok := infraStore.(interface {
		DepListBatch(ids []string) (map[string][]beads.Dep, error)
	}); ok {
		m, err := batch.DepListBatch(ids)
		if err != nil {
			return nil, err
		}
		if m == nil {
			m = map[string][]beads.Dep{}
		}
		return m, nil
	}
	out := map[string][]beads.Dep{}
	for _, id := range ids {
		deps, err := infraStore.DepList(id, "down")
		if err != nil {
			return nil, err
		}
		out[id] = deps
	}
	return out, nil
}

// depAlreadyPresent reports whether edge d (matched on issue+depends_on+type) is
// already in the list of existing edges for its issue.
func depAlreadyPresent(existing []beads.Dep, d beads.Dep) bool {
	for _, e := range existing {
		if e.IssueID == d.IssueID && e.DependsOnID == d.DependsOnID && e.Type == d.Type {
			return true
		}
	}
	return false
}

// isDottedParentChild reports whether depends_on is the dotted-id parent of issue
// (issue == parent + "."). Such parent-child links are implied by the id nesting
// and are not re-added as explicit edges (matching the design's skip rule).
func isDottedParentChild(issueID, dependsOnID string) bool {
	return strings.HasPrefix(issueID, dependsOnID+".")
}

// deleteFromDomainStoreOrphaning removes ids from a domain store via the
// orphan-preserving batch delete (BatchDeleter.DeleteAllOrphaning). On bd that is
// a raw set-based SQL DELETE, NOT `bd delete` — `bd delete` (single OR batch)
// text-rewrites every connected STAYING bead's free-text to "[deleted:ID]", which
// would corrupt work beads that reference a moved infra bead's id. It NEVER falls
// back to the plain Store.Delete for a multi-bead set (Store.Delete routes through
// `bd delete`, the mutation bomb this primitive exists to avoid). A store lacking
// BatchDeleter is an error for a multi-bead set; a genuine single-id set on such a
// store is delegated to Store.Delete as the only available path.
func deleteFromDomainStoreOrphaning(store beads.Store, ids []string) (int, error) {
	deleter, ok := store.(beads.BatchDeleter)
	if !ok {
		if len(ids) > 1 {
			return 0, fmt.Errorf("domain store %T does not support orphan-preserving batch delete; "+
				"refusing to single-id-delete a %d-bead move set (would text-rewrite staying neighbors)", store, len(ids))
		}
		if err := store.Delete(ids[0]); err != nil {
			return 0, err
		}
		return 1, nil
	}
	return deleter.DeleteAllOrphaning(ids)
}

// verifyMovedBeadsPresent gates deletion (§5): every moved id must Get from the
// infra store with matching Type, Status, Labels, and metadata superset, and must
// classify as infrastructure. It is the "only delete what is proven copied"
// guarantee.
func verifyMovedBeadsPresent(infraStore beads.Store, moved []beads.Bead) error {
	for _, src := range moved {
		got, err := infraStore.Get(src.ID)
		if err != nil {
			return fmt.Errorf("moved bead %q not found in infra store: %w", src.ID, err)
		}
		if got.Type != src.Type {
			return fmt.Errorf("moved bead %q type mismatch: infra=%q source=%q", src.ID, got.Type, src.Type)
		}
		if !coordclass.Classify(got).IsInfrastructure() {
			return fmt.Errorf("moved bead %q classifies as work in the infra store (type=%q labels=%v)",
				src.ID, got.Type, got.Labels)
		}
		if !sameStatus(got.Status, src.Status) {
			return fmt.Errorf("moved bead %q status mismatch: infra=%q source=%q", src.ID, got.Status, src.Status)
		}
		if !labelsSubset(src.Labels, got.Labels) {
			return fmt.Errorf("moved bead %q labels not preserved: infra=%v source=%v", src.ID, got.Labels, src.Labels)
		}
		if !metadataSuperset(got.Metadata, src.Metadata) {
			return fmt.Errorf("moved bead %q metadata not preserved: infra=%v source=%v", src.ID, got.Metadata, src.Metadata)
		}
	}
	return nil
}

// verifyStoreClassBoundaryAfterMigration is the §5 post-delete invariant: every
// domain store holds no infrastructure-class bead, and the infra store holds no
// work-class bead. It uses the same coordclass.Classify authority the boundary
// invariant test uses.
func verifyStoreClassBoundaryAfterMigration(domainStores []migrateStore, infraStore beads.Store) error {
	for _, ds := range domainStores {
		if err := verifyStoreClassBoundary(ds.store, false); err != nil {
			return fmt.Errorf("domain store %q: %w", ds.ref, err)
		}
	}
	return verifyStoreClassBoundary(infraStore, true)
}

// verifyStoreClassBoundary lists every bead in store and returns an error if any
// bead sits on the wrong side of the coordination boundary. wantInfra=false
// asserts a domain store (every bead ClassWork); wantInfra=true asserts the infra
// store (every bead an infrastructure class). This is the non-test twin of
// assertStoreClassBoundary in infra_store_boundary_invariant_test.go, sharing the
// exact classify-every-bead logic.
func verifyStoreClassBoundary(store beads.Store, wantInfra bool) error {
	list, err := store.List(beads.ListQuery{IncludeClosed: true, TierMode: beads.TierBoth, AllowScan: true})
	if err != nil {
		return fmt.Errorf("listing store: %w", err)
	}
	for _, b := range list {
		if coordclass.Classify(b).IsInfrastructure() != wantInfra {
			side := "domain"
			if wantInfra {
				side = "infra"
			}
			return fmt.Errorf("%s store holds wrong-side bead: id=%q type=%q labels=%v class=%s",
				side, b.ID, b.Type, b.Labels, coordclass.Classify(b))
		}
	}
	return nil
}

// inventoryCrossBoundaryBlockingEdges records work→infra blocking edges that
// become dangling after the move (risk #2): a staying work bead that depends on
// (is blocked by) a now-moved infra bead. Such an edge stops blocking in bd ready
// once its target leaves the store. It is reported for operator visibility, not
// acted on. movedIDs is the set of ids that moved to the infra store.
func inventoryCrossBoundaryBlockingEdges(domainStores []migrateStore, movedIDs []string) []string {
	if len(movedIDs) == 0 {
		return nil
	}
	moved := make(map[string]struct{}, len(movedIDs))
	for _, id := range movedIDs {
		moved[id] = struct{}{}
	}
	var edges []string
	for _, ds := range domainStores {
		list, err := ds.store.List(beads.ListQuery{IncludeClosed: true, TierMode: beads.TierBoth, AllowScan: true})
		if err != nil {
			continue
		}
		for _, b := range list {
			if _, isMoved := moved[b.ID]; isMoved {
				continue // the source stayed; only staying beads' outbound edges matter
			}
			deps, err := outgoingDepsForBead(ds.store, b)
			if err != nil {
				continue
			}
			for _, d := range deps {
				if _, targetMoved := moved[d.DependsOnID]; !targetMoved {
					continue
				}
				if !beads.IsReadyBlockingDependencyType(d.Type) {
					continue
				}
				edges = append(edges, fmt.Sprintf("%s→%s(%s)", d.IssueID, d.DependsOnID, d.Type))
			}
		}
	}
	sort.Strings(edges)
	return edges
}

// ── small helpers ──

// moveSet2ids returns the bead ids of a move set, in the set's current order.
func moveSet2ids(entries []moveEntry) []string {
	ids := make([]string, 0, len(entries))
	for _, e := range entries {
		ids = append(ids, e.bead.ID)
	}
	return ids
}

// moveSet2entriesBeads returns the source beads of a move set.
func moveSet2entriesBeads(entries []moveEntry) []beads.Bead {
	out := make([]beads.Bead, 0, len(entries))
	for _, e := range entries {
		out = append(out, e.bead)
	}
	return out
}

func dedupeStrings(ids []string) []string {
	seen := make(map[string]struct{}, len(ids))
	out := make([]string, 0, len(ids))
	for _, id := range ids {
		if _, ok := seen[id]; ok {
			continue
		}
		seen[id] = struct{}{}
		out = append(out, id)
	}
	return out
}

func sameStatus(a, b string) bool {
	return normalizeStatus(a) == normalizeStatus(b)
}

func normalizeStatus(s string) string {
	if strings.TrimSpace(s) == "" {
		return "open"
	}
	return s
}

func labelsSubset(want, have []string) bool {
	set := make(map[string]struct{}, len(have))
	for _, l := range have {
		set[l] = struct{}{}
	}
	for _, l := range want {
		if _, ok := set[l]; !ok {
			return false
		}
	}
	return true
}

func metadataSuperset(have, want map[string]string) bool {
	for k, v := range want {
		if have[k] != v {
			return false
		}
	}
	return true
}
