package main

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/nudgequeue"
	"github.com/gastownhall/gascity/internal/reconcilekey"
)

type fakeNudgeCommandPager struct {
	page    nudgequeue.CommandIndexPage
	err     error
	calls   int
	session string
	after   uint64
	limit   int
	onPage  func()
}

func (f *fakeNudgeCommandPager) Page(sessionID string, afterSequence uint64, limit int) (nudgequeue.CommandIndexPage, error) {
	f.calls++
	f.session = sessionID
	f.after = afterSequence
	f.limit = limit
	if f.onPage != nil {
		f.onPage()
	}
	return f.page, f.err
}

func TestNewNudgeKeyReconcilerDerivesCanonicalScopeFromStoreLineage(t *testing.T) {
	store := nudgequeue.CommandStoreBinding{StoreUUID: "store-1", RestoreEpoch: 7}
	reconciler, err := newNudgeKeyReconciler(&fakeNudgeCommandPager{}, store, 1)
	if err != nil {
		t.Fatalf("newNudgeKeyReconciler: %v", err)
	}
	if got, want := reconciler.keyScope, "command-store/v1/7:store-1/7"; got != want {
		t.Fatalf("key scope = %q, want %q", got, want)
	}

	otherEpoch, err := newNudgeKeyReconciler(&fakeNudgeCommandPager{}, nudgequeue.CommandStoreBinding{StoreUUID: "store-1", RestoreEpoch: 8}, 1)
	if err != nil {
		t.Fatalf("newNudgeKeyReconciler other epoch: %v", err)
	}
	if otherEpoch.keyScope == reconciler.keyScope {
		t.Fatalf("restore epoch replacement reused key scope %q", reconciler.keyScope)
	}
}

func TestNudgeKeyReconcileResultCannotCarryIdentityOrContent(t *testing.T) {
	assertNudgeKeyResultTypeIsBounded(t, reflect.TypeOf(nudgeKeyReconcilePageResult{}))
}

func assertNudgeKeyResultTypeIsBounded(t *testing.T, typ reflect.Type) {
	t.Helper()
	switch typ.Kind() {
	case reflect.Bool, reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64,
		reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
		return
	case reflect.Struct:
		for i := 0; i < typ.NumField(); i++ {
			assertNudgeKeyResultTypeIsBounded(t, typ.Field(i).Type)
		}
	default:
		t.Fatalf("keyed nudge result type %s contains identity/content-capable kind %s", typ, typ.Kind())
	}
}

func TestNewNudgeKeyReconcilerRejectsIncompleteDependencies(t *testing.T) {
	validStore := nudgequeue.CommandStoreBinding{StoreUUID: "store-1", RestoreEpoch: 7}
	tests := []struct {
		name  string
		pager nudgeCommandPager
		store nudgequeue.CommandStoreBinding
		limit int
	}{
		{name: "nil pager", store: validStore, limit: 1},
		{name: "missing store uuid", pager: &fakeNudgeCommandPager{}, store: nudgequeue.CommandStoreBinding{RestoreEpoch: 1}, limit: 1},
		{name: "noncanonical store uuid", pager: &fakeNudgeCommandPager{}, store: nudgequeue.CommandStoreBinding{StoreUUID: " store", RestoreEpoch: 1}, limit: 1},
		{name: "control store uuid", pager: &fakeNudgeCommandPager{}, store: nudgequeue.CommandStoreBinding{StoreUUID: "store\x00", RestoreEpoch: 1}, limit: 1},
		{name: "invalid utf8 store uuid", pager: &fakeNudgeCommandPager{}, store: nudgequeue.CommandStoreBinding{StoreUUID: string([]byte{0xff}), RestoreEpoch: 1}, limit: 1},
		{name: "overlong store uuid", pager: &fakeNudgeCommandPager{}, store: nudgequeue.CommandStoreBinding{StoreUUID: strings.Repeat("s", 257), RestoreEpoch: 1}, limit: 1},
		{name: "missing restore epoch", pager: &fakeNudgeCommandPager{}, store: nudgequeue.CommandStoreBinding{StoreUUID: "store"}, limit: 1},
		{name: "zero page limit", pager: &fakeNudgeCommandPager{}, store: validStore},
		{name: "overlarge page limit", pager: &fakeNudgeCommandPager{}, store: validStore, limit: nudgequeue.MaxCommandIndexPageSize + 1},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := newNudgeKeyReconciler(tc.pager, tc.store, tc.limit); err == nil {
				t.Fatal("newNudgeKeyReconciler error = nil")
			}
		})
	}
}

func TestNudgeKeyReconcilerReadsOneBoundedPageForExactStableKey(t *testing.T) {
	store := nudgequeue.CommandStoreBinding{StoreUUID: "store-1", RestoreEpoch: 7}
	key := mustNudgeCommandReconcileKey(t, store)
	pager := &fakeNudgeCommandPager{page: nudgequeue.CommandIndexPage{
		Store:                  store,
		Revision:               20,
		CompletedAuditRevision: 18,
		Entries: []nudgequeue.CommandIndexEntry{
			knownPageEntry(pageTestCommand("command-11", "session-a", 11, 17, nudgequeue.CommandStatePending, store)),
			knownPageEntry(pageTestCommand("command-12", "session-a", 12, 19, nudgequeue.CommandStateInFlight, store)),
			knownPageEntry(pageTestCommand("command-13", "session-a", 13, 20, nudgequeue.CommandStateUpgradeRequired, store)),
		},
	}}
	reconciler, err := newNudgeKeyReconciler(pager, store, 3)
	if err != nil {
		t.Fatalf("newNudgeKeyReconciler: %v", err)
	}

	result, err := reconciler.Reconcile(context.Background(), key)
	if err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if pager.calls != 1 || pager.session != "session-a" || pager.after != 0 || pager.limit != 3 {
		t.Fatalf("Page calls = %d session=%q after=%d limit=%d, want one exact bounded read", pager.calls, pager.session, pager.after, pager.limit)
	}
	if result.Disposition != nudgeKeyPageEvaluated || result.Revision != 20 || result.CompletedAuditRevision != 18 {
		t.Fatalf("result watermark = %#v", result)
	}
	if result.Evaluated != 3 || result.Pending != 1 || result.InFlight != 1 || result.KnownUpgradeRequired != 1 || result.OpaqueUpgradeRequired != 0 {
		t.Fatalf("result counts = %#v", result)
	}
	if result.FirstSequence != 11 || result.LastSequence != 13 {
		t.Fatalf("result sequence bounds = %d..%d, want 11..13", result.FirstSequence, result.LastSequence)
	}
	if result.Continuation.Required {
		t.Fatalf("upgrade-required barrier requested continuation: %#v", result.Continuation)
	}
}

func TestNudgeKeyReconcilerTreatsOpaqueEntryAsAnOrderingBarrier(t *testing.T) {
	store := nudgequeue.CommandStoreBinding{StoreUUID: "store-1", RestoreEpoch: 7}
	key := mustNudgeCommandReconcileKey(t, store)
	pager := &fakeNudgeCommandPager{page: nudgequeue.CommandIndexPage{
		Store:                  store,
		Revision:               2,
		CompletedAuditRevision: 2,
		Entries: []nudgequeue.CommandIndexEntry{
			knownPageEntry(pageTestCommand("command-1", "session-a", 1, 1, nudgequeue.CommandStatePending, store)),
			pageTestOpaqueEntry(t, "command-2", "session-a", 2, 2, store),
		},
	}}
	reconciler, err := newNudgeKeyReconciler(pager, store, 8)
	if err != nil {
		t.Fatalf("newNudgeKeyReconciler: %v", err)
	}

	result, err := reconciler.Reconcile(context.Background(), key)
	if err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if result.Evaluated != 2 || result.Pending != 1 || result.KnownUpgradeRequired != 0 || result.OpaqueUpgradeRequired != 1 {
		t.Fatalf("opaque barrier result = %#v", result)
	}
	if result.LastSequence != 2 || result.Continuation.Required {
		t.Fatalf("opaque barrier ordering = %#v", result)
	}
}

func TestNudgeKeyReconcilerUsesTheRealCommandIndexPagerContract(t *testing.T) {
	store := nudgequeue.CommandStoreBinding{StoreUUID: "store-1", RestoreEpoch: 7}
	index, err := nudgequeue.BuildCommandIndex(nudgequeue.CommandIndexSnapshot{
		Store: store,
		Entries: []nudgequeue.CommandIndexEntry{
			knownPageEntry(pageTestCommand("command-1", "session-a", 1, 1, nudgequeue.CommandStatePending, store)),
			pageTestOpaqueEntry(t, "command-2", "session-a", 2, 2, store),
		},
		Revision:          2,
		SequenceHighWater: 2,
	})
	if err != nil {
		t.Fatalf("BuildCommandIndex: %v", err)
	}
	reconciler, err := newNudgeKeyReconciler(index, store, 8)
	if err != nil {
		t.Fatalf("newNudgeKeyReconciler: %v", err)
	}

	result, err := reconciler.Reconcile(context.Background(), mustNudgeCommandReconcileKey(t, store))
	if err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if result.Evaluated != 2 || result.Pending != 1 || result.OpaqueUpgradeRequired != 1 || result.Continuation.Required {
		t.Fatalf("real index result = %#v", result)
	}
}

func TestNudgeKeyReconcilerKnownFixturesSatisfyTheCodecContract(t *testing.T) {
	store := nudgequeue.CommandStoreBinding{StoreUUID: "store-1", RestoreEpoch: 7}
	for _, state := range []nudgequeue.CommandState{
		nudgequeue.CommandStatePending,
		nudgequeue.CommandStateInFlight,
		nudgequeue.CommandStateUpgradeRequired,
	} {
		t.Run(string(state), func(t *testing.T) {
			command := pageTestCommand("command-1", "session-a", 1, 1, state, store)
			if _, err := nudgequeue.EncodeCommandV1(command); err != nil {
				t.Fatalf("page fixture is not codec-valid: %v", err)
			}
		})
	}
}

func TestNudgeKeyReconcilerExplicitContinuationReadsLatestPage(t *testing.T) {
	store := nudgequeue.CommandStoreBinding{StoreUUID: "store-1", RestoreEpoch: 7}
	key := mustNudgeCommandReconcileKey(t, store)
	pager := &fakeNudgeCommandPager{page: nudgequeue.CommandIndexPage{
		Store:                  store,
		Revision:               10,
		CompletedAuditRevision: 9,
		Entries: []nudgequeue.CommandIndexEntry{
			knownPageEntry(pageTestCommand("command-1", "session-a", 1, 10, nudgequeue.CommandStatePending, store)),
		},
	}}
	reconciler, err := newNudgeKeyReconciler(pager, store, 4)
	if err != nil {
		t.Fatalf("newNudgeKeyReconciler: %v", err)
	}
	first, err := reconciler.Reconcile(context.Background(), key)
	if err != nil {
		t.Fatalf("first Reconcile: %v", err)
	}
	if first.Revision != 10 {
		t.Fatalf("first revision = %d, want 10", first.Revision)
	}

	pager.page = nudgequeue.CommandIndexPage{
		Store:                  store,
		Revision:               12,
		CompletedAuditRevision: 12,
		Entries: []nudgequeue.CommandIndexEntry{
			knownPageEntry(pageTestCommand("command-8", "session-a", 8, 11, nudgequeue.CommandStatePending, store)),
			knownPageEntry(pageTestCommand("command-9", "session-a", 9, 12, nudgequeue.CommandStatePending, store)),
		},
	}
	second, err := reconciler.ReconcilePage(context.Background(), key, 7)
	if err != nil {
		t.Fatalf("ReconcilePage: %v", err)
	}
	if pager.calls != 2 || pager.after != 7 {
		t.Fatalf("latest page calls = %d after=%d, want second read after 7", pager.calls, pager.after)
	}
	if second.Revision != 12 || second.Evaluated != 2 || second.FirstSequence != 8 || second.LastSequence != 9 {
		t.Fatalf("second result = %#v, want latest revision 12", second)
	}
	if second.Continuation.Required {
		t.Fatalf("final page requested continuation: %#v", second.Continuation)
	}
}

func TestNudgeKeyReconcilerTreatsUnsyncedProjectionAsAuditOnly(t *testing.T) {
	store := nudgequeue.CommandStoreBinding{StoreUUID: "store-1", RestoreEpoch: 7}
	key := mustNudgeCommandReconcileKey(t, store)
	pager := &fakeNudgeCommandPager{err: fmt.Errorf("gap: %w", nudgequeue.ErrCommandIndexUnsynced)}
	reconciler, err := newNudgeKeyReconciler(pager, store, 8)
	if err != nil {
		t.Fatalf("newNudgeKeyReconciler: %v", err)
	}

	result, err := reconciler.Reconcile(context.Background(), key)
	if err != nil {
		t.Fatalf("Reconcile unsynced: %v", err)
	}
	if result.Disposition != nudgeKeyPageAuditNeeded || result.Evaluated != 0 || result.Continuation.Required {
		t.Fatalf("unsynced result = %#v, want audit-only", result)
	}
}

func TestNudgeKeyReconcilerFailsClosedOnScopeLineageAndPageInvariantErrors(t *testing.T) {
	store := nudgequeue.CommandStoreBinding{StoreUUID: "store-1", RestoreEpoch: 7}
	key := mustNudgeCommandReconcileKey(t, store)
	foreignStore := nudgequeue.CommandStoreBinding{StoreUUID: "store-2", RestoreEpoch: 7}
	tests := []struct {
		name string
		key  reconcilekey.Session
		page nudgequeue.CommandIndexPage
		err  error
	}{
		{
			name: "wrong key scope",
			key:  mustNudgeReconcileKey(t, "other-scope", "session-a"),
			page: validNudgeCommandPage(store),
		},
		{
			name: "foreign page lineage",
			key:  key,
			page: nudgequeue.CommandIndexPage{Store: foreignStore, Revision: 1, CompletedAuditRevision: 1},
		},
		{
			name: "audit ahead of projection",
			key:  key,
			page: nudgequeue.CommandIndexPage{Store: store, Revision: 1, CompletedAuditRevision: 2},
		},
		{
			name: "foreign command target",
			key:  key,
			page: nudgequeue.CommandIndexPage{
				Store: store, Revision: 1, CompletedAuditRevision: 1,
				Entries: []nudgequeue.CommandIndexEntry{knownPageEntry(pageTestCommand("command-1", "session-b", 1, 1, nudgequeue.CommandStatePending, store))},
			},
		},
		{
			name: "foreign command lineage",
			key:  key,
			page: nudgequeue.CommandIndexPage{
				Store: store, Revision: 1, CompletedAuditRevision: 1,
				Entries: []nudgequeue.CommandIndexEntry{knownPageEntry(pageTestCommand("command-1", "session-a", 1, 1, nudgequeue.CommandStatePending, foreignStore))},
			},
		},
		{
			name: "terminal command leaked into active page",
			key:  key,
			page: nudgequeue.CommandIndexPage{
				Store: store, Revision: 1, CompletedAuditRevision: 1,
				Entries: []nudgequeue.CommandIndexEntry{knownPageEntry(pageTestCommand("command-1", "session-a", 1, 1, nudgequeue.CommandStateDelivered, store))},
			},
		},
		{
			name: "entry has neither tagged arm",
			key:  key,
			page: nudgequeue.CommandIndexPage{
				Store: store, Revision: 1, CompletedAuditRevision: 1,
				Entries: []nudgequeue.CommandIndexEntry{{}},
			},
		},
		{
			name: "entry has both tagged arms",
			key:  key,
			page: nudgequeue.CommandIndexPage{
				Store: store, Revision: 1, CompletedAuditRevision: 1,
				Entries: []nudgequeue.CommandIndexEntry{func() nudgequeue.CommandIndexEntry {
					entry := pageTestOpaqueEntry(t, "command-1", "session-a", 1, 1, store)
					command := pageTestCommand("command-1", "session-a", 1, 1, nudgequeue.CommandStatePending, store)
					entry.Command = &command
					return entry
				}()},
			},
		},
		{
			name: "opaque arm does not name a future version",
			key:  key,
			page: nudgequeue.CommandIndexPage{
				Store: store, Revision: 1, CompletedAuditRevision: 1,
				Entries: []nudgequeue.CommandIndexEntry{func() nudgequeue.CommandIndexEntry {
					entry := pageTestOpaqueEntry(t, "command-1", "session-a", 1, 1, store)
					entry.Opaque.Version = nudgequeue.CommandVersion1
					return entry
				}()},
			},
		},
		{
			name: "foreign opaque target",
			key:  key,
			page: nudgequeue.CommandIndexPage{
				Store: store, Revision: 1, CompletedAuditRevision: 1,
				Entries: []nudgequeue.CommandIndexEntry{pageTestOpaqueEntry(t, "command-1", "session-b", 1, 1, store)},
			},
		},
		{
			name: "foreign opaque lineage",
			key:  key,
			page: nudgequeue.CommandIndexPage{
				Store: store, Revision: 1, CompletedAuditRevision: 1,
				Entries: []nudgequeue.CommandIndexEntry{pageTestOpaqueEntry(t, "command-1", "session-a", 1, 1, foreignStore)},
			},
		},
		{
			name: "opaque revision ahead of page",
			key:  key,
			page: nudgequeue.CommandIndexPage{
				Store: store, Revision: 1, CompletedAuditRevision: 1,
				Entries: []nudgequeue.CommandIndexEntry{pageTestOpaqueEntry(t, "command-1", "session-a", 1, 2, store)},
			},
		},
		{
			name: "entry follows opaque barrier",
			key:  key,
			page: nudgequeue.CommandIndexPage{
				Store: store, Revision: 2, CompletedAuditRevision: 2,
				Entries: []nudgequeue.CommandIndexEntry{
					pageTestOpaqueEntry(t, "command-1", "session-a", 1, 1, store),
					knownPageEntry(pageTestCommand("command-2", "session-a", 2, 2, nudgequeue.CommandStatePending, store)),
				},
			},
		},
		{
			name: "entry follows known upgrade barrier",
			key:  key,
			page: nudgequeue.CommandIndexPage{
				Store: store, Revision: 2, CompletedAuditRevision: 2,
				Entries: []nudgequeue.CommandIndexEntry{
					knownPageEntry(pageTestCommand("command-1", "session-a", 1, 1, nudgequeue.CommandStateUpgradeRequired, store)),
					knownPageEntry(pageTestCommand("command-2", "session-a", 2, 2, nudgequeue.CommandStatePending, store)),
				},
			},
		},
		{
			name: "continuation crosses known upgrade barrier",
			key:  key,
			page: nudgequeue.CommandIndexPage{
				Store: store, Revision: 1, CompletedAuditRevision: 1,
				Entries: []nudgequeue.CommandIndexEntry{
					knownPageEntry(pageTestCommand("command-1", "session-a", 1, 1, nudgequeue.CommandStateUpgradeRequired, store)),
				},
				NextAfterSequence: 1,
			},
		},
		{
			name: "continuation crosses opaque upgrade barrier",
			key:  key,
			page: nudgequeue.CommandIndexPage{
				Store: store, Revision: 1, CompletedAuditRevision: 1,
				Entries: []nudgequeue.CommandIndexEntry{
					pageTestOpaqueEntry(t, "command-1", "session-a", 1, 1, store),
				},
				NextAfterSequence: 1,
			},
		},
		{
			name: "non-increasing sequence",
			key:  key,
			page: nudgequeue.CommandIndexPage{
				Store: store, Revision: 2, CompletedAuditRevision: 2,
				Entries: []nudgequeue.CommandIndexEntry{
					knownPageEntry(pageTestCommand("command-2", "session-a", 2, 1, nudgequeue.CommandStatePending, store)),
					knownPageEntry(pageTestCommand("command-1", "session-a", 1, 2, nudgequeue.CommandStatePending, store)),
				},
			},
		},
		{
			name: "command revision ahead of page",
			key:  key,
			page: nudgequeue.CommandIndexPage{
				Store: store, Revision: 1, CompletedAuditRevision: 1,
				Entries: []nudgequeue.CommandIndexEntry{knownPageEntry(pageTestCommand("command-1", "session-a", 1, 2, nudgequeue.CommandStatePending, store))},
			},
		},
		{
			name: "continuation without commands",
			key:  key,
			page: nudgequeue.CommandIndexPage{Store: store, Revision: 1, CompletedAuditRevision: 1, NextAfterSequence: 1},
		},
		{
			name: "continuation cursor differs from last command",
			key:  key,
			page: nudgequeue.CommandIndexPage{
				Store: store, Revision: 2, CompletedAuditRevision: 2,
				Entries: []nudgequeue.CommandIndexEntry{
					knownPageEntry(pageTestCommand("command-1", "session-a", 1, 1, nudgequeue.CommandStatePending, store)),
					knownPageEntry(pageTestCommand("command-2", "session-a", 2, 2, nudgequeue.CommandStatePending, store)),
				},
				NextAfterSequence: 1,
			},
		},
		{
			name: "ordinary pager error",
			key:  key,
			err:  errors.New("reader unavailable"),
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			pager := &fakeNudgeCommandPager{page: tc.page, err: tc.err}
			reconciler, err := newNudgeKeyReconciler(pager, store, 2)
			if err != nil {
				t.Fatalf("newNudgeKeyReconciler: %v", err)
			}
			if _, err := reconciler.Reconcile(context.Background(), tc.key); err == nil {
				t.Fatal("Reconcile error = nil, want fail-closed invariant error")
			}
		})
	}
}

func TestNudgeKeyReconcilerRejectsOverfullPageAndHonorsCancellation(t *testing.T) {
	store := nudgequeue.CommandStoreBinding{StoreUUID: "store-1", RestoreEpoch: 7}
	key := mustNudgeCommandReconcileKey(t, store)
	overfull := []nudgequeue.CommandIndexEntry{
		knownPageEntry(pageTestCommand("command-1", "session-a", 1, 1, nudgequeue.CommandStatePending, store)),
		knownPageEntry(pageTestCommand("command-2", "session-a", 2, 2, nudgequeue.CommandStatePending, store)),
	}
	pager := &fakeNudgeCommandPager{page: nudgequeue.CommandIndexPage{
		Store: store, Revision: 2, CompletedAuditRevision: 2, Entries: overfull,
	}}
	reconciler, err := newNudgeKeyReconciler(pager, store, 1)
	if err != nil {
		t.Fatalf("newNudgeKeyReconciler: %v", err)
	}
	if _, err := reconciler.Reconcile(context.Background(), key); err == nil {
		t.Fatal("overfull page error = nil")
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	pager.calls = 0
	if _, err := reconciler.Reconcile(ctx, key); !errors.Is(err, context.Canceled) {
		t.Fatalf("pre-read cancellation error = %v, want context.Canceled", err)
	}
	if pager.calls != 0 {
		t.Fatalf("canceled reconcile performed %d page reads", pager.calls)
	}

	ctx, cancel = context.WithCancel(context.Background())
	pager.page.Entries = pager.page.Entries[:1]
	pager.onPage = cancel
	if _, err := reconciler.Reconcile(ctx, key); !errors.Is(err, context.Canceled) {
		t.Fatalf("post-read cancellation error = %v, want context.Canceled", err)
	}

	ctx, cancel = context.WithCancel(context.Background())
	pager.err = nudgequeue.ErrCommandIndexUnsynced
	pager.onPage = cancel
	if _, err := reconciler.Reconcile(ctx, key); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled failed-read error = %v, want context.Canceled to win", err)
	}
}

func TestNudgeKeyReconcilerWorkIsBoundedByPageNotFleetSize(t *testing.T) {
	const fleetCommands = 100_000
	store := nudgequeue.CommandStoreBinding{StoreUUID: "store-1", RestoreEpoch: 7}
	key := mustNudgeCommandReconcileKey(t, store)
	const pageLimit = 32
	pager := &countingNudgeCommandPager{
		store:        store,
		revision:     fleetCommands,
		commandCount: fleetCommands,
	}
	reconciler, err := newNudgeKeyReconciler(pager, store, pageLimit)
	if err != nil {
		t.Fatalf("newNudgeKeyReconciler: %v", err)
	}
	result, err := reconciler.Reconcile(context.Background(), key)
	if err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if pager.calls != 1 || pager.visited != pageLimit+1 {
		t.Fatalf("pager calls/visits = %d/%d, want 1/%d independent of %d-command fleet", pager.calls, pager.visited, pageLimit+1, fleetCommands)
	}
	if result.Evaluated != pageLimit || !result.Continuation.Required || result.Continuation.AfterSequence != pageLimit {
		t.Fatalf("bounded result = %#v", result)
	}
}

type countingNudgeCommandPager struct {
	store        nudgequeue.CommandStoreBinding
	revision     uint64
	commandCount uint64
	calls        int
	visited      int
}

func (p *countingNudgeCommandPager) Page(sessionID string, afterSequence uint64, limit int) (nudgequeue.CommandIndexPage, error) {
	p.calls++
	page := nudgequeue.CommandIndexPage{
		Store:                  p.store,
		Revision:               p.revision,
		CompletedAuditRevision: p.revision,
	}
	for sequence := afterSequence + 1; sequence <= p.commandCount && len(page.Entries) < limit; sequence++ {
		p.visited++
		command := pageTestCommand(fmt.Sprintf("command-%d", sequence), sessionID, sequence, sequence, nudgequeue.CommandStatePending, p.store)
		page.Entries = append(page.Entries, knownPageEntry(command))
	}
	if uint64(len(page.Entries))+afterSequence < p.commandCount {
		p.visited++ // one bounded lookahead proves continuation exists
		page.NextAfterSequence = page.Entries[len(page.Entries)-1].Command.Order.Sequence
	}
	return page, nil
}

func mustNudgeReconcileKey(t *testing.T, scope, sessionID string) reconcilekey.Session {
	t.Helper()
	key, err := reconcilekey.NewSession(scope, sessionID)
	if err != nil {
		t.Fatalf("reconcilekey.NewSession: %v", err)
	}
	return key
}

func mustNudgeCommandReconcileKey(t *testing.T, store nudgequeue.CommandStoreBinding) reconcilekey.Session {
	t.Helper()
	return mustNudgeReconcileKey(t, nudgeCommandReconcileScope(store), "session-a")
}

func pageTestCommand(id, sessionID string, sequence, revision uint64, state nudgequeue.CommandState, store nudgequeue.CommandStoreBinding) nudgequeue.Command {
	created := time.Date(2026, 7, 15, 10, 0, 0, 0, time.UTC)
	command := nudgequeue.Command{
		Version: nudgequeue.CommandVersion1,
		ID:      id,
		State:   state,
		Mode:    nudgequeue.DeliveryModeQueue,
		Target: nudgequeue.CommandTarget{
			SessionID:            sessionID,
			IntentGeneration:     1,
			ContinuationIdentity: "continuation-1",
			Policy:               nudgequeue.TargetPolicyContinuation,
		},
		Store: store,
		Order: nudgequeue.CommandOrder{Sequence: sequence, Revision: revision},
		TrustedIngress: nudgequeue.TrustedIngressReference{
			Issuer:           "local-ingress",
			ReferenceID:      "ingress-1",
			PrincipalID:      "principal-1",
			TenantScope:      "tenant-1",
			CityScope:        "city-1",
			CredentialClass:  "controller-ingress",
			PolicyVersion:    "policy-v1",
			PolicyDecisionID: "decision-1",
			Action:           nudgequeue.NudgeCommandAction,
			TargetSessionID:  sessionID,
			IssuedAt:         created.Add(-time.Minute),
			ExpiresAt:        created.Add(10 * time.Minute),
		},
		Source:       nudgequeue.CommandSourceSession,
		Message:      "wake up",
		CreatedAt:    created,
		DeliverAfter: created.Add(time.Second),
		ExpiresAt:    created.Add(time.Hour),
	}
	command.TrustedIngress.PayloadDigest = nudgequeue.ComputeCommandPayloadDigest(command)
	if state == nudgequeue.CommandStateInFlight {
		claimedAt := created.Add(2 * time.Second)
		claim := &nudgequeue.CommandClaim{
			ID:                         "claim-1",
			OwnerID:                    "controller-1",
			OperationID:                id,
			AttemptID:                  "attempt-1",
			BoundLaunchIdentity:        "launch-1",
			AuthorizationDecisionID:    "claim-decision-1",
			AuthorizationPolicyVersion: "policy-v2",
			ClaimedAt:                  claimedAt,
			LeaseUntil:                 created.Add(time.Minute),
		}
		command.Binding = &nudgequeue.CommandBinding{LaunchIdentity: claim.BoundLaunchIdentity, BoundAt: claimedAt}
		command.Retry = &nudgequeue.CommandRetry{
			AttemptCount:               1,
			LastAttemptAt:              claimedAt,
			ClaimID:                    claim.ID,
			OperationID:                claim.OperationID,
			AttemptID:                  claim.AttemptID,
			BoundLaunchIdentity:        claim.BoundLaunchIdentity,
			AuthorizationDecisionID:    claim.AuthorizationDecisionID,
			AuthorizationPolicyVersion: claim.AuthorizationPolicyVersion,
		}
		command.Claim = claim
	}
	return command
}

func knownPageEntry(command nudgequeue.Command) nudgequeue.CommandIndexEntry {
	return nudgequeue.CommandIndexEntry{Command: &command}
}

func pageTestOpaqueEntry(t *testing.T, id, sessionID string, sequence, revision uint64, store nudgequeue.CommandStoreBinding) nudgequeue.CommandIndexEntry {
	t.Helper()
	wire := []byte(fmt.Sprintf(
		`{"version":2,"id":%q,"store":{"store_uuid":%q,"restore_epoch":%d},"target":{"session_id":%q,"intent_generation":1},"order":{"sequence":%d,"revision":%d},"future":{"preserve":true}}`,
		id,
		store.StoreUUID,
		store.RestoreEpoch,
		sessionID,
		sequence,
		revision,
	))
	decoded := nudgequeue.DecodeCommand(wire)
	if decoded.Disposition != nudgequeue.CommandDecodeUpgradeRequired {
		t.Fatalf("DecodeCommand future fixture = %#v", decoded)
	}
	return nudgequeue.CommandIndexEntry{Opaque: &nudgequeue.OpaqueCommand{
		Version: decoded.Version,
		Routing: decoded.Routing,
		Raw:     decoded.Raw,
	}}
}

func validNudgeCommandPage(store nudgequeue.CommandStoreBinding) nudgequeue.CommandIndexPage {
	return nudgequeue.CommandIndexPage{Store: store, Revision: 1, CompletedAuditRevision: 1}
}
