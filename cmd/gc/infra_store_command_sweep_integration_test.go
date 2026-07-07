//go:build integration

package main

import (
	"bytes"
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/coordclass"
	"github.com/gastownhall/gascity/internal/molecule"
	"github.com/gastownhall/gascity/internal/orders"
	"github.com/gastownhall/gascity/internal/session"
	"github.com/gastownhall/gascity/internal/sling"
)

// This is the E4.4 command-sweep tier of the domain/infra store split (backlog
// §E4): the boundary invariant proven LIVE, per gc command, on a two-store
// managed-Dolt city. Where E2.5 (infra_store_boundary_invariant_integration_test.go)
// seeds a representative infra population through the class accessors and asserts
// the boundary ONCE, this tier drives a representative sweep of gc COMMANDS and
// re-asserts the boundary after EACH one:
//
//   - every domain store (city HQ + every rig) holds NO infrastructure-class bead;
//   - the city infra store holds NO domain (work) class bead.
//
// Each sweep step exercises the production CLI store-selection seam against real
// Dolt, so the DESTINATION of every bead a command creates is decided by
// production code (the cmd* one-shot entry points and the cli*Store /
// slingSplitGraphStore / openCityMailProvider routers), never by the test. A
// command that mis-routes an infra create into a domain store — or drops a work
// bead into the infra store — fails the boundary re-assertion for that step,
// naming the command, the bead, and the store. That per-step failure IS the E4.4
// worklist entry the backlog calls for.
//
// It rides the same managed-Dolt harness the passing cmd/gc process tests use
// (setupManagedBdWaitTestCity), so it is gated behind GC_FAST_UNIT=0 and skips —
// never falsely fails — on a machine without a working bd/dolt toolchain. Run
// `make test-cmd-gc-process` (or the -tags integration go test invocation in the
// E4 backlog) for full coverage.

// sweepStep is one gc-command exercise in the live sweep: a label, the class the
// command is expected to CREATE (for the count reconciliation), and a run func
// that drives the real CLI path. wantInfra records whether that class is an
// infrastructure class, so the reconciliation can attribute the bead delta to the
// correct store.
type sweepStep struct {
	name      string
	wantClass coordclass.Class
	// creates reports whether this step is a CREATOR (its bead delta counts toward
	// reconciliation) or a read-only command (asserted only for non-corruption).
	creates bool
	run     func(t *testing.T, city *sweepCity)
}

// sweepCity bundles the live two-store city handles the sweep steps drive.
type sweepCity struct {
	cityPath string
	rigPath  string
	cfg      *config.City
}

func TestInfraStoreCommandSweepIntegration(t *testing.T) {
	// Activate the split for the managed-Dolt city seeded by the harness (same as
	// the E2.5 integration test): GC_INFRA_STORE_SPLIT=1 makes seedInitInfraScope
	// write the .gc/infra canonical scope config, the opt-in gc init uses.
	t.Setenv("GC_INFRA_STORE_SPLIT", "1")

	cityPath, rigPath := setupManagedBdWaitTestCity(t)

	cfg, _, err := loadCityConfigWithBuiltinPacks(cityPath)
	if err != nil {
		t.Fatalf("load city config: %v", err)
	}

	// Seed + bd-init the infra scope (its own Dolt database on the running managed
	// server) through the exact production path gc init performs. Writing
	// config.yaml is what makes cityHasInfraStore true and activates the split.
	if err := seedInitInfraScope(cityPath); err != nil {
		t.Fatalf("seedInitInfraScope: %v", err)
	}
	if !cityHasInfraStore(cityPath) {
		t.Fatal("cityHasInfraStore is false after seeding the infra scope; the split did not activate")
	}
	if err := initAndHookDir(cityPath, infraScopeRoot(cityPath), config.InfraScopePrefix); err != nil {
		t.Fatalf("initAndHookDir(infra scope): %v", err)
	}

	// The infra store may have been probed (and its absence cached) before the
	// scope was seeded. Clear the per-process memo so the CLI seams
	// (cachedCityInfraStore) re-open and resolve the now-present real infra store
	// on the next call — proving live routing off disk, not an injected double.
	clearInfraStoreCacheKey(cityPath)
	t.Cleanup(func() { clearInfraStoreCacheKey(cityPath) })

	// Sanity: the CLI seam must resolve the real infra store off disk now.
	if cachedCityInfraStore(cityPath, cfg) == nil {
		t.Fatal("cachedCityInfraStore returned nil on a seeded split city; the CLI seams would route infra creates to the work store")
	}

	city := &sweepCity{cityPath: cityPath, rigPath: rigPath, cfg: cfg}

	// The sweep. Each creator obtains its destination store from the PRODUCTION
	// CLI seam, so placement is production's decision. The read-only commands must
	// not corrupt either store. Ordered so the session exists before mail/nudge
	// address it.
	steps := []sweepStep{
		// ── negative control: a work bead must stay in the DOMAIN store ──
		{
			name:      "bd create (work → domain)",
			wantClass: coordclass.ClassWork,
			creates:   true,
			run: func(t *testing.T, c *sweepCity) {
				var stdout, stderr bytes.Buffer
				if code := doBd([]string{"--city", c.cityPath, "create", "--json", "real backlog item", "-t", "task"}, &stdout, &stderr); code != 0 {
					t.Fatalf("gc bd create = %d; stdout=%q stderr=%q", code, stdout.String(), stderr.String())
				}
				id := parseCreatedBeadID(t, stdout.String())
				// Prove the negative control directly: the work bead is in the DOMAIN
				// (city) store, not the infra store.
				assertBeadInDomainNotInfra(t, c, id, "gc bd create")
			},
		},
		// ── convoy create: a user convoy is work-class → DOMAIN store ──
		{
			name:      "convoy create (user convoy → domain)",
			wantClass: coordclass.ClassWork,
			creates:   true,
			run: func(t *testing.T, c *sweepCity) {
				var stdout, stderr bytes.Buffer
				name := fmt.Sprintf("sweep-convoy-%d", time.Now().UnixNano())
				if code := cmdConvoyCreateWithOptions([]string{name}, convoyCreateOptions{}, &stdout, &stderr); code != 0 {
					t.Fatalf("gc convoy create = %d; stdout=%q stderr=%q", code, stdout.String(), stderr.String())
				}
			},
		},
		// ── SESSION: the session bead is infra-class → INFRA store ──
		{
			name:      "session create (session → infra)",
			wantClass: coordclass.ClassSessions,
			creates:   true,
			run: func(t *testing.T, c *sweepCity) {
				// Drive the CLI session-store seam (cliSessionStore →
				// cachedCityInfraStore) that every gc session RunE routes through, on
				// the live city work store. A full `gc session new` needs a tmux/worker
				// runtime (out of harness); the bead-creating CORE is the session-store
				// Create, which is what the split routes. This resolves the real infra
				// store off disk exactly as the running CLI would.
				work := openCityStoreForSweep(t, c)
				defer closeSweepStore(t, work)
				sessStore := cliSessionStore(work, c.cfg, c.cityPath)
				if _, err := session.NewStore(beads.SessionStore{Store: sessStore}).CreateSession(session.CreateSpec{
					Title:     "worker-1",
					AgentName: "worker-1",
					Metadata:  map[string]string{"provider": "tmux", "template": "claude"},
				}); err != nil {
					t.Fatalf("session create via cliSessionStore: %v", err)
				}
			},
		},
		// ── session list: read-only; must not corrupt either store ──
		{
			name: "session list (read-only)",
			run: func(t *testing.T, c *sweepCity) {
				var stdout, stderr bytes.Buffer
				if code := cmdSessionList("", "", false, &stdout, &stderr); code != 0 {
					t.Fatalf("gc session list = %d; stderr=%q", code, stderr.String())
				}
			},
		},
		// ── MAIL: the message bead is infra-class → INFRA store ──
		{
			name:      "mail send (message → infra)",
			wantClass: coordclass.ClassMessaging,
			creates:   true,
			run: func(t *testing.T, c *sweepCity) {
				// openCityMailProvider is the exact opener every gc mail RunE uses; on a
				// split city it builds the provider over resolveMailMessagesStore(...,
				// cachedCityInfraStore(...)) → the infra store. Drive Send through it so
				// the CLI store-selection is production's, then bypass only the
				// recipient-name UI layer (which needs a live-session mailbox lookup).
				mp, code := openCityMailProvider(&bytes.Buffer{}, "gc mail send")
				if mp == nil {
					t.Fatalf("openCityMailProvider = nil (code %d)", code)
				}
				if _, err := mp.Send("human", "worker-1", "hello", "body text"); err != nil {
					t.Fatalf("mail send via openCityMailProvider: %v", err)
				}
			},
		},
		// ── mail list/inbox: read-only ──
		{
			name: "mail inbox (read-only)",
			run: func(t *testing.T, c *sweepCity) {
				var stdout, stderr bytes.Buffer
				// A missing-recipient inbox may exit non-zero; we only assert it does
				// not corrupt the stores. Route to human's inbox which always resolves.
				_ = cmdMailInbox([]string{"human"}, &stdout, &stderr)
			},
		},
		// ── NUDGE enqueue: the nudge shadow bead is infra-class → INFRA store ──
		{
			name:      "nudge enqueue (nudge → infra)",
			wantClass: coordclass.ClassNudges,
			creates:   true,
			run: func(t *testing.T, c *sweepCity) {
				// cliNudgesStore is the CLI nudge-class seam; the queued-nudge shadow
				// bead is what `gc nudge` enqueues. Drive the seam live so the shadow
				// bead lands where production routes it.
				work := openCityStoreForSweep(t, c)
				defer closeSweepStore(t, work)
				ns := cliNudgesStore(work, c.cfg, c.cityPath)
				if _, created, err := ensureQueuedNudgeBead(beads.NudgesStore{Store: ns},
					newQueuedNudge("worker-1", "please continue", time.Now().UTC())); err != nil {
					t.Fatalf("nudge enqueue via cliNudgesStore: %v", err)
				} else if !created {
					t.Fatal("nudge enqueue: expected a bead to be created")
				}
			},
		},
		// ── nudge status: read-only ──
		{
			name: "nudge status (read-only)",
			run: func(t *testing.T, c *sweepCity) {
				var stdout, stderr bytes.Buffer
				// gc nudge status may exit non-zero when no controller is up; it must
				// not corrupt the stores.
				_ = cmdNudgeStatus([]string{"worker-1"}, false, &stdout, &stderr)
			},
		},
		// ── ORDER-TRACKING: the run bead is infra-class → INFRA store ──
		{
			name:      "order run tracking (order → infra)",
			wantClass: coordclass.ClassOrders,
			creates:   true,
			run: func(t *testing.T, c *sweepCity) {
				// The order-tracking run bead is created via the orders store; on a
				// split city cliOrderStore routes it to the infra store. Drive the CLI
				// order-class seam live (a full `gc order run` for a graph formula needs
				// an order configured in city.toml; the tracking bead is the order-class
				// artifact the split routes, exercised here through the same seam
				// doOrderRunWithJSON uses).
				work := openCityStoreForSweep(t, c)
				defer closeSweepStore(t, work)
				orderStore := cliOrderStore(work, c.cfg, c.cityPath)
				if _, err := orders.NewStore(beads.OrdersStore{Store: orderStore}).CreateRun("gate-alpha", orders.RunOpts{}); err != nil {
					t.Fatalf("order run tracking via cliOrderStore: %v", err)
				}
			},
		},
		// ── SLING graph molecule: the E2.3 historical-leak target → INFRA store ──
		{
			name:      "sling graph molecule (graph → infra)",
			wantClass: coordclass.ClassGraph,
			creates:   true,
			run: func(t *testing.T, c *sweepCity) {
				// This is the command that historically LEAKED (E2.3): a formula sling
				// materializes a workflow/wisp molecule explosion. Build the exact
				// SlingDeps.GraphStore production builds (slingSplitGraphStore →
				// cliGraphStore → cachedCityInfraStore) and materialize onto
				// SlingDeps.graphStore(), so the molecule root + steps land where the
				// live CLI routes them. A full `gc sling --formula` also dispatches to a
				// worker (out of harness); the bead-creating core is this molecule
				// materialization.
				rig, err := openStoreAtForCity(c.rigPath, c.cityPath)
				if err != nil {
					t.Fatalf("open rig store: %v", err)
				}
				defer closeSweepStore(t, rig)
				deps := sling.SlingDeps{
					Store:      rig,
					GraphStore: slingSplitGraphStore(rig, c.cfg, c.cityPath),
				}
				if deps.GraphStore == nil {
					t.Fatal("split city: SlingDeps.GraphStore must be the infra store, got nil (the sling graph seam is unrouted)")
				}
				if _, err := molecule.Instantiate(context.Background(), slingDepsGraphStore(deps), graphRecipe(), molecule.Options{}); err != nil {
					t.Fatalf("molecule instantiate (sling seam): %v", err)
				}
			},
		},
		// ── status: read-only aggregate over both stores ──
		{
			name: "city status (read-only)",
			run: func(t *testing.T, c *sweepCity) {
				var stdout, stderr bytes.Buffer
				// gc status can exit non-zero without a live controller; it must not
				// corrupt either store.
				_ = cmdCityStatus(nil, false, &stdout, &stderr)
			},
		},
	}

	// Track expected creator deltas per store class for the final reconciliation.
	wantInfraCreated := 0
	wantDomainCreated := 0

	for _, step := range steps {
		step.run(t, city)

		// Re-open BOTH stores fresh after each command and re-assert the boundary.
		// Fresh handles over the live Dolt server reflect every write the command
		// made, so a mis-routed bead is caught the instant it lands.
		assertSweepBoundary(t, city, step.name)

		if step.creates {
			if step.wantClass.IsInfrastructure() {
				wantInfraCreated++
			} else {
				wantDomainCreated++
			}
		}
		if t.Failed() {
			t.Fatalf("boundary violated after %q; stopping the sweep (see the wrong-side bead above — that is the E4.4 worklist entry)", step.name)
		}
	}

	// ── Final count reconciliation ──
	//
	// Every domain store together must hold at least the work beads the sweep
	// created, and the infra store at least the infra beads (>= because the
	// production creators mint internal siblings — a session may spawn a wait,
	// molecule.Instantiate mints multiple graph nodes, mail may mint a thread
	// bead — all correctly classified and side-checked above). The invariant that
	// matters is that NO bead is on the wrong side; the >= guards prove the sweep
	// was not vacuous (each creator actually produced a bead in its store).
	work := openCityStoreForSweep(t, city)
	defer closeSweepStore(t, work)
	infra := openCityInfraStoreForSweep(t, city)
	defer closeSweepStore(t, infra)

	infraCount := storeBeadCount(t, infra)
	if infraCount < wantInfraCreated {
		t.Errorf("infra store holds %d beads, want >= %d (one per infra creator, plus internal siblings)", infraCount, wantInfraCreated)
	}
	workCount := storeBeadCount(t, work)
	if workCount < wantDomainCreated {
		t.Errorf("city work store holds %d beads, want >= %d (one per work creator)", workCount, wantDomainCreated)
	}

	// The authoritative gate one more time, on the fully-populated stores.
	assertStoreClassBoundary(t, "domain:hq", work, false)
	assertStoreClassBoundary(t, "infra", infra, true)

	t.Logf("command sweep OK: %d steps (%d infra creators, %d work creators); infra store=%d beads, work store=%d beads",
		len(steps), wantInfraCreated, wantDomainCreated, infraCount, workCount)
}

// assertSweepBoundary re-opens every domain store (city HQ + rig) plus the infra
// store fresh and asserts the boundary invariant, labeling any failure with the
// command that just ran.
func assertSweepBoundary(t *testing.T, c *sweepCity, cmd string) {
	t.Helper()

	work := openCityStoreForSweep(t, c)
	defer closeSweepStore(t, work)
	assertStoreClassBoundary(t, "after "+cmd+" — domain:hq", work, false)

	rig, err := openStoreAtForCity(c.rigPath, c.cityPath)
	if err != nil {
		t.Fatalf("after %s: open rig store: %v", cmd, err)
	}
	defer closeSweepStore(t, rig)
	assertStoreClassBoundary(t, "after "+cmd+" — domain:rig", rig, false)

	infra := openCityInfraStoreForSweep(t, c)
	defer closeSweepStore(t, infra)
	assertStoreClassBoundary(t, "after "+cmd+" — infra", infra, true)
}

// assertBeadInDomainNotInfra proves the negative control directly: a freshly
// created work bead resolves in the domain store and is absent from the infra
// store, so the boundary is not trivially "route everything to infra".
func assertBeadInDomainNotInfra(t *testing.T, c *sweepCity, id, cmd string) {
	t.Helper()
	work := openCityStoreForSweep(t, c)
	defer closeSweepStore(t, work)
	if _, err := work.Get(id); err != nil {
		t.Errorf("%s: work bead %q not found in the DOMAIN store: %v", cmd, id, err)
	}
	infra := openCityInfraStoreForSweep(t, c)
	defer closeSweepStore(t, infra)
	if _, err := infra.Get(id); err == nil {
		t.Errorf("%s: work bead %q leaked into the INFRA store (the boundary is not just 'everything to infra')", cmd, id)
	}
}

// openCityStoreForSweep opens a FRESH handle to the city work store. Fresh
// handles over the live Dolt server reflect every prior write, so re-opening
// after each command avoids any per-handle caching staleness.
func openCityStoreForSweep(t *testing.T, c *sweepCity) beads.Store {
	t.Helper()
	store, err := openCityStoreAt(c.cityPath)
	if err != nil {
		t.Fatalf("open city work store: %v", err)
	}
	return store
}

// openCityInfraStoreForSweep opens a FRESH handle to the city infra store. It is
// a distinct handle from the one cachedCityInfraStore memoizes for the CLI seams,
// so closing it never disturbs the cached routing store.
func openCityInfraStoreForSweep(t *testing.T, c *sweepCity) beads.Store {
	t.Helper()
	infra, present, err := openCityInfraStoreAt(c.cityPath)
	if err != nil {
		t.Fatalf("open city infra store: %v", err)
	}
	if !present || infra == nil {
		t.Fatal("infra store not present on a seeded split city")
	}
	return infra
}

func closeSweepStore(t *testing.T, store beads.Store) {
	t.Helper()
	if err := closeBeadStoreHandle(store); err != nil {
		t.Errorf("close store handle: %v", err)
	}
}
