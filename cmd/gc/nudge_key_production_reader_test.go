package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/nudgequeue"
	"github.com/gastownhall/gascity/internal/reconcilekey"
)

type fakeNudgeCommandSource struct {
	mu sync.Mutex

	snapshot       nudgequeue.CommandIndexSnapshot
	snapshotErr    error
	snapshotCalls  int
	snapshotLimits []int
	onSnapshot     func(context.Context) error

	resolutions  map[string]nudgequeue.CommandIndexResolution
	getErr       error
	getCalls     []string
	transientErr error
}

func (f *fakeNudgeCommandSource) ClassifyNudgeCommandSourceError(err error) nudgeCommandSourceErrorClass {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.transientErr != nil && errors.Is(err, f.transientErr) {
		return nudgeCommandSourceErrorTransient
	}
	return nudgeCommandSourceErrorInvariant
}

func (f *fakeNudgeCommandSource) Snapshot(ctx context.Context, limit int) (nudgequeue.CommandIndexSnapshot, error) {
	f.mu.Lock()
	f.snapshotCalls++
	f.snapshotLimits = append(f.snapshotLimits, limit)
	onSnapshot := f.onSnapshot
	snapshot := f.snapshot
	err := f.snapshotErr
	f.mu.Unlock()
	if onSnapshot != nil {
		if callbackErr := onSnapshot(ctx); callbackErr != nil {
			return nudgequeue.CommandIndexSnapshot{}, callbackErr
		}
	}
	return snapshot, err
}

func (f *fakeNudgeCommandSource) Get(_ context.Context, commandID string) (nudgequeue.CommandIndexResolution, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.getCalls = append(f.getCalls, commandID)
	return f.resolutions[commandID], f.getErr
}

func (f *fakeNudgeCommandSource) snapshotCallCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.snapshotCalls
}

func (f *fakeNudgeCommandSource) exactGetCalls() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.getCalls...)
}

func (f *fakeNudgeCommandSource) setSnapshot(snapshot nudgequeue.CommandIndexSnapshot) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.snapshot = snapshot
	f.snapshotErr = nil
}

func (f *fakeNudgeCommandSource) setOnSnapshot(callback func(context.Context) error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.onSnapshot = callback
}

func (f *fakeNudgeCommandSource) setGetError(err error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.getErr = err
}

type recordingNudgePageCursor struct {
	delegate nudgeCommandPager
	after    []uint64
}

func (p *recordingNudgePageCursor) Page(sessionID string, afterSequence uint64, limit int) (nudgequeue.CommandIndexPage, error) {
	p.after = append(p.after, afterSequence)
	return p.delegate.Page(sessionID, afterSequence, limit)
}

func TestInstallNudgeKeyReaderBuildsSnapshotBeforePublishingController(t *testing.T) {
	storeBinding := nudgequeue.CommandStoreBinding{StoreUUID: "store-production", RestoreEpoch: 9}
	command := pageTestCommand("command-startup", "session-startup", 1, 1, nudgequeue.CommandStatePending, storeBinding)
	source := &fakeNudgeCommandSource{
		snapshot: nudgequeue.CommandIndexSnapshot{
			Store:             storeBinding,
			Entries:           []nudgequeue.CommandIndexEntry{knownPageEntry(command)},
			Revision:          1,
			SequenceHighWater: 1,
		},
	}
	cityStore := beads.NewMemStore()
	var cr *CityRuntime
	source.onSnapshot = func(context.Context) error {
		if cr.nudgeKeyController != nil || cr.nudgeKeyReader != nil {
			t.Fatal("keyed controller became visible before the consistent startup snapshot completed")
		}
		return nil
	}
	cr = &CityRuntime{
		cityPath:            t.TempDir(),
		cfg:                 supervisorCfg(),
		standaloneCityStore: cityStore,
		stderr:              &bytes.Buffer{},
		nudgeCommandSourceOpener: func(
			_ context.Context,
			gotCityPath string,
			got beads.Store,
			partition nudgequeue.TrustedCityPartition,
			resolver nudgequeue.TrustedCityPartitionResolver,
		) (nudgeCommandSource, error) {
			if gotCityPath != cr.cityPath {
				t.Fatalf("source opener city path = %q, want %q", gotCityPath, cr.cityPath)
			}
			if got != cityStore {
				t.Fatalf("source opener store = %T %p, want city store %T %p", got, got, cityStore, cityStore)
			}
			if partition != (nudgequeue.TrustedCityPartition{}) || resolver != nil {
				t.Fatalf("source opener authority = (%#v, %T), want explicit unverified zero values", partition, resolver)
			}
			return source, nil
		},
	}

	if err := cr.installNudgeKeyShadow(t.Context()); err != nil {
		t.Fatalf("installNudgeKeyShadow: %v", err)
	}
	if source.snapshotCallCount() != 1 {
		t.Fatalf("startup snapshot calls = %d, want 1", source.snapshotCallCount())
	}
	if cr.nudgeKeyController == nil || cr.nudgeKeyReader == nil {
		t.Fatalf("verified source installed controller=%v reader=%v, want both", cr.nudgeKeyController != nil, cr.nudgeKeyReader != nil)
	}
	if got, want := cr.nudgeKeyShadowScope, nudgeCommandReconcileScope(storeBinding); got != want {
		t.Fatalf("installed scope = %q, want actual command-store scope %q", got, want)
	}
	startupKey := mustNudgeReconcileKey(t, cr.nudgeKeyShadowScope, command.Target.SessionID)
	cr.nudgeKeyController.mu.Lock()
	startupBatch, startupAdmitted := cr.nudgeKeyController.pending[startupKey]
	cr.nudgeKeyController.mu.Unlock()
	if !startupAdmitted || startupBatch.Causes != nudgeCauseCommandCommit {
		t.Fatalf("startup active-key admission = (%v, %#v), want command-commit admission before start", startupAdmitted, startupBatch)
	}
	resolved, err := cr.nudgeKeyReader.index.Resolve(command.ID)
	if err != nil {
		t.Fatalf("Resolve startup command: %v", err)
	}
	if !resolved.Found || resolved.Entry.Command == nil || resolved.Entry.Command.ID != command.ID {
		t.Fatalf("startup index resolution = %#v, want command %q", resolved, command.ID)
	}
	if got := source.exactGetCalls(); len(got) != 0 {
		t.Fatalf("startup exact Get calls = %v, want snapshot-only construction", got)
	}
}

func TestInstallNudgeKeyReaderUnsupportedOrUnverifiedBackendStaysLegacyOnlyAndWarnsOnce(t *testing.T) {
	var stderr bytes.Buffer
	opens := 0
	cr := &CityRuntime{
		cityPath:            t.TempDir(),
		cfg:                 supervisorCfg(),
		standaloneCityStore: beads.NewMemStore(),
		stderr:              &stderr,
		nudgeCommandSourceOpener: func(context.Context, string, beads.Store, nudgequeue.TrustedCityPartition, nudgequeue.TrustedCityPartitionResolver) (nudgeCommandSource, error) {
			opens++
			return nil, errNudgeCommandSourceUnverified
		},
	}

	for i := 0; i < 2; i++ {
		if err := cr.installNudgeKeyShadow(t.Context()); err != nil {
			t.Fatalf("installNudgeKeyShadow call %d: %v", i+1, err)
		}
	}
	if opens != 2 {
		t.Fatalf("source open attempts = %d, want 2 idempotent probes", opens)
	}
	if cr.nudgeKeyController != nil || cr.nudgeKeyReader != nil || cr.nudgeKeyShadowScope != "" {
		t.Fatalf("unverified backend installed controller=%v reader=%v scope=%q", cr.nudgeKeyController != nil, cr.nudgeKeyReader != nil, cr.nudgeKeyShadowScope)
	}
	const diagnostic = "verified durable nudge command source unavailable; legacy dispatcher remains sole effect owner"
	if got := strings.Count(stderr.String(), diagnostic); got != 1 {
		t.Fatalf("diagnostic occurrences = %d, want 1; stderr=%q", got, stderr.String())
	}
}

func TestInstallNudgeKeyReaderIsSupervisorModeOnly(t *testing.T) {
	cfg := supervisorCfg()
	cfg.Daemon.NudgeDispatcher = "legacy"
	opens := 0
	cr := &CityRuntime{
		cityPath: t.TempDir(),
		cfg:      cfg,
		stderr:   &bytes.Buffer{},
		nudgeCommandSourceOpener: func(context.Context, string, beads.Store, nudgequeue.TrustedCityPartition, nudgequeue.TrustedCityPartitionResolver) (nudgeCommandSource, error) {
			opens++
			return nil, errors.New("must not be called")
		},
	}
	if err := cr.installNudgeKeyShadow(t.Context()); err != nil {
		t.Fatalf("legacy install: %v", err)
	}
	if opens != 0 || cr.nudgeKeyController != nil || cr.nudgeKeyReader != nil {
		t.Fatalf("legacy mode opened=%d controller=%v reader=%v, want inert", opens, cr.nudgeKeyController != nil, cr.nudgeKeyReader != nil)
	}
}

func TestInstallNudgeKeyReaderHonorsCancellationBeforeOpeningRepository(t *testing.T) {
	opens := 0
	cr := &CityRuntime{
		cityPath: t.TempDir(),
		cfg:      supervisorCfg(),
		stderr:   &bytes.Buffer{},
		nudgeCommandSourceOpener: func(context.Context, string, beads.Store, nudgequeue.TrustedCityPartition, nudgequeue.TrustedCityPartitionResolver) (nudgeCommandSource, error) {
			opens++
			return nil, errors.New("must not be called")
		},
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := cr.installNudgeKeyShadow(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled install error = %v, want context.Canceled", err)
	}
	if opens != 0 {
		t.Fatalf("canceled install opened repository %d time(s), want zero", opens)
	}
}

func TestNudgeKeyExactHintUsesCommandIDAndDurableTargetAuthority(t *testing.T) {
	storeBinding := nudgequeue.CommandStoreBinding{StoreUUID: "store-authority", RestoreEpoch: 3}
	command := pageTestCommand("command-authority", "durable-session", 1, 1, nudgequeue.CommandStatePending, storeBinding)
	source := &fakeNudgeCommandSource{
		snapshot: nudgequeue.CommandIndexSnapshot{Store: storeBinding},
		resolutions: map[string]nudgequeue.CommandIndexResolution{
			command.ID: {
				Store:                  storeBinding,
				Revision:               1,
				CompletedAuditRevision: 0,
				Entry:                  knownPageEntry(command),
				Found:                  true,
			},
		},
	}
	cr := newInstalledNudgeKeyReaderForTest(t, source)

	cr.acceptNudgeKeyShadowHint(t.Context(), nudgeWakeHint{
		Version:   nudgequeue.SessionWakeHintVersion1,
		CommandID: command.ID,
		SessionID: "untrusted-wrong-session",
	})

	if got, want := source.exactGetCalls(), []string{command.ID}; !equalStrings(got, want) {
		t.Fatalf("exact Get calls = %v, want %v", got, want)
	}
	key := mustNudgeReconcileKey(t, nudgeCommandReconcileScope(storeBinding), command.Target.SessionID)
	wrongKey := mustNudgeReconcileKey(t, nudgeCommandReconcileScope(storeBinding), "untrusted-wrong-session")
	cr.nudgeKeyController.mu.Lock()
	_, accepted := cr.nudgeKeyController.pending[key]
	_, wrongAccepted := cr.nudgeKeyController.pending[wrongKey]
	cr.nudgeKeyController.mu.Unlock()
	if !accepted || wrongAccepted {
		t.Fatalf("durable target accepted=%v hinted target accepted=%v, want true/false", accepted, wrongAccepted)
	}
}

func TestNudgeKeyCanceledExactHintDoesNotReadRepository(t *testing.T) {
	storeBinding := nudgequeue.CommandStoreBinding{StoreUUID: "store-canceled-hint", RestoreEpoch: 1}
	source := &fakeNudgeCommandSource{snapshot: nudgequeue.CommandIndexSnapshot{Store: storeBinding}}
	cr := newInstalledNudgeKeyReaderForTest(t, source)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	cr.acceptNudgeKeyShadowHint(ctx, nudgeWakeHint{
		Version:   nudgequeue.SessionWakeHintVersion1,
		CommandID: "command-canceled",
		SessionID: "session-canceled",
	})
	if calls := source.exactGetCalls(); len(calls) != 0 {
		t.Fatalf("canceled exact hint Get calls = %v, want none", calls)
	}
}

func TestNudgeKeyExactHintRejectsMissingMalformedAndForeignDurableAuthority(t *testing.T) {
	installed := nudgequeue.CommandStoreBinding{StoreUUID: "store-installed", RestoreEpoch: 4}
	foreign := nudgequeue.CommandStoreBinding{StoreUUID: "store-foreign", RestoreEpoch: 4}
	valid := pageTestCommand("command-valid", "session-valid", 1, 1, nudgequeue.CommandStatePending, installed)
	foreignEntry := pageTestCommand("command-foreign-entry", "session-foreign", 1, 1, nudgequeue.CommandStatePending, foreign)
	wrongID := pageTestCommand("different-command", "session-valid", 1, 1, nudgequeue.CommandStatePending, installed)
	tests := []struct {
		name       string
		commandID  string
		resolution nudgequeue.CommandIndexResolution
	}{
		{
			name:      "missing",
			commandID: "command-missing",
			resolution: nudgequeue.CommandIndexResolution{
				Store: installed,
			},
		},
		{
			name:      "foreign resolution lineage",
			commandID: valid.ID,
			resolution: nudgequeue.CommandIndexResolution{
				Store: foreign, Revision: 1, Entry: knownPageEntry(valid), Found: true,
			},
		},
		{
			name:      "foreign entry lineage",
			commandID: foreignEntry.ID,
			resolution: nudgequeue.CommandIndexResolution{
				Store: installed, Revision: 1, Entry: knownPageEntry(foreignEntry), Found: true,
			},
		},
		{
			name:      "wrong durable command id",
			commandID: valid.ID,
			resolution: nudgequeue.CommandIndexResolution{
				Store: installed, Revision: 1, Entry: knownPageEntry(wrongID), Found: true,
			},
		},
		{
			name:      "malformed tagged entry",
			commandID: "command-malformed",
			resolution: nudgequeue.CommandIndexResolution{
				Store: installed, Revision: 1, Entry: nudgequeue.CommandIndexEntry{}, Found: true,
			},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			source := &fakeNudgeCommandSource{
				snapshot: nudgequeue.CommandIndexSnapshot{Store: installed},
				resolutions: map[string]nudgequeue.CommandIndexResolution{
					tc.commandID: tc.resolution,
				},
			}
			cr := newInstalledNudgeKeyReaderForTest(t, source)
			cr.acceptNudgeKeyShadowHint(t.Context(), nudgeWakeHint{
				Version:   nudgequeue.SessionWakeHintVersion1,
				CommandID: tc.commandID,
				SessionID: "hint-must-not-be-authority",
			})
			cr.nudgeKeyController.mu.Lock()
			pending := len(cr.nudgeKeyController.pending)
			cr.nudgeKeyController.mu.Unlock()
			if pending != 0 {
				t.Fatalf("invalid exact authority enqueued %d key(s), want none", pending)
			}
		})
	}
}

func TestNudgeKeyExactUnsyncedReadEnqueuesStoredTargetForAudit(t *testing.T) {
	storeBinding := nudgequeue.CommandStoreBinding{StoreUUID: "store-exact-audit", RestoreEpoch: 7}
	command := pageTestCommand("command-exact-audit", "session-exact-audit", 1, 1, nudgequeue.CommandStatePending, storeBinding)
	source := &fakeNudgeCommandSource{
		snapshot: nudgequeue.CommandIndexSnapshot{Store: storeBinding},
		resolutions: map[string]nudgequeue.CommandIndexResolution{
			command.ID: {Store: storeBinding, Revision: 1, Entry: knownPageEntry(command), Found: true},
		},
		getErr: fmt.Errorf("repository feed is incomplete: %w", nudgequeue.ErrCommandIndexUnsynced),
	}
	cr := newInstalledNudgeKeyReaderForTest(t, source)
	cr.acceptNudgeKeyShadowHint(t.Context(), nudgeWakeHint{
		Version:   nudgequeue.SessionWakeHintVersion1,
		CommandID: command.ID,
		SessionID: "untrusted-session",
	})

	key := mustNudgeReconcileKey(t, nudgeCommandReconcileScope(storeBinding), command.Target.SessionID)
	cr.nudgeKeyController.mu.Lock()
	batch, accepted := cr.nudgeKeyController.pending[key]
	cr.nudgeKeyController.mu.Unlock()
	if !accepted {
		t.Fatal("unsynced exact read with validated durable routing did not enqueue its stored target")
	}
	outcome := cr.nudgeKeyReader.reconcile(t.Context(), key, batch)
	if outcome.disposition != nudgeReconcileOutcomeAudit || outcome.err != nil {
		t.Fatalf("unsynced exact read outcome = %#v, want audit", outcome)
	}
}

func TestNudgeKeyReaderMapsAuditAndTransientRepositoryFailureToClosedOutcomes(t *testing.T) {
	storeBinding := nudgequeue.CommandStoreBinding{StoreUUID: "store-audit", RestoreEpoch: 2}
	source := &fakeNudgeCommandSource{
		snapshot: nudgequeue.CommandIndexSnapshot{Store: storeBinding},
	}
	reader, err := newNudgeKeyReadShadow(t.Context(), source, nudgequeue.MaxCommandIndexPageSize, newNudgeKeyObservationWarnings(&bytes.Buffer{}))
	if err != nil {
		t.Fatalf("newNudgeKeyReadShadow: %v", err)
	}
	gap := pageTestCommand("command-gap", "session-audit", 1, 2, nudgequeue.CommandStatePending, storeBinding)
	if err := reader.index.Apply(nudgequeue.CommandIndexMutation{Store: storeBinding, Revision: 2, Entry: commandEntryPtr(gap)}); !errors.Is(err, nudgequeue.ErrCommandIndexUnsynced) {
		t.Fatalf("Apply revision gap error = %v, want ErrCommandIndexUnsynced", err)
	}
	key := mustNudgeReconcileKey(t, nudgeCommandReconcileScope(storeBinding), gap.Target.SessionID)
	outcome := reader.reconcile(t.Context(), key, nudgeReconcileBatch{Causes: nudgeCauseCommandCommit})
	if outcome.disposition != nudgeReconcileOutcomeAudit || outcome.err != nil {
		t.Fatalf("unsynced outcome = %#v, want audit", outcome)
	}

	wantErr := errors.New("repository temporarily unavailable")
	source.snapshotErr = wantErr
	source.transientErr = wantErr
	outcome = reader.reconcile(t.Context(), key, nudgeReconcileBatch{Causes: nudgeCauseAudit})
	if outcome.disposition != nudgeReconcileOutcomeTransient || !errors.Is(outcome.err, wantErr) {
		t.Fatalf("audit source failure outcome = %#v, want transient %v", outcome, wantErr)
	}
}

func TestNudgeKeyReaderTreatsForeignAuditSnapshotAsInvariant(t *testing.T) {
	installed := nudgequeue.CommandStoreBinding{StoreUUID: "store-installed-audit", RestoreEpoch: 2}
	foreign := nudgequeue.CommandStoreBinding{StoreUUID: "store-foreign-audit", RestoreEpoch: 2}
	source := &fakeNudgeCommandSource{snapshot: nudgequeue.CommandIndexSnapshot{Store: installed}}
	reader, err := newNudgeKeyReadShadow(t.Context(), source, nudgequeue.MaxCommandIndexPageSize, newNudgeKeyObservationWarnings(&bytes.Buffer{}))
	if err != nil {
		t.Fatalf("newNudgeKeyReadShadow: %v", err)
	}
	source.snapshot = nudgequeue.CommandIndexSnapshot{Store: foreign}
	key := mustNudgeReconcileKey(t, nudgeCommandReconcileScope(installed), "session-foreign-audit")
	outcome := reader.reconcile(t.Context(), key, nudgeReconcileBatch{Causes: nudgeCauseAudit})
	if outcome.disposition != nudgeReconcileOutcomeInvariant || outcome.err == nil {
		t.Fatalf("foreign audit snapshot outcome = %#v, want invariant", outcome)
	}
}

func TestNudgeKeyReadOnlyContinuationPreservesCursorAcrossBoundedCallbacks(t *testing.T) {
	storeBinding := nudgequeue.CommandStoreBinding{StoreUUID: "store-bounded", RestoreEpoch: 6}
	commands := make([]nudgequeue.CommandIndexEntry, 0, nudgeKeyReadMaxPagesPerCallback+2)
	for sequence := 1; sequence <= nudgeKeyReadMaxPagesPerCallback+2; sequence++ {
		command := pageTestCommand(
			fmt.Sprintf("command-%d", sequence),
			"session-bounded",
			uint64(sequence),
			uint64(sequence),
			nudgequeue.CommandStatePending,
			storeBinding,
		)
		commands = append(commands, knownPageEntry(command))
	}
	source := &fakeNudgeCommandSource{snapshot: nudgequeue.CommandIndexSnapshot{
		Store:             storeBinding,
		Entries:           commands,
		Revision:          uint64(len(commands)),
		SequenceHighWater: uint64(len(commands)),
	}}
	reader, err := newNudgeKeyReadShadow(t.Context(), source, 1, newNudgeKeyObservationWarnings(&bytes.Buffer{}))
	if err != nil {
		t.Fatalf("newNudgeKeyReadShadow: %v", err)
	}
	fixedNow := time.Date(2026, 7, 15, 0, 0, 0, 0, time.UTC)
	reader.now = func() time.Time { return fixedNow }
	pager := &recordingNudgePageCursor{delegate: reader.index}
	reader.reconciler.pager = pager
	key := mustNudgeReconcileKey(t, nudgeCommandReconcileScope(storeBinding), "session-bounded")
	outcome := reader.reconcile(t.Context(), key, nudgeReconcileBatch{Causes: nudgeCauseCommandCommit})
	if outcome.disposition != nudgeReconcileOutcomeContinue || outcome.err != nil {
		t.Fatalf("bounded read-only continuation outcome = %#v, want continue", outcome)
	}
	wantAfter := []uint64{0, 1, 2, 3}
	if !equalUint64s(pager.after, wantAfter) {
		t.Fatalf("stack-local page cursors = %v, want bounded visits %v", pager.after, wantAfter)
	}

	outcome = reader.reconcile(t.Context(), key, nudgeReconcileBatch{Causes: nudgeCauseCommandCommit})
	if outcome.disposition != nudgeReconcileOutcomeForget || outcome.err != nil {
		t.Fatalf("final read-only continuation outcome = %#v, want forget", outcome)
	}
	wantAfter = []uint64{0, 1, 2, 3, 4, 5}
	if !equalUint64s(pager.after, wantAfter) {
		t.Fatalf("cross-callback page cursors = %v, want complete visit %v", pager.after, wantAfter)
	}
}

func TestNudgeKeyReadOnlyContinuationFullyVisitsMoreThan1024ActiveCommands(t *testing.T) {
	storeBinding := nudgequeue.CommandStoreBinding{StoreUUID: "store-deep-continuation", RestoreEpoch: 1}
	const commandCount = nudgequeue.MaxCommandIndexPageSize*nudgeKeyReadMaxPagesPerCallback + 1
	entries := make([]nudgequeue.CommandIndexEntry, 0, commandCount)
	for sequence := 1; sequence <= commandCount; sequence++ {
		entries = append(entries, knownPageEntry(pageTestCommand(
			fmt.Sprintf("command-deep-%d", sequence),
			"session-deep-continuation",
			uint64(sequence),
			uint64(sequence),
			nudgequeue.CommandStatePending,
			storeBinding,
		)))
	}
	source := &fakeNudgeCommandSource{snapshot: nudgequeue.CommandIndexSnapshot{
		Store: storeBinding, Entries: entries, Revision: commandCount, SequenceHighWater: commandCount,
	}}
	reader, err := newNudgeKeyReadShadow(t.Context(), source, nudgequeue.MaxCommandIndexPageSize, newNudgeKeyObservationWarnings(&bytes.Buffer{}))
	if err != nil {
		t.Fatalf("newNudgeKeyReadShadow: %v", err)
	}
	fixedNow := time.Date(2026, 7, 15, 0, 30, 0, 0, time.UTC)
	reader.now = func() time.Time { return fixedNow }
	pager := &recordingNudgePageCursor{delegate: reader.index}
	reader.reconciler.pager = pager
	key := mustNudgeReconcileKey(t, nudgeCommandReconcileScope(storeBinding), "session-deep-continuation")
	if first := reader.reconcile(t.Context(), key, nudgeReconcileBatch{Causes: nudgeCauseCommandCommit}); first.disposition != nudgeReconcileOutcomeContinue {
		t.Fatalf("first 1024-command slice outcome = %#v, want continue", first)
	}
	if second := reader.reconcile(t.Context(), key, nudgeReconcileBatch{Causes: nudgeCauseCommandCommit}); second.disposition != nudgeReconcileOutcomeForget {
		t.Fatalf("final deep slice outcome = %#v, want forget", second)
	}
	wantAfter := []uint64{0, 256, 512, 768, 1024}
	if !equalUint64s(pager.after, wantAfter) {
		t.Fatalf("deep continuation cursors = %v, want complete bounded walk %v", pager.after, wantAfter)
	}
}

func TestNudgeKeyReadOnlyContinuationRestartsAfterDirtyExactUpdate(t *testing.T) {
	storeBinding := nudgequeue.CommandStoreBinding{StoreUUID: "store-dirty-continuation", RestoreEpoch: 2}
	entries := make([]nudgequeue.CommandIndexEntry, 0, nudgeKeyReadMaxPagesPerCallback+2)
	for sequence := 1; sequence <= nudgeKeyReadMaxPagesPerCallback+2; sequence++ {
		entries = append(entries, knownPageEntry(pageTestCommand(
			fmt.Sprintf("command-%d", sequence),
			"session-dirty-continuation",
			uint64(sequence),
			uint64(sequence),
			nudgequeue.CommandStatePending,
			storeBinding,
		)))
	}
	updated := pageTestCommand(
		"command-1",
		"session-dirty-continuation",
		1,
		uint64(len(entries)+1),
		nudgequeue.CommandStateInFlight,
		storeBinding,
	)
	source := &fakeNudgeCommandSource{
		snapshot: nudgequeue.CommandIndexSnapshot{
			Store:             storeBinding,
			Entries:           entries,
			Revision:          uint64(len(entries)),
			SequenceHighWater: uint64(len(entries)),
		},
		resolutions: map[string]nudgequeue.CommandIndexResolution{
			updated.ID: {
				Store:    storeBinding,
				Revision: updated.Order.Revision,
				Entry:    knownPageEntry(updated),
				Found:    true,
			},
		},
	}
	reader, err := newNudgeKeyReadShadow(t.Context(), source, 1, newNudgeKeyObservationWarnings(&bytes.Buffer{}))
	if err != nil {
		t.Fatalf("newNudgeKeyReadShadow: %v", err)
	}
	fixedNow := time.Date(2026, 7, 15, 1, 0, 0, 0, time.UTC)
	reader.now = func() time.Time { return fixedNow }
	pager := &recordingNudgePageCursor{delegate: reader.index}
	reader.reconciler.pager = pager
	key := mustNudgeReconcileKey(t, nudgeCommandReconcileScope(storeBinding), updated.Target.SessionID)
	first := reader.reconcile(t.Context(), key, nudgeReconcileBatch{Causes: nudgeCauseCommandCommit})
	if first.disposition != nudgeReconcileOutcomeContinue {
		t.Fatalf("first continuation outcome = %#v, want continue", first)
	}
	if _, accepted, err := reader.acceptCommandHint(t.Context(), updated.ID); err != nil || !accepted {
		t.Fatalf("accept dirty exact update = accepted:%v err:%v, want true/nil", accepted, err)
	}
	second := reader.reconcile(t.Context(), key, nudgeReconcileBatch{Causes: nudgeCauseCommandCommit})
	if second.disposition != nudgeReconcileOutcomeContinue {
		t.Fatalf("post-dirty continuation outcome = %#v, want continue from reset cursor", second)
	}
	if got := pager.after[nudgeKeyReadMaxPagesPerCallback]; got != 0 {
		t.Fatalf("first cursor after dirty update = %d, want restart at zero", got)
	}
}

func TestNudgeKeyExactHintAcceptsIdenticalReplayOlderThanIndexHistory(t *testing.T) {
	storeBinding := nudgequeue.CommandStoreBinding{StoreUUID: "store-old-replay", RestoreEpoch: 5}
	commandCount := nudgequeue.MaxCommandIndexReplayHistory + 17
	entries := make([]nudgequeue.CommandIndexEntry, 0, commandCount)
	for sequence := 1; sequence <= commandCount; sequence++ {
		entries = append(entries, knownPageEntry(pageTestCommand(
			fmt.Sprintf("command-%d", sequence),
			fmt.Sprintf("session-%d", sequence),
			uint64(sequence),
			uint64(sequence),
			nudgequeue.CommandStatePending,
			storeBinding,
		)))
	}
	old := *entries[0].Command
	source := &fakeNudgeCommandSource{
		snapshot: nudgequeue.CommandIndexSnapshot{
			Store:             storeBinding,
			Entries:           entries,
			Revision:          uint64(commandCount),
			SequenceHighWater: uint64(commandCount),
		},
		resolutions: map[string]nudgequeue.CommandIndexResolution{
			old.ID: {
				Store:                  storeBinding,
				Revision:               uint64(commandCount),
				CompletedAuditRevision: uint64(commandCount),
				Entry:                  knownPageEntry(old),
				Found:                  true,
			},
		},
	}
	reader, err := newNudgeKeyReadShadow(t.Context(), source, 1, newNudgeKeyObservationWarnings(&bytes.Buffer{}))
	if err != nil {
		t.Fatalf("newNudgeKeyReadShadow: %v", err)
	}
	sessionID, accepted, err := reader.acceptCommandHint(t.Context(), old.ID)
	if err != nil || !accepted || sessionID != old.Target.SessionID {
		t.Fatalf("old identical exact replay = session:%q accepted:%v err:%v", sessionID, accepted, err)
	}
	if status := reader.index.Status(); !status.Synced || status.Revision != uint64(commandCount) {
		t.Fatalf("index status after old identical replay = %#v, want unchanged synced projection", status)
	}
}

func TestNudgeKeyExactHintRejectsConflictingSameRevisionWithoutPoisoningIndex(t *testing.T) {
	storeBinding := nudgequeue.CommandStoreBinding{StoreUUID: "store-conflicting-replay", RestoreEpoch: 1}
	original := pageTestCommand("command-conflict", "session-conflict", 1, 1, nudgequeue.CommandStatePending, storeBinding)
	conflicting := original
	conflicting.Message = "different durable content"
	conflicting.TrustedIngress.PayloadDigest = nudgequeue.ComputeCommandPayloadDigest(conflicting)
	source := &fakeNudgeCommandSource{
		snapshot: nudgequeue.CommandIndexSnapshot{
			Store: storeBinding, Entries: []nudgequeue.CommandIndexEntry{knownPageEntry(original)}, Revision: 1, SequenceHighWater: 1,
		},
		resolutions: map[string]nudgequeue.CommandIndexResolution{
			original.ID: {Store: storeBinding, Revision: 1, Entry: knownPageEntry(conflicting), Found: true},
		},
	}
	reader, err := newNudgeKeyReadShadow(t.Context(), source, 1, newNudgeKeyObservationWarnings(&bytes.Buffer{}))
	if err != nil {
		t.Fatalf("newNudgeKeyReadShadow: %v", err)
	}
	if _, accepted, err := reader.acceptCommandHint(t.Context(), original.ID); err == nil || accepted {
		t.Fatalf("conflicting same-revision exact replay = accepted:%v err:%v, want invariant rejection", accepted, err)
	}
	if status := reader.index.Status(); !status.Synced || status.Revision != 1 {
		t.Fatalf("index status after rejected conflict = %#v, want original synced projection", status)
	}
}

func TestNudgeKeyContinuationCacheIsBoundedAndEvictionRestartsFromZero(t *testing.T) {
	cache := newNudgeKeyContinuationCache(2)
	first := mustNudgeReconcileKey(t, "scope", "session-1")
	second := mustNudgeReconcileKey(t, "scope", "session-2")
	third := mustNudgeReconcileKey(t, "scope", "session-3")
	firstToken, _ := cache.begin(first)
	if !cache.advance(first, firstToken, 10) {
		t.Fatal("first cursor advance failed")
	}
	cache.begin(second)
	cache.begin(third)
	cache.mu.Lock()
	gotSize := len(cache.entries)
	_, firstRetained := cache.entries[first]
	cache.mu.Unlock()
	if gotSize != 2 || firstRetained {
		t.Fatalf("bounded cache size/oldest retained = %d/%v, want 2/false", gotSize, firstRetained)
	}
	if cache.advance(first, firstToken, 20) {
		t.Fatal("evicted in-flight cursor was allowed to restore stale state")
	}
	_, after := cache.begin(first)
	if after != 0 {
		t.Fatalf("evicted key restarted at %d, want zero", after)
	}
}

func TestNudgeKeyPeriodicAntiEntropyDiscoversMissedHintWithOneSnapshot(t *testing.T) {
	storeBinding := nudgequeue.CommandStoreBinding{StoreUUID: "store-missed-hint", RestoreEpoch: 3}
	source := &fakeNudgeCommandSource{snapshot: nudgequeue.CommandIndexSnapshot{Store: storeBinding}}
	cr := newInstalledNudgeKeyReaderForTest(t, source)
	reconciled := make(chan reconcilekey.Session, 1)
	productionReconcile := cr.nudgeKeyController.reconcile
	cr.nudgeKeyController.reconcile = func(ctx context.Context, key reconcilekey.Session, batch nudgeReconcileBatch) nudgeReconcileOutcome {
		reconciled <- key
		return productionReconcile(ctx, key, batch)
	}
	ticks := make(chan time.Time, 1)
	tickerStopped := make(chan struct{})
	requestedInterval := make(chan time.Duration, 1)
	cr.nudgeKeyTickerFactory = func(interval time.Duration) nudgeKeyPeriodicTicker {
		requestedInterval <- interval
		return nudgeKeyPeriodicTicker{ticks: ticks, stop: func() { close(tickerStopped) }}
	}
	ctx, cancel := context.WithCancel(context.Background())
	stop := cr.startNudgeKeyController(ctx)
	if got := receiveBeforeDeadline(t, requestedInterval); got != defaultNudgeKeyAntiEntropyInterval {
		cancel()
		stop()
		t.Fatalf("anti-entropy interval = %v, want %v", got, defaultNudgeKeyAntiEntropyInterval)
	}
	command := pageTestCommand("command-missed", "session-missed", 1, 1, nudgequeue.CommandStatePending, storeBinding)
	source.setSnapshot(nudgequeue.CommandIndexSnapshot{
		Store: storeBinding, Entries: []nudgequeue.CommandIndexEntry{knownPageEntry(command)}, Revision: 1, SequenceHighWater: 1,
	})
	ticks <- time.Now()
	key := receiveBeforeDeadline(t, reconciled)
	if key.SessionID() != command.Target.SessionID {
		t.Fatalf("anti-entropy reconciled key = %s, want session %q", key, command.Target.SessionID)
	}
	if got := source.snapshotCallCount(); got != 2 {
		t.Fatalf("snapshot calls after one interval = %d, want startup + one global audit", got)
	}
	cancel()
	stop()
	receiveBeforeDeadline(t, tickerStopped)
}

func TestNudgeKeyPeriodicAuditConcurrentAdvanceArmsExplicitBoundedRetry(t *testing.T) {
	storeBinding := nudgequeue.CommandStoreBinding{StoreUUID: "store-concurrent-audit", RestoreEpoch: 6}
	source := &fakeNudgeCommandSource{snapshot: nudgequeue.CommandIndexSnapshot{Store: storeBinding}}
	cr := newInstalledNudgeKeyReaderForTest(t, source)
	command := pageTestCommand("command-concurrent-audit", "session-concurrent-audit", 1, 1, nudgequeue.CommandStatePending, storeBinding)
	entry := knownPageEntry(command)
	var advanceOnce sync.Once
	source.setOnSnapshot(func(context.Context) error {
		advanceOnce.Do(func() {
			if err := cr.nudgeKeyReader.index.Apply(nudgequeue.CommandIndexMutation{
				Store: storeBinding, Revision: 1, Entry: &entry,
			}); err != nil {
				t.Errorf("concurrent exact Apply: %v", err)
			}
		})
		return nil
	})
	source.setSnapshot(nudgequeue.CommandIndexSnapshot{
		Store: storeBinding, Entries: []nudgequeue.CommandIndexEntry{entry}, Revision: 1, SequenceHighWater: 1,
	})

	reconciled := make(chan reconcilekey.Session, 1)
	productionReconcile := cr.nudgeKeyController.reconcile
	cr.nudgeKeyController.reconcile = func(ctx context.Context, key reconcilekey.Session, batch nudgeReconcileBatch) nudgeReconcileOutcome {
		reconciled <- key
		return productionReconcile(ctx, key, batch)
	}
	periodicTicks := make(chan time.Time, 1)
	retryTicks := make(chan time.Time, 1)
	retryDelays := make(chan time.Duration, 1)
	cr.nudgeKeyTickerFactory = func(time.Duration) nudgeKeyPeriodicTicker {
		return nudgeKeyPeriodicTicker{ticks: periodicTicks, stop: func() {}}
	}
	cr.nudgeKeyRetryTimerFactory = func(delay time.Duration) nudgeKeyRetryTimer {
		retryDelays <- delay
		return nudgeKeyRetryTimer{ticks: retryTicks, stop: func() {}}
	}
	ctx, cancel := context.WithCancel(context.Background())
	stop := cr.startNudgeKeyController(ctx)
	periodicTicks <- time.Now()
	if got := receiveBeforeDeadline(t, retryDelays); got != defaultNudgeKeyAuditRetryBaseDelay {
		cancel()
		stop()
		t.Fatalf("concurrent-audit retry delay = %v, want %v", got, defaultNudgeKeyAuditRetryBaseDelay)
	}
	if !cr.nudgeKeyReader.auditRetryRequired.Load() {
		cancel()
		stop()
		t.Fatal("concurrent audit advance did not retain explicit retry-required state")
	}
	cr.nudgeKeyController.mu.Lock()
	pendingBeforeRetry := len(cr.nudgeKeyController.pending)
	cr.nudgeKeyController.mu.Unlock()
	if pendingBeforeRetry != 0 {
		cancel()
		stop()
		t.Fatalf("incomplete global audit admitted %d key(s) before reconstruction completed", pendingBeforeRetry)
	}
	retryTicks <- time.Now()
	if key := receiveBeforeDeadline(t, reconciled); key.SessionID() != command.Target.SessionID {
		cancel()
		stop()
		t.Fatalf("retried audit key = %s, want session %q", key, command.Target.SessionID)
	}
	if cr.nudgeKeyReader.auditRetryRequired.Load() {
		cancel()
		stop()
		t.Fatal("successful global retry left retry-required state set")
	}
	if got := source.snapshotCallCount(); got != 3 {
		cancel()
		stop()
		t.Fatalf("snapshot calls = %d, want startup + one interval + one bounded retry", got)
	}
	cancel()
	stop()
}

func TestNudgeKeyPeriodicAntiEntropyRetainsTransientExactReadRecovery(t *testing.T) {
	storeBinding := nudgequeue.CommandStoreBinding{StoreUUID: "store-transient-exact", RestoreEpoch: 4}
	transient := errors.New("repository connection temporarily unavailable")
	source := &fakeNudgeCommandSource{
		snapshot:     nudgequeue.CommandIndexSnapshot{Store: storeBinding},
		resolutions:  make(map[string]nudgequeue.CommandIndexResolution),
		getErr:       transient,
		transientErr: transient,
	}
	cr := newInstalledNudgeKeyReaderForTest(t, source)
	command := pageTestCommand("command-after-exact-failure", "session-after-exact-failure", 1, 1, nudgequeue.CommandStatePending, storeBinding)
	cr.acceptNudgeKeyShadowHint(t.Context(), nudgeWakeHint{Version: nudgequeue.SessionWakeHintVersion1, CommandID: command.ID})
	if got := source.exactGetCalls(); !equalStrings(got, []string{command.ID}) {
		t.Fatalf("transient exact read calls = %v, want command %q", got, command.ID)
	}

	reconciled := make(chan reconcilekey.Session, 1)
	productionReconcile := cr.nudgeKeyController.reconcile
	cr.nudgeKeyController.reconcile = func(ctx context.Context, key reconcilekey.Session, batch nudgeReconcileBatch) nudgeReconcileOutcome {
		reconciled <- key
		return productionReconcile(ctx, key, batch)
	}
	ticks := make(chan time.Time, 1)
	cr.nudgeKeyTickerFactory = func(time.Duration) nudgeKeyPeriodicTicker {
		return nudgeKeyPeriodicTicker{ticks: ticks, stop: func() {}}
	}
	ctx, cancel := context.WithCancel(context.Background())
	stop := cr.startNudgeKeyController(ctx)
	source.setGetError(nil)
	source.setSnapshot(nudgequeue.CommandIndexSnapshot{
		Store: storeBinding, Entries: []nudgequeue.CommandIndexEntry{knownPageEntry(command)}, Revision: 1, SequenceHighWater: 1,
	})
	ticks <- time.Now()
	if key := receiveBeforeDeadline(t, reconciled); key.SessionID() != command.Target.SessionID {
		t.Fatalf("recovered exact-read key = %s, want session %q", key, command.Target.SessionID)
	}
	cancel()
	stop()
}

func TestNudgeKeyTransientStartupSnapshotRetriesOnBoundedTick(t *testing.T) {
	storeBinding := nudgequeue.CommandStoreBinding{StoreUUID: "store-startup-retry", RestoreEpoch: 2}
	transient := errors.New("startup repository temporarily unavailable")
	snapshots := make(chan struct{}, 4)
	source := &fakeNudgeCommandSource{
		snapshot:     nudgequeue.CommandIndexSnapshot{Store: storeBinding},
		snapshotErr:  transient,
		transientErr: transient,
		onSnapshot: func(context.Context) error {
			snapshots <- struct{}{}
			return nil
		},
	}
	ticks := make(chan time.Time, 2)
	retryTicks := make(chan time.Time, 2)
	tickerStopped := make(chan struct{})
	cr := &CityRuntime{
		cityPath:            t.TempDir(),
		cfg:                 supervisorCfg(),
		standaloneCityStore: beads.NewMemStore(),
		stderr:              &bytes.Buffer{},
		nudgeCommandSourceOpener: func(context.Context, string, beads.Store, nudgequeue.TrustedCityPartition, nudgequeue.TrustedCityPartitionResolver) (nudgeCommandSource, error) {
			return source, nil
		},
		nudgeKeyTickerFactory: func(time.Duration) nudgeKeyPeriodicTicker {
			return nudgeKeyPeriodicTicker{ticks: ticks, stop: func() { close(tickerStopped) }}
		},
		nudgeKeyRetryTimerFactory: func(time.Duration) nudgeKeyRetryTimer {
			return nudgeKeyRetryTimer{ticks: retryTicks, stop: func() {}}
		},
	}
	if err := cr.installNudgeKeyShadow(t.Context()); err == nil || !nudgeCommandSourceFailureIsTransient(err) {
		t.Fatalf("initial transient install error = %v, want classified retry", err)
	}
	receiveBeforeDeadline(t, snapshots)
	if cr.nudgeKeyController != nil {
		t.Fatal("transient startup snapshot published a controller")
	}
	command := pageTestCommand("command-startup-retry", "session-startup-retry", 1, 1, nudgequeue.CommandStatePending, storeBinding)
	source.setSnapshot(nudgequeue.CommandIndexSnapshot{
		Store: storeBinding, Entries: []nudgequeue.CommandIndexEntry{knownPageEntry(command)}, Revision: 1, SequenceHighWater: 1,
	})
	ctx, cancel := context.WithCancel(context.Background())
	stop := cr.startNudgeKeyController(ctx)
	ticks <- time.Now()
	receiveBeforeDeadline(t, snapshots)
	// A second tick can only perform an audit after the first tick completed
	// installation and started the controller.
	ticks <- time.Now()
	receiveBeforeDeadline(t, snapshots)
	cancel()
	stop()
	receiveBeforeDeadline(t, tickerStopped)
	if cr.nudgeKeyController == nil || cr.nudgeKeyReader == nil {
		t.Fatal("transient startup failure did not install on a later bounded tick")
	}
}

func TestNudgeKeyTransientOpenerRetriesOnBoundedTick(t *testing.T) {
	storeBinding := nudgequeue.CommandStoreBinding{StoreUUID: "store-opener-retry", RestoreEpoch: 7}
	command := pageTestCommand("command-opener-retry", "session-opener-retry", 1, 1, nudgequeue.CommandStatePending, storeBinding)
	snapshots := make(chan struct{}, 3)
	source := &fakeNudgeCommandSource{
		snapshot: nudgequeue.CommandIndexSnapshot{
			Store: storeBinding, Entries: []nudgequeue.CommandIndexEntry{knownPageEntry(command)}, Revision: 1, SequenceHighWater: 1,
		},
		onSnapshot: func(context.Context) error {
			snapshots <- struct{}{}
			return nil
		},
	}
	transient := errors.New("opening repository connection failed")
	opens := 0
	openAttempts := make(chan int, 2)
	ticks := make(chan time.Time, 2)
	retryTicks := make(chan time.Time, 2)
	cr := &CityRuntime{
		cityPath:            t.TempDir(),
		cfg:                 supervisorCfg(),
		standaloneCityStore: beads.NewMemStore(),
		stderr:              &bytes.Buffer{},
		nudgeCommandSourceOpener: func(context.Context, string, beads.Store, nudgequeue.TrustedCityPartition, nudgequeue.TrustedCityPartitionResolver) (nudgeCommandSource, error) {
			opens++
			openAttempts <- opens
			if opens == 1 {
				// This is the contract the production Provision/open adapter uses:
				// only positively transient failures receive retry admission.
				return nil, retryableNudgeCommandSourceFailure(transient)
			}
			return source, nil
		},
		nudgeKeyTickerFactory: func(time.Duration) nudgeKeyPeriodicTicker {
			return nudgeKeyPeriodicTicker{ticks: ticks, stop: func() {}}
		},
		nudgeKeyRetryTimerFactory: func(time.Duration) nudgeKeyRetryTimer {
			return nudgeKeyRetryTimer{ticks: retryTicks, stop: func() {}}
		},
	}
	if err := cr.installNudgeKeyShadow(t.Context()); err == nil || !nudgeCommandSourceFailureIsTransient(err) {
		t.Fatalf("initial transient opener error = %v, want classified retry", err)
	}
	if got := receiveBeforeDeadline(t, openAttempts); got != 1 {
		t.Fatalf("initial opener attempt = %d, want 1", got)
	}
	ctx, cancel := context.WithCancel(context.Background())
	stop := cr.startNudgeKeyController(ctx)
	ticks <- time.Now()
	if got := receiveBeforeDeadline(t, openAttempts); got != 2 {
		cancel()
		stop()
		t.Fatalf("retried opener attempt = %d, want 2", got)
	}
	receiveBeforeDeadline(t, snapshots)
	// Confirm the installed child owns the next interval rather than reopening.
	ticks <- time.Now()
	receiveBeforeDeadline(t, snapshots)
	cancel()
	stop()
	if opens != 2 || cr.nudgeKeyController == nil {
		t.Fatalf("opener attempts/controller = %d/%v, want 2/installed", opens, cr.nudgeKeyController != nil)
	}
}

func TestNudgeKeyUnclassifiedOpenerFailureFailsClosedWithoutRetry(t *testing.T) {
	rawFailure := errors.New("unclassified repository opener failure")
	tickerStarts := 0
	cr := &CityRuntime{
		cityPath:            t.TempDir(),
		cfg:                 supervisorCfg(),
		standaloneCityStore: beads.NewMemStore(),
		stderr:              &bytes.Buffer{},
		nudgeCommandSourceOpener: func(context.Context, string, beads.Store, nudgequeue.TrustedCityPartition, nudgequeue.TrustedCityPartitionResolver) (nudgeCommandSource, error) {
			return nil, rawFailure
		},
		nudgeKeyTickerFactory: func(time.Duration) nudgeKeyPeriodicTicker {
			tickerStarts++
			return nudgeKeyPeriodicTicker{}
		},
	}
	if err := cr.installNudgeKeyShadow(t.Context()); !errors.Is(err, rawFailure) || nudgeCommandSourceFailureIsTransient(err) {
		t.Fatalf("unclassified opener error = %v transient=%v, want invariant failure", err, nudgeCommandSourceFailureIsTransient(err))
	}
	stop := cr.startNudgeKeyController(t.Context())
	stop()
	if tickerStarts != 0 {
		t.Fatalf("unclassified invariant opener started %d retry ticker(s), want none", tickerStarts)
	}
}

func TestNudgeKeyPeriodicInvariantFailsShadowClosed(t *testing.T) {
	installed := nudgequeue.CommandStoreBinding{StoreUUID: "store-periodic-invariant", RestoreEpoch: 1}
	foreign := nudgequeue.CommandStoreBinding{StoreUUID: "store-foreign-invariant", RestoreEpoch: 1}
	source := &fakeNudgeCommandSource{snapshot: nudgequeue.CommandIndexSnapshot{Store: installed}}
	cr := newInstalledNudgeKeyReaderForTest(t, source)
	closed := make(chan struct{})
	cr.nudgeKeyController.onAdmissionClosed = func() { close(closed) }
	ticks := make(chan time.Time, 1)
	tickerStopped := make(chan struct{})
	cr.nudgeKeyTickerFactory = func(time.Duration) nudgeKeyPeriodicTicker {
		return nudgeKeyPeriodicTicker{ticks: ticks, stop: func() { close(tickerStopped) }}
	}
	ctx, cancel := context.WithCancel(context.Background())
	stop := cr.startNudgeKeyController(ctx)
	source.setSnapshot(nudgequeue.CommandIndexSnapshot{Store: foreign})
	ticks <- time.Now()
	receiveBeforeDeadline(t, closed)
	receiveBeforeDeadline(t, tickerStopped)
	if err := cr.nudgeKeyController.Enqueue(mustNudgeReconcileKey(t, nudgeCommandReconcileScope(installed), "session-closed"), nudgeCauseAudit); err == nil {
		t.Fatal("invariant-failed shadow continued accepting work")
	}
	cancel()
	stop()
}

func TestNudgeKeyPeriodicAuditCancellationStopsTickerAndRead(t *testing.T) {
	storeBinding := nudgequeue.CommandStoreBinding{StoreUUID: "store-periodic-cancel", RestoreEpoch: 9}
	source := &fakeNudgeCommandSource{snapshot: nudgequeue.CommandIndexSnapshot{Store: storeBinding}}
	cr := newInstalledNudgeKeyReaderForTest(t, source)
	entered := make(chan struct{})
	var once sync.Once
	source.setOnSnapshot(func(ctx context.Context) error {
		once.Do(func() { close(entered) })
		<-ctx.Done()
		return ctx.Err()
	})
	ticks := make(chan time.Time, 1)
	tickerStopped := make(chan struct{})
	cr.nudgeKeyTickerFactory = func(time.Duration) nudgeKeyPeriodicTicker {
		return nudgeKeyPeriodicTicker{ticks: ticks, stop: func() { close(tickerStopped) }}
	}
	ctx, cancel := context.WithCancel(context.Background())
	stop := cr.startNudgeKeyController(ctx)
	ticks <- time.Now()
	receiveBeforeDeadline(t, entered)
	stopped := make(chan struct{})
	go func() {
		stop()
		close(stopped)
	}()
	receiveBeforeDeadline(t, stopped)
	receiveBeforeDeadline(t, tickerStopped)
	cancel()
}

func TestNudgeKeyReaderShutdownCancelsInFlightRepositoryAudit(t *testing.T) {
	storeBinding := nudgequeue.CommandStoreBinding{StoreUUID: "store-shutdown", RestoreEpoch: 8}
	entered := make(chan struct{})
	var enteredOnce sync.Once
	source := &fakeNudgeCommandSource{snapshot: nudgequeue.CommandIndexSnapshot{Store: storeBinding}}
	cr := newInstalledNudgeKeyReaderForTest(t, source)
	source.onSnapshot = func(ctx context.Context) error {
		enteredOnce.Do(func() { close(entered) })
		<-ctx.Done()
		return ctx.Err()
	}

	ctx, cancel := context.WithCancel(context.Background())
	stop := cr.startNudgeKeyController(ctx)
	key := mustNudgeReconcileKey(t, nudgeCommandReconcileScope(storeBinding), "session-shutdown")
	if err := cr.nudgeKeyController.Enqueue(key, nudgeCauseAudit); err != nil {
		cancel()
		stop()
		t.Fatalf("Enqueue audit: %v", err)
	}
	receiveBeforeDeadline(t, entered)
	stopped := make(chan struct{})
	go func() {
		stop()
		close(stopped)
	}()
	receiveBeforeDeadline(t, stopped)
	cancel()
}

func newInstalledNudgeKeyReaderForTest(t *testing.T, source nudgeCommandSource) *CityRuntime {
	t.Helper()
	cr := &CityRuntime{
		cityPath:            t.TempDir(),
		cfg:                 supervisorCfg(),
		standaloneCityStore: beads.NewMemStore(),
		stderr:              &bytes.Buffer{},
		nudgeCommandSourceOpener: func(context.Context, string, beads.Store, nudgequeue.TrustedCityPartition, nudgequeue.TrustedCityPartitionResolver) (nudgeCommandSource, error) {
			return source, nil
		},
	}
	if err := cr.installNudgeKeyShadow(t.Context()); err != nil {
		t.Fatalf("installNudgeKeyShadow: %v", err)
	}
	return cr
}

func commandEntryPtr(command nudgequeue.Command) *nudgequeue.CommandIndexEntry {
	entry := knownPageEntry(command)
	return &entry
}

func equalStrings(left, right []string) bool {
	if len(left) != len(right) {
		return false
	}
	for i := range left {
		if left[i] != right[i] {
			return false
		}
	}
	return true
}

func equalUint64s(left, right []uint64) bool {
	if len(left) != len(right) {
		return false
	}
	for i := range left {
		if left[i] != right[i] {
			return false
		}
	}
	return true
}

var (
	_ nudgeCommandSource                = (*fakeNudgeCommandSource)(nil)
	_ nudgeCommandSourceErrorClassifier = (*fakeNudgeCommandSource)(nil)
)
