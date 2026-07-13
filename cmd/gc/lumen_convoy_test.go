package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/graphstore"
	"github.com/gastownhall/gascity/internal/lumen/engine"
	"github.com/gastownhall/gascity/internal/lumen/ir"
)

// --- resolver fixtures ------------------------------------------------------

// drainConvoyID is the fixed convoy id every resolver fixture uses.
const drainConvoyID = "gc-convoy"

// newDrainConvoyStore builds a MemStore holding a convoy bead (drainConvoyID) that TRACKS
// the given members (plus each member as a real bead), returning the store. Members are
// seeded via NewMemStoreFrom so their CreatedAt is explicit — letting the sort-canonical
// pin observe that the resolver sorts by ID ascending independent of convoycore.Members'
// CreatedAt order.
func newDrainConvoyStore(members []beads.Bead) *beads.MemStore {
	all := make([]beads.Bead, 0, len(members)+1)
	all = append(all, beads.Bead{ID: drainConvoyID, Title: "drain", Type: "convoy", Status: "open", CreatedAt: time.Unix(0, 0)})
	all = append(all, members...)
	deps := make([]beads.Dep, 0, len(members))
	for _, m := range members {
		deps = append(deps, beads.Dep{IssueID: drainConvoyID, DependsOnID: m.ID, Type: "tracks"})
	}
	return beads.NewMemStoreFrom(len(all), all, deps)
}

// member is a compact drain-convoy member bead builder with an explicit CreatedAt.
func member(id, title string, createdAt time.Time) beads.Bead {
	return beads.Bead{ID: id, Title: title, Type: "task", Status: "open", CreatedAt: createdAt}
}

// TestResolveConvoyMemberIDsSortedCanonical pins the canonical ordering (§3.3): the
// resolver returns member ids sorted by ID ASCENDING, independent of the order
// convoycore.Members yields them (here CreatedAt order is the REVERSE of ID order). Break
// the sort (return Members' order / sort descending) and this reds — the load-bearing
// determinism guard.
func TestResolveConvoyMemberIDsSortedCanonical(t *testing.T) {
	base := time.Unix(1000, 0)
	// CreatedAt DESC relative to ID: gc-cc earliest, gc-aa latest ⇒ Members returns
	// [gc-cc, gc-bb, gc-aa]; the resolver must re-sort to [gc-aa, gc-bb, gc-cc].
	store := newDrainConvoyStore([]beads.Bead{
		member("gc-aa", "a", base.Add(3*time.Second)),
		member("gc-bb", "b", base.Add(2*time.Second)),
		member("gc-cc", "c", base.Add(1*time.Second)),
	})

	ids, err := resolveConvoyMemberIDs(store, drainConvoyID, nil)
	if err != nil {
		t.Fatalf("resolveConvoyMemberIDs: %v", err)
	}
	want := []string{"gc-aa", "gc-bb", "gc-cc"}
	if len(ids) != len(want) {
		t.Fatalf("ids = %v, want %v", ids, want)
	}
	for i := range want {
		if ids[i] != want[i] {
			t.Fatalf("ids[%d] = %q, want %q (ascending by ID, not Members/CreatedAt order); all=%v", i, ids[i], want[i], ids)
		}
	}
}

// TestResolveConvoyInputHashStableAndFreezeIDs pins the freeze-ids determinism property
// (§3.3): the resolved id array, seeded and hashed via engine.InputHash, is
//   - STABLE across repeated resolves of the same membership,
//   - UNCHANGED when a member's mutable field (title) changes after the snapshot (the
//     resolver freezes IDS, not bead snapshots), and
//   - CHANGED when membership changes (a member is added).
//
// Mutation pin (ii): project member beads instead of ids and the title-mutation branch
// reds (a mutable field perturbs the hash).
func TestResolveConvoyInputHashStableAndFreezeIDs(t *testing.T) {
	base := time.Unix(2000, 0)
	store := newDrainConvoyStore([]beads.Bead{
		member("gc-aa", "a", base.Add(1*time.Second)),
		member("gc-bb", "b", base.Add(2*time.Second)),
		member("gc-cc", "c", base.Add(3*time.Second)),
	})

	hashOf := func(t *testing.T) string {
		t.Helper()
		ids, err := resolveConvoyMemberIDs(store, drainConvoyID, nil)
		if err != nil {
			t.Fatalf("resolve: %v", err)
		}
		return engine.InputHash(map[string]any{"members": idsToAnySlice(ids)})
	}

	h1 := hashOf(t)
	if h1 == "" {
		t.Fatal("InputHash of a non-empty resolved membership is empty")
	}
	if h2 := hashOf(t); h2 != h1 {
		t.Errorf("InputHash not stable across repeated resolves: %q vs %q", h2, h1)
	}

	// Mutate a member's TITLE — a mutable field that must NOT enter the run identity.
	newTitle := "renamed"
	if err := store.Update("gc-bb", beads.UpdateOpts{Title: &newTitle}); err != nil {
		t.Fatalf("update member title: %v", err)
	}
	if hMut := hashOf(t); hMut != h1 {
		t.Errorf("InputHash changed after a member TITLE change: %q vs %q (freeze-ids violated — the resolver must project ids, not bead snapshots)", hMut, h1)
	}

	// Add a member — membership MUST change the hash. MemStore.Create assigns the id,
	// so track the RETURNED bead.
	added, err := store.Create(beads.Bead{Title: "d", Type: "task"})
	if err != nil {
		t.Fatalf("create new member: %v", err)
	}
	if err := store.DepAdd(drainConvoyID, added.ID, "tracks"); err != nil {
		t.Fatalf("track new member: %v", err)
	}
	if hAdd := hashOf(t); hAdd == h1 {
		t.Errorf("InputHash unchanged after ADDING a member — membership must pin the hash")
	}
}

// TestResolveConvoyRejectsUnresolvedMember pins R-UNRESOLVED: a dangling tracks target
// (no backing bead) surfaces as an unresolved placeholder and the resolver hard-fails
// with errConvoyMemberUnresolved — so a broken convoy never becomes a silently-empty
// run. Mutation pin (iv): drop the unresolved rejection and this reds.
func TestResolveConvoyRejectsUnresolvedMember(t *testing.T) {
	base := time.Unix(3000, 0)
	store := newDrainConvoyStore([]beads.Bead{
		member("gc-aa", "a", base.Add(1*time.Second)),
	})
	// A tracks edge to a bead that does not exist ⇒ unresolved placeholder.
	if err := store.DepAdd(drainConvoyID, "gc-missing", "tracks"); err != nil {
		t.Fatalf("add dangling tracks: %v", err)
	}

	_, err := resolveConvoyMemberIDs(store, drainConvoyID, nil)
	if !errors.Is(err, errConvoyMemberUnresolved) {
		t.Fatalf("err = %v, want errConvoyMemberUnresolved", err)
	}
}

// TestResolveConvoyRefusesInterMemberBlocks pins R-ORDER (§7, the one real semantic gap):
// a resolved convoy in which one member has a ready-blocking edge to ANOTHER member is
// refused LOUD (a flat for-each cannot gate them); a convoy of INDEPENDENT members
// proceeds. Mutation pin (iii): drop the R-ORDER guard and the refusal branch reds.
func TestResolveConvoyRefusesInterMemberBlocks(t *testing.T) {
	base := time.Unix(4000, 0)
	mk := func() *beads.MemStore {
		return newDrainConvoyStore([]beads.Bead{
			member("gc-aa", "a", base.Add(1*time.Second)),
			member("gc-bb", "b", base.Add(2*time.Second)),
		})
	}

	// Independent members → proceeds.
	indep := mk()
	if ids, err := resolveConvoyMemberIDs(indep, drainConvoyID, nil); err != nil {
		t.Fatalf("independent members must proceed, got err %v (ids=%v)", err, ids)
	}

	// gc-aa blocks-on gc-bb (both convoy members) → refuse loud.
	blocked := mk()
	if err := blocked.DepAdd("gc-aa", "gc-bb", "blocks"); err != nil {
		t.Fatalf("add inter-member blocks: %v", err)
	}
	_, err := resolveConvoyMemberIDs(blocked, drainConvoyID, nil)
	if !errors.Is(err, errConvoyInterMemberOrder) {
		t.Fatalf("err = %v, want errConvoyInterMemberOrder", err)
	}
}

// TestResolveConvoyBlocksEdgeToNonMemberProceeds pins the R-ORDER boundary: a member
// blocking on a bead OUTSIDE the convoy is not an inter-member ordering constraint, so
// the drain proceeds (the guard fires only on intra-convoy edges).
func TestResolveConvoyBlocksEdgeToNonMemberProceeds(t *testing.T) {
	base := time.Unix(4500, 0)
	store := newDrainConvoyStore([]beads.Bead{
		member("gc-aa", "a", base.Add(1*time.Second)),
		member("gc-bb", "b", base.Add(2*time.Second)),
	})
	// gc-aa blocks on an external bead, which is NOT tracked by the convoy.
	outside, err := store.Create(beads.Bead{Title: "x", Type: "task"})
	if err != nil {
		t.Fatalf("create non-member: %v", err)
	}
	if err := store.DepAdd("gc-aa", outside.ID, "blocks"); err != nil {
		t.Fatalf("add external blocks: %v", err)
	}
	if _, err := resolveConvoyMemberIDs(store, drainConvoyID, nil); err != nil {
		t.Fatalf("a blocks edge to a NON-member must not trip R-ORDER, got %v", err)
	}
}

// newCrossStoreDrainConvoy builds a two-store drain fixture: a PRIMARY store holding the
// convoy bead, its tracks edges to BOTH members, and the primary-resident member gc-pp;
// and a TAIL member store holding the tail-resident member gc-tt plus tailDeps (gc-tt's
// dependency edges, co-resident with it — invisible to the primary store). Members are
// seeded with explicit ids (NewMemStoreFrom) so cross-store id references resolve.
func newCrossStoreDrainConvoy(base time.Time, tailDeps []beads.Dep) (primary, tail *beads.MemStore) {
	primary = beads.NewMemStoreFrom(2,
		[]beads.Bead{
			{ID: drainConvoyID, Title: "drain", Type: "convoy", Status: "open", CreatedAt: base},
			{ID: "gc-pp", Title: "p", Type: "task", Status: "open", CreatedAt: base.Add(1 * time.Second)},
		},
		[]beads.Dep{
			{IssueID: drainConvoyID, DependsOnID: "gc-pp", Type: "tracks"},
			{IssueID: drainConvoyID, DependsOnID: "gc-tt", Type: "tracks"},
		})
	tail = beads.NewMemStoreFrom(1,
		[]beads.Bead{
			{ID: "gc-tt", Title: "t", Type: "task", Status: "open", CreatedAt: base.Add(2 * time.Second)},
		},
		tailDeps)
	return primary, tail
}

// TestResolveConvoyCrossStoreInterMemberBlocksRefuses pins the R-ORDER guard across a
// STORE boundary: a tail-resident member (gc-tt) carrying a blocks edge to a
// primary-resident member (gc-pp) — both in the convoy — must STILL refuse loud. The
// tail member's dependency edges are co-resident with it in the tail store, so the guard
// MUST read them from the member's OWNING store (convoyMemberOwningStore). Mutation:
// make convoyMemberOwningStore always return the primary store → primary.DepList("gc-tt")
// misses the tail-resident edge → this pin reds (a silently mis-ordered cross-store drain,
// the exact hazard the guard exists to prevent).
func TestResolveConvoyCrossStoreInterMemberBlocksRefuses(t *testing.T) {
	base := time.Unix(8000, 0)
	primary, tail := newCrossStoreDrainConvoy(base, []beads.Dep{
		{IssueID: "gc-tt", DependsOnID: "gc-pp", Type: "blocks"},
	})
	_, err := resolveConvoyMemberIDs(primary, drainConvoyID, []beads.Store{tail})
	if !errors.Is(err, errConvoyInterMemberOrder) {
		t.Fatalf("cross-store inter-member blocks: err = %v, want errConvoyInterMemberOrder (the tail member's dep edge must be read from its OWNING store)", err)
	}
}

// TestResolveConvoyCrossStoreExternalDepProceeds is the positive cross-store case: a
// tail-resident member whose only blocks edge targets a NON-member proceeds (both
// cross-store members resolve and sort; the guard fires only on intra-convoy edges).
func TestResolveConvoyCrossStoreExternalDepProceeds(t *testing.T) {
	base := time.Unix(8500, 0)
	primary, tail := newCrossStoreDrainConvoy(base, []beads.Dep{
		{IssueID: "gc-tt", DependsOnID: "gc-external", Type: "blocks"},
	})
	ids, err := resolveConvoyMemberIDs(primary, drainConvoyID, []beads.Store{tail})
	if err != nil {
		t.Fatalf("cross-store external dep must proceed, got %v", err)
	}
	want := []string{"gc-pp", "gc-tt"}
	if len(ids) != len(want) || ids[0] != want[0] || ids[1] != want[1] {
		t.Fatalf("ids = %v, want %v (both cross-store members resolved and sorted)", ids, want)
	}
}

// TestResolveConvoyEmptyIsLegal pins R-EMPTY: a convoy with zero live members resolves to
// an empty (nil/zero-length) id array with NO error — a legal empty drain (the caller
// warns-and-proceeds; the fan settles a vacuous PASS).
func TestResolveConvoyEmptyIsLegal(t *testing.T) {
	store := newDrainConvoyStore(nil)
	ids, err := resolveConvoyMemberIDs(store, drainConvoyID, nil)
	if err != nil {
		t.Fatalf("empty convoy must be legal, got err %v", err)
	}
	if len(ids) != 0 {
		t.Fatalf("ids = %v, want empty", ids)
	}
}

// TestResolveConvoyClosedMembersExcluded pins the live-set intent (includeClosed=false):
// a CLOSED member is not part of the drained set.
func TestResolveConvoyClosedMembersExcluded(t *testing.T) {
	base := time.Unix(5000, 0)
	store := newDrainConvoyStore([]beads.Bead{
		member("gc-aa", "a", base.Add(1*time.Second)),
		{ID: "gc-bb", Title: "b", Type: "task", Status: "closed", CreatedAt: base.Add(2 * time.Second)},
	})
	ids, err := resolveConvoyMemberIDs(store, drainConvoyID, nil)
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if len(ids) != 1 || ids[0] != "gc-aa" {
		t.Fatalf("ids = %v, want [gc-aa] (closed member excluded from the live set)", ids)
	}
}

// TestParseInputConvoyFlag pins the flag grammar: <field>=<convoyID> with both sides
// required; malformed specs are loud errors.
func TestParseInputConvoyFlag(t *testing.T) {
	ok, err := parseInputConvoyFlag(" members = gc-convoy ")
	if err != nil {
		t.Fatalf("valid spec errored: %v", err)
	}
	if ok.Field != "members" || ok.ConvoyID != drainConvoyID {
		t.Fatalf("parsed = %+v, want {members gc-convoy} (trimmed)", ok)
	}
	for _, bad := range []string{"", "members", "=gc-convoy", "members=", "  =  "} {
		if _, err := parseInputConvoyFlag(bad); err == nil {
			t.Errorf("parseInputConvoyFlag(%q) = nil error, want a loud refusal", bad)
		}
	}
}

// --- sling seam tests -------------------------------------------------------

// drainConvoyIR is a minimal build-from-convoy formula: `accepts {members:[Text]};
// for-each member input.members { run impl given {item: <binder>} }; impl = do {{item}}`.
// It lowers under the enqueue gate and declares `members` required, so the seeded array
// satisfies resolveDeclaredInput and is pinned into the run input hash.
const drainConvoyIR = `{
  "contract": {"name":"lumen.ir","version":"0.2.5","producer":"test"},
  "name":"drain-convoy",
  "input":{"name":"drain-convoy.input","fields":[
    {"name":"members","type":{"kind":"array","element":{"kind":"atomic","name":"string"}},"required":true,"body":false}
  ]},
  "nodes":[
    {"kind":"scatter","id":"fanout","name":"fanout","after":[],
     "form":"each","binder":"item",
     "over":{"kind":"member","base":{"kind":"ref","name":"input"},"name":"members"},
     "body":{"kind":"block","id":"fanout.body","after":[],"members":[
       {"kind":"run","id":"lane","name":"lane","after":[],
        "target":{"kind":"by-name","name":"impl"},
        "environment":{"fields":[
          {"name":"item","value":{"kind":"expr","expr":{"kind":"ref","name":"item"}}}
        ]},
        "outcome":"transparent"}
     ]},
     "on_fail":"continue"}
  ],
  "formulas":{
    "impl":{
      "contract":{"name":"lumen.ir","version":"0.2.5","producer":"test"},
      "name":"impl",
      "input":{"name":"impl.input","fields":[
        {"name":"item","type":{"kind":"atomic","name":"string"},"required":true,"body":false}
      ]},
      "nodes":[
        {"kind":"do","id":"work","name":"work","after":[],
         "source":{"kind":"prompt"},
         "interpreter":{"kind":"agent","mode":{"kind":"do"}},
         "body":{"raw":"Process {{ item }}.","language":"markdown","source":{"kind":"inline"}}}
      ]
    }
  }
}`

// writeDrainConvoyIR writes the drain-convoy IR to a temp file under cityPath.
func writeDrainConvoyIR(t *testing.T, cityPath string) string {
	t.Helper()
	path := filepath.Join(cityPath, "drain-convoy.lumen.json")
	if err := os.WriteFile(path, []byte(drainConvoyIR), 0o644); err != nil {
		t.Fatalf("write drain-convoy IR: %v", err)
	}
	return path
}

// stubLumenConvoyStores replaces the store-opening seam with one returning a fixed
// convoy store (and no member stores) for the duration of a test.
func stubLumenConvoyStores(t *testing.T, store beads.Store) {
	t.Helper()
	orig := lumenConvoyStores
	lumenConvoyStores = func(string) (beads.Store, []beads.Store, error) {
		return store, nil, nil
	}
	t.Cleanup(func() { lumenConvoyStores = orig })
}

// TestLumenSlingInputConvoySeedsSortedIDs (end-to-end sling seed) proves an
// --input-convoy binding resolves the convoy, seeds input[members] with the SORTED id
// array, writes it as the run's content-addressed input blob, and opens a discoverable
// run whose pinned input_hash matches engine.InputHash of the seeded array.
func TestLumenSlingInputConvoySeedsSortedIDs(t *testing.T) {
	ctx := context.Background()
	cityPath := tbHookGraphCity(t)
	irPath := writeDrainConvoyIR(t, cityPath)
	stubPokeLumenRuns(t)

	base := time.Unix(6000, 0)
	// CreatedAt reverse of ID order to prove the seed is sorted, not Members order.
	store := newDrainConvoyStore([]beads.Bead{
		member("gc-aa", "a", base.Add(3*time.Second)),
		member("gc-bb", "b", base.Add(2*time.Second)),
		member("gc-cc", "c", base.Add(1*time.Second)),
	})
	stubLumenConvoyStores(t, store)

	var stderr bytes.Buffer
	res, err := lumenEnqueue(ctx, cityPath, lumenEnqueueRequest{
		IRPath:       irPath,
		Route:        tbHookRoute,
		InputConvoys: []inputConvoyBinding{{Field: "members", ConvoyID: drainConvoyID}},
	}, &stderr)
	if err != nil {
		t.Fatalf("lumenEnqueue: %v; stderr=%s", err, stderr.String())
	}

	wantInput := map[string]any{"members": []any{"gc-aa", "gc-bb", "gc-cc"}}
	wantHash := engine.InputHash(wantInput)

	// The input blob is content-addressed by the pinned hash and decodes to the sorted ids.
	raw, err := os.ReadFile(lumenInputBlobPath(cityPath, wantHash))
	if err != nil {
		t.Fatalf("input blob missing at the pinned hash: %v", err)
	}
	var gotInput map[string]any
	if err := json.Unmarshal(raw, &gotInput); err != nil {
		t.Fatalf("input blob is not JSON: %v", err)
	}
	membersAny, ok := gotInput["members"].([]any)
	if !ok {
		t.Fatalf("input blob members = %T, want []any: %v", gotInput["members"], gotInput)
	}
	got := make([]string, len(membersAny))
	for i, v := range membersAny {
		got[i], _ = v.(string)
	}
	want := []string{"gc-aa", "gc-bb", "gc-cc"}
	for i := range want {
		if i >= len(got) || got[i] != want[i] {
			t.Fatalf("seeded members = %v, want %v (sorted ascending)", got, want)
		}
	}

	// The run is discoverable and its run.started pins exactly that input hash.
	gs := tbHookOpenStore(t, cityPath)
	defer func() { _ = gs.Close() }()
	m, err := engine.ReadRunManifest(ctx, gs, res.StreamID)
	if err != nil {
		t.Fatalf("read run manifest: %v", err)
	}
	if m.InputHash != wantHash {
		t.Fatalf("run.started input_hash = %q, want %q (the frozen membership pins the run identity)", m.InputHash, wantHash)
	}
}

// TestLumenSlingInputConvoyUnresolvedHardFailsNoRun pins R-UNRESOLVED at the sling: a
// convoy with a dangling member fails the sling LOUD with NO discoverable run — the
// engine enqueue seam is never reached and no input blob is written.
func TestLumenSlingInputConvoyUnresolvedHardFailsNoRun(t *testing.T) {
	ctx := context.Background()
	cityPath := tbHookGraphCity(t)
	irPath := writeDrainConvoyIR(t, cityPath)
	stubPokeLumenRuns(t)

	base := time.Unix(7000, 0)
	store := newDrainConvoyStore([]beads.Bead{member("gc-aa", "a", base)})
	if err := store.DepAdd(drainConvoyID, "gc-missing", "tracks"); err != nil {
		t.Fatalf("add dangling tracks: %v", err)
	}
	stubLumenConvoyStores(t, store)

	// Wrap the engine seam to prove it is NEVER called on a broken convoy.
	called := false
	origSeam := lumenEngineEnqueueRun
	lumenEngineEnqueueRun = func(ctx context.Context, gs *graphstore.Store, doc *ir.IR, in map[string]any, formulaRef, defaultRoute, driverKind string) (string, error) {
		called = true
		return origSeam(ctx, gs, doc, in, formulaRef, defaultRoute, driverKind)
	}
	t.Cleanup(func() { lumenEngineEnqueueRun = origSeam })

	var stderr bytes.Buffer
	_, err := lumenEnqueue(ctx, cityPath, lumenEnqueueRequest{
		IRPath:       irPath,
		Route:        tbHookRoute,
		InputConvoys: []inputConvoyBinding{{Field: "members", ConvoyID: drainConvoyID}},
	}, &stderr)
	if !errors.Is(err, errConvoyMemberUnresolved) {
		t.Fatalf("err = %v, want errConvoyMemberUnresolved", err)
	}
	if called {
		t.Fatal("engine EnqueueRun was called for a broken convoy (a run became discoverable)")
	}
	if _, err := os.Stat(filepath.Join(cityPath, ".gc", "graph", "input")); !os.IsNotExist(err) {
		t.Fatalf("an input blob dir was written for a broken convoy (want nothing): stat err=%v", err)
	}
	if _, err := os.Stat(filepath.Join(cityPath, ".gc", "graph", "ir")); !os.IsNotExist(err) {
		t.Fatalf("an IR blob dir was written for a broken convoy (want nothing): stat err=%v", err)
	}
}

// TestSeedInputConvoysRefusesDuplicateField pins the duplicate-field refusal: the same
// input field bound by two --input-convoy directives is refused LOUD (explicit operator
// intent must be unambiguous). The refusal fires BEFORE any store is opened.
func TestSeedInputConvoysRefusesDuplicateField(t *testing.T) {
	stubLumenConvoyStores(t, newDrainConvoyStore(nil))
	var stderr bytes.Buffer
	_, err := seedInputConvoys(t.TempDir(), nil, []inputConvoyBinding{
		{Field: "members", ConvoyID: "c1"},
		{Field: "members", ConvoyID: "c2"},
	}, &stderr)
	if err == nil || !strings.Contains(err.Error(), "bound more than once") {
		t.Fatalf("err = %v, want a duplicate-field refusal", err)
	}
}

// TestSeedInputConvoysRefusesFieldAlsoInInput pins the --input collision refusal: a field
// set via BOTH --input and --input-convoy is refused LOUD rather than silently shadowed.
func TestSeedInputConvoysRefusesFieldAlsoInInput(t *testing.T) {
	stubLumenConvoyStores(t, newDrainConvoyStore(nil))
	var stderr bytes.Buffer
	_, err := seedInputConvoys(t.TempDir(), map[string]any{"members": []any{"x"}}, []inputConvoyBinding{
		{Field: "members", ConvoyID: "c1"},
	}, &stderr)
	if err == nil || !strings.Contains(err.Error(), "also set via --input") {
		t.Fatalf("err = %v, want a --input collision refusal", err)
	}
}
