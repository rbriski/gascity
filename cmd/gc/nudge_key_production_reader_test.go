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
		nudgeCommandSourceOpener: func(_ context.Context, gotCityPath string, got beads.Store) (nudgeCommandSource, error) {
			if gotCityPath != cr.cityPath {
				t.Fatalf("source opener city path = %q, want %q", gotCityPath, cr.cityPath)
			}
			if got != cityStore {
				t.Fatalf("source opener store = %T %p, want city store %T %p", got, got, cityStore, cityStore)
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
		nudgeCommandSourceOpener: func(context.Context, string, beads.Store) (nudgeCommandSource, error) {
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
		nudgeCommandSourceOpener: func(context.Context, string, beads.Store) (nudgeCommandSource, error) {
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
		nudgeCommandSourceOpener: func(context.Context, string, beads.Store) (nudgeCommandSource, error) {
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

func TestNudgeKeyReadOnlyContinuationVisitsBoundedPagesWithoutQueueTreadmill(t *testing.T) {
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
	if outcome.disposition != nudgeReconcileOutcomeForget || outcome.err != nil {
		t.Fatalf("unchanged read-only continuation outcome = %#v, want forget", outcome)
	}
	wantAfter := []uint64{0, 1, 2, 3}
	if !equalUint64s(pager.after, wantAfter) {
		t.Fatalf("stack-local page cursors = %v, want bounded visits %v", pager.after, wantAfter)
	}
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
		nudgeCommandSourceOpener: func(context.Context, string, beads.Store) (nudgeCommandSource, error) {
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
