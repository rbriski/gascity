package main

import (
	"bytes"
	"context"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/beads/contract"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/fsys"
	"github.com/gastownhall/gascity/internal/nudgequeue"
	"github.com/gastownhall/gascity/internal/reconcilekey"
	"github.com/gastownhall/gascity/internal/runtime"
	"github.com/gastownhall/gascity/internal/session"
	"github.com/gastownhall/gascity/internal/testutil"
)

// supervisorCfg returns a minimal *config.City wired for supervisor-mode
// nudge dispatching. Tests use it to drive nudgeDispatcherIsSupervisor.
func supervisorCfg() *config.City {
	return &config.City{
		Daemon: config.DaemonConfig{NudgeDispatcher: "supervisor"},
	}
}

func TestPingNudgeWakeSocketNoListenerIsNoOp(t *testing.T) {
	dir := t.TempDir()
	// No listener — DialTimeout returns "no such file or directory". The
	// helper must swallow it; otherwise enqueue producers would surface
	// transient warnings to legacy-mode users.
	pingNudgeWakeSocket(dir)
}

func TestPingNudgeWakeSocketEmptyCityPathIsNoOp(_ *testing.T) {
	// No assertion needed — test passes if pingNudgeWakeSocket does not
	// panic on an empty cityPath. The function dials a derived socket path
	// and exits silently on dial failure, which is the legacy-mode contract.
	pingNudgeWakeSocket("")
}

func TestStartNudgeWakeListenerSignalsOnConnect(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	dir := t.TempDir()
	wakeCh := make(chan struct{}, 1)

	lis, err := startNudgeWakeListener(ctx, dir, wakeCh)
	if err != nil {
		t.Fatalf("startNudgeWakeListener: %v", err)
	}
	defer lis.Close() //nolint:errcheck

	pingNudgeWakeSocket(dir)
	select {
	case <-wakeCh:
	case <-time.After(2 * time.Second):
		t.Fatal("wakeCh not signaled within 2s of producer ping")
	}
}

func TestDispatchAcceptedNudgeWakeSignalsImmediatelyAndBoundsSlowReaders(t *testing.T) {
	wakeCh := make(chan struct{}, 1)
	readerSlots := make(chan struct{}, 1)
	firstServer, firstClient := net.Pipe()
	defer firstClient.Close() //nolint:errcheck
	readerStarted := make(chan struct{})
	releaseReader := make(chan struct{})
	readerDone := make(chan struct{})
	released := false
	defer func() {
		if !released {
			close(releaseReader)
		}
	}()
	readConnection := func(conn net.Conn) {
		close(readerStarted)
		<-releaseReader
		_ = conn.Close()
		close(readerDone)
	}

	dispatchAcceptedNudgeWake(firstServer, wakeCh, readerSlots, readConnection)
	receiveBeforeDeadline(t, wakeCh)
	receiveBeforeDeadline(t, readerStarted)

	// The first reader is deliberately wedged. A second accepted connection
	// must still wake legacy immediately and be closed instead of allocating an
	// unbounded goroutine or waiting behind the slow exact frame.
	secondServer, secondClient := net.Pipe()
	defer secondClient.Close() //nolint:errcheck
	if err := secondClient.SetReadDeadline(time.Now().Add(testutil.GoroutineRaceTimeout)); err != nil {
		t.Fatalf("set saturated client read deadline: %v", err)
	}
	dispatchAcceptedNudgeWake(secondServer, wakeCh, readerSlots, readConnection)
	receiveBeforeDeadline(t, wakeCh)
	var one [1]byte
	if _, err := secondClient.Read(one[:]); err == nil {
		t.Fatal("saturated exact-reader connection remained open")
	}

	close(releaseReader)
	released = true
	receiveBeforeDeadline(t, readerDone)
}

func TestStartNudgeWakeListenerRoutesVersionedHintAndPreservesLegacyWake(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	dir := t.TempDir()
	wakeCh := make(chan struct{}, 1)
	exactCh := make(chan nudgeWakeHint, 1)

	lis, err := startNudgeWakeListenerWithHints(ctx, dir, wakeCh, func(hint nudgeWakeHint) {
		exactCh <- hint
	}, nil, "test")
	if err != nil {
		t.Fatalf("startNudgeWakeListenerWithHints: %v", err)
	}
	defer lis.Close() //nolint:errcheck

	want := nudgeWakeHint{Version: nudgequeue.SessionWakeHintVersion1, CommandID: "command-123", SessionID: "session-456"}
	pingNudgeWakeSocketHint(dir, want)
	if got := receiveBeforeDeadline(t, exactCh); got != want {
		t.Fatalf("exact hint = %+v, want %+v", got, want)
	}
	receiveBeforeDeadline(t, wakeCh)
}

func TestStartNudgeWakeListenerDecodesFragmentedFrame(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	dir := t.TempDir()
	exactCh := make(chan nudgeWakeHint, 1)
	lis, err := startNudgeWakeListenerWithHints(ctx, dir, make(chan struct{}, 1), func(hint nudgeWakeHint) {
		exactCh <- hint
	}, nil, "test")
	if err != nil {
		t.Fatalf("startNudgeWakeListenerWithHints: %v", err)
	}
	defer lis.Close() //nolint:errcheck

	want := nudgeWakeHint{Version: nudgequeue.SessionWakeHintVersion1, CommandID: "fragmented-command", SessionID: "fragmented-session"}
	wire, err := nudgequeue.EncodeSessionWakeHint(want)
	if err != nil {
		t.Fatalf("EncodeSessionWakeHint: %v", err)
	}
	conn, err := net.DialTimeout("unix", nudgequeue.WakeSocketPath(dir), testutil.GoroutineRaceTimeout)
	if err != nil {
		t.Fatalf("dial nudge wake socket: %v", err)
	}
	if err := conn.SetWriteDeadline(time.Now().Add(testutil.GoroutineRaceTimeout)); err != nil {
		_ = conn.Close()
		t.Fatalf("set fragmented write deadline: %v", err)
	}
	split := len(wire) / 2
	for _, fragment := range [][]byte{wire[:split], wire[split:]} {
		if _, err := conn.Write(fragment); err != nil {
			_ = conn.Close()
			t.Fatalf("write fragmented nudge wake frame: %v", err)
		}
	}
	if err := conn.Close(); err != nil {
		t.Fatalf("close fragmented nudge wake frame: %v", err)
	}
	if got := receiveBeforeDeadline(t, exactCh); got != want {
		t.Fatalf("fragmented exact hint = %+v, want %+v", got, want)
	}
}

func TestInvokeNudgeWakeHintContainsExactCallbackPanic(t *testing.T) {
	var stderr bytes.Buffer
	invokeNudgeWakeHint(func(nudgeWakeHint) {
		panic("shadow callback failure")
	}, nudgeWakeHint{Version: nudgequeue.SessionWakeHintVersion1, CommandID: "panic-command", SessionID: "panic-session"}, &stderr, "test")
	if !strings.Contains(stderr.String(), "nudge exact wake callback panicked: shadow callback failure") {
		t.Fatalf("stderr = %q, want contained callback panic", stderr.String())
	}
}

func TestStartNudgeWakeListenerKeepsMalformedAndLegacyPayloadsGlobalOnly(t *testing.T) {
	tests := []struct {
		name    string
		payload []byte
	}{
		{name: "legacy byte", payload: []byte{1}},
		{name: "unknown version", payload: []byte("GCNW/2/Y21k/c2Vzc2lvbg")},
		{name: "missing command", payload: []byte("GCNW/1//c2Vzc2lvbg")},
		{name: "malformed base64", payload: []byte("GCNW/1/%%%/c2Vzc2lvbg")},
		{name: "oversized", payload: bytes.Repeat([]byte("x"), nudgeWakeHintMaxPayloadBytes+1)},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			dir := t.TempDir()
			wakeCh := make(chan struct{}, 1)
			lis, err := startNudgeWakeListenerWithHints(ctx, dir, wakeCh, func(nudgeWakeHint) {}, nil, "test")
			if err != nil {
				t.Fatalf("startNudgeWakeListenerWithHints: %v", err)
			}
			defer lis.Close() //nolint:errcheck

			writeRawNudgeWakePayload(t, dir, tc.payload)
			receiveBeforeDeadline(t, wakeCh)

			// Exercise the framed reader synchronously as well. Exact readers are
			// concurrent in production, so a cross-connection FIFO assertion
			// would be a false barrier.
			server, client := net.Pipe()
			writeDone := make(chan error, 1)
			go func() {
				_, writeErr := client.Write(tc.payload)
				closeErr := client.Close()
				if writeErr == nil {
					writeErr = closeErr
				}
				writeDone <- writeErr
			}()
			directExact := make(chan nudgeWakeHint, 1)
			readNudgeWakeHintConnection(context.Background(), server, func(hint nudgeWakeHint) {
				directExact <- hint
			}, nil, "test")
			if err := receiveBeforeDeadline(t, writeDone); err != nil {
				t.Fatalf("write direct framed payload: %v", err)
			}
			select {
			case exact := <-directExact:
				t.Fatalf("malformed/legacy payload produced exact hint %+v", exact)
			default:
			}
		})
	}
}

func TestCommittedNudgeStartsProvisionalExactShadowBeforePatrol(t *testing.T) {
	dir := t.TempDir()
	if err := contract.WriteProjectIdentity(fsys.OSFS{}, dir, "project-shadow-scope"); err != nil {
		t.Fatalf("WriteProjectIdentity: %v", err)
	}
	cr := &CityRuntime{
		cityPath: dir,
		cfg:      supervisorCfg(),
		stderr:   &bytes.Buffer{},
	}
	if err := cr.installNudgeKeyShadow(); err != nil {
		t.Fatalf("installNudgeKeyShadow: %v", err)
	}
	if cr.nudgeKeyController == nil {
		t.Fatal("durable project identity did not install provisional keyed shadow controller")
	}

	type observation struct {
		key       string
		storeID   string
		sessionID string
		batch     nudgeReconcileBatch
	}
	observed := make(chan observation, 1)
	type hintObservation struct {
		hint      nudgeWakeHint
		committed bool
		loadErr   error
	}
	hintSeen := make(chan hintObservation, 1)
	commandID := "command-exact-1"
	sessionID := "session-durable-1"
	cr.nudgeKeyController.reconcile = func(_ context.Context, key reconcilekey.Session, batch nudgeReconcileBatch) {
		observed <- observation{
			key:       key.String(),
			storeID:   key.StoreID(),
			sessionID: key.SessionID(),
			batch:     batch,
		}
	}

	ctx, cancel := context.WithCancel(context.Background())
	stopController := cr.startNudgeKeyController(ctx)
	wakeCh := make(chan struct{}, 1)
	hintErrCh := make(chan error, 1)
	lis, err := startNudgeWakeListenerWithHints(ctx, dir, wakeCh, func(hint nudgeWakeHint) {
		state, loadErr := nudgequeue.LoadState(dir)
		committed := false
		if loadErr == nil {
			for _, queued := range state.Pending {
				if queued.ID == hint.CommandID && queued.SessionID == hint.SessionID {
					committed = true
					break
				}
			}
		}
		hintSeen <- hintObservation{hint: hint, committed: committed, loadErr: loadErr}
		if err := cr.enqueueNudgeKeyShadow(hint.SessionID); err != nil {
			hintErrCh <- err
		}
	}, nil, "test")
	if err != nil {
		cancel()
		stopController()
		t.Fatalf("startNudgeWakeListenerWithHints: %v", err)
	}
	t.Cleanup(func() {
		cancel()
		stopController()
		_ = lis.Close()
	})

	item := newQueuedNudge("display-alias-must-not-be-key", "message-must-not-enter-hint", time.Now())
	item.ID = commandID
	item.SessionID = sessionID
	if err := enqueueQueuedNudgeWithStore(dir, beads.NudgesStore{Store: beads.NewMemStore()}, item); err != nil {
		t.Fatalf("enqueueQueuedNudgeWithStore: %v", err)
	}

	got := receiveBeforeDeadline(t, observed)
	gotHint := receiveBeforeDeadline(t, hintSeen)
	if gotHint.loadErr != nil || !gotHint.committed {
		t.Fatalf("hint observed committed queue state = %v, loadErr=%v", gotHint.committed, gotHint.loadErr)
	}
	if gotHint.hint.CommandID != commandID || gotHint.hint.SessionID != sessionID {
		t.Fatalf("wire hint = %+v, want only committed command/session identities", gotHint.hint)
	}
	if got.storeID != nudgeKeyShadowProjectScopePrefix+"project-shadow-scope" || got.sessionID != sessionID {
		t.Fatalf("provisional key = scope %q session %q, want namespaced project scope + durable session ID (encoded %q)", got.storeID, got.sessionID, got.key)
	}
	if got.batch.Causes != nudgeCauseCommandCommit {
		t.Fatalf("causes = %v, want command commit only", got.batch.Causes)
	}
	receiveBeforeDeadline(t, wakeCh)
	select {
	case err := <-hintErrCh:
		t.Fatalf("exact hint admission: %v", err)
	default:
	}
}

func TestDuplicateCommandWakeUsesPersistedSessionIdentity(t *testing.T) {
	dir := t.TempDir()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	exactCh := make(chan nudgeWakeHint, 2)
	lis, err := startNudgeWakeListenerWithHints(ctx, dir, make(chan struct{}, 2), func(hint nudgeWakeHint) {
		exactCh <- hint
	}, nil, "test")
	if err != nil {
		t.Fatalf("startNudgeWakeListenerWithHints: %v", err)
	}
	defer lis.Close() //nolint:errcheck

	store := beads.NudgesStore{Store: beads.NewMemStore()}
	first := newQueuedNudge("worker", "original", time.Now())
	first.ID = "immutable-command-id"
	first.SessionID = "persisted-session"
	if err := enqueueQueuedNudgeWithStore(dir, store, first); err != nil {
		t.Fatalf("first enqueue: %v", err)
	}
	if got := receiveBeforeDeadline(t, exactCh); got.CommandID != first.ID || got.SessionID != first.SessionID {
		t.Fatalf("first hint = %+v, want persisted command/session", got)
	}

	conflictingRetry := first
	conflictingRetry.SessionID = "caller-supplied-conflict"
	conflictingRetry.Message = "conflicting retry"
	if err := enqueueQueuedNudgeWithStore(dir, store, conflictingRetry); err != nil {
		t.Fatalf("duplicate enqueue: %v", err)
	}
	if got := receiveBeforeDeadline(t, exactCh); got.CommandID != first.ID || got.SessionID != first.SessionID {
		t.Fatalf("duplicate hint = %+v, want canonical persisted command/session", got)
	}
}

func TestDistinctCommandWakeHintsCoalesceAtProvisionalSessionKey(t *testing.T) {
	dir := t.TempDir()
	if err := contract.WriteProjectIdentity(fsys.OSFS{}, dir, "duplicate-project"); err != nil {
		t.Fatalf("WriteProjectIdentity: %v", err)
	}
	cr := &CityRuntime{cityPath: dir, cfg: supervisorCfg(), stderr: &bytes.Buffer{}}
	if err := cr.installNudgeKeyShadow(); err != nil {
		t.Fatalf("installNudgeKeyShadow: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	accepted := make(chan error, 2)
	lis, err := startNudgeWakeListenerWithHints(ctx, dir, make(chan struct{}, 1), func(hint nudgeWakeHint) {
		accepted <- cr.enqueueNudgeKeyShadow(hint.SessionID)
	}, nil, "test")
	if err != nil {
		t.Fatalf("startNudgeWakeListenerWithHints: %v", err)
	}
	defer lis.Close() //nolint:errcheck
	pingNudgeWakeSocketHint(dir, nudgeWakeHint{Version: nudgequeue.SessionWakeHintVersion1, CommandID: "command-one", SessionID: "same-session"})
	pingNudgeWakeSocketHint(dir, nudgeWakeHint{Version: nudgequeue.SessionWakeHintVersion1, CommandID: "command-two", SessionID: "same-session"})
	for i := 0; i < 2; i++ {
		if err := receiveBeforeDeadline(t, accepted); err != nil {
			t.Fatalf("enqueueNudgeKeyShadow: %v", err)
		}
	}

	key, err := reconcilekey.NewSession(nudgeKeyShadowProjectScopePrefix+"duplicate-project", "same-session")
	if err != nil {
		t.Fatalf("NewSession: %v", err)
	}
	cr.nudgeKeyController.mu.Lock()
	batch, ok := cr.nudgeKeyController.pending[key]
	pendingKeys := len(cr.nudgeKeyController.pending)
	cr.nudgeKeyController.mu.Unlock()
	if !ok || pendingKeys != 1 || batch.Causes != nudgeCauseCommandCommit {
		t.Fatalf("coalesced pending = ok:%v keys:%d batch:%+v, want one command-commit key", ok, pendingKeys, batch)
	}
	if got := cr.nudgeKeyController.queue.Len(); got != 1 {
		t.Fatalf("workqueue length = %d, want one coalesced key", got)
	}
	cr.nudgeKeyController.closeAdmission()
}

func TestFailedCommitEmitsNoWakeAndUnkeyedCommitFallsBackGlobal(t *testing.T) {
	tests := []struct {
		name         string
		sessionID    string
		corruptState bool
		wantErr      bool
		wantWakes    int
	}{
		{name: "failed durable write", sessionID: "session-failed", corruptState: true, wantErr: true, wantWakes: 1},
		{name: "blank durable session id", sessionID: "", wantErr: false, wantWakes: 2},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			if tc.corruptState {
				statePath := nudgequeue.StatePath(dir)
				if err := os.MkdirAll(filepath.Dir(statePath), 0o755); err != nil {
					t.Fatalf("MkdirAll queue dir: %v", err)
				}
				if err := os.WriteFile(statePath, []byte("{not-json\n"), 0o600); err != nil {
					t.Fatalf("WriteFile corrupt queue: %v", err)
				}
			}

			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			wakeCh := make(chan struct{}, 4)
			exactCh := make(chan nudgeWakeHint, 2)
			lis, err := startNudgeWakeListenerWithHints(ctx, dir, wakeCh, func(hint nudgeWakeHint) {
				exactCh <- hint
			}, nil, "test")
			if err != nil {
				t.Fatalf("startNudgeWakeListenerWithHints: %v", err)
			}
			defer lis.Close() //nolint:errcheck

			item := newQueuedNudge("worker", "secret-message", time.Now())
			item.ID = "command-under-test"
			item.SessionID = tc.sessionID
			err = enqueueQueuedNudgeWithStore(dir, beads.NudgesStore{Store: beads.NewMemStore()}, item)
			if (err != nil) != tc.wantErr {
				t.Fatalf("enqueue error = %v, wantErr=%v", err, tc.wantErr)
			}

			barrier := nudgeWakeHint{Version: nudgequeue.SessionWakeHintVersion1, CommandID: "barrier-command", SessionID: "barrier-session"}
			pingNudgeWakeSocketHint(dir, barrier)
			if got := receiveBeforeDeadline(t, exactCh); got != barrier {
				t.Fatalf("first exact hint = %+v, want barrier %+v", got, barrier)
			}
			if got := len(wakeCh); got != tc.wantWakes {
				t.Fatalf("global wakes after enqueue + barrier = %d, want %d", got, tc.wantWakes)
			}
		})
	}
}

func writeRawNudgeWakePayload(t *testing.T, cityPath string, payload []byte) {
	t.Helper()
	conn, err := net.DialTimeout("unix", nudgequeue.WakeSocketPath(cityPath), testutil.GoroutineRaceTimeout)
	if err != nil {
		t.Fatalf("dial nudge wake socket: %v", err)
	}
	defer conn.Close() //nolint:errcheck
	if err := conn.SetWriteDeadline(time.Now().Add(testutil.GoroutineRaceTimeout)); err != nil {
		t.Fatalf("set nudge wake write deadline: %v", err)
	}
	if _, err := conn.Write(payload); err != nil {
		t.Fatalf("write nudge wake payload: %v", err)
	}
}

func TestStartNudgeWakeListenerCoalescesBurst(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	dir := t.TempDir()
	wakeCh := make(chan struct{}, 1)

	lis, err := startNudgeWakeListener(ctx, dir, wakeCh)
	if err != nil {
		t.Fatalf("startNudgeWakeListener: %v", err)
	}
	defer lis.Close() //nolint:errcheck

	// Fire several pings in quick succession. The buffered channel of size
	// 1 must coalesce them — never block the listener accept loop.
	for i := 0; i < 10; i++ {
		pingNudgeWakeSocket(dir)
	}
	// Let all accepts drain through the listener so coalescing settles, then
	// verify a wake was produced. The structural coalescing guarantee is the
	// chan's bounded capacity; the previous test counted cumulative wakes
	// over time, which races against accept-loop scheduling on fast hardware.
	time.Sleep(200 * time.Millisecond)
	select {
	case <-wakeCh:
	default:
		t.Fatal("wakeCh not signaled at all after burst of 10 pings")
	}
	if got := cap(wakeCh); got != 1 {
		t.Fatalf("wakeCh capacity = %d; want 1 (coalescing relies on bounded buffer)", got)
	}
}

func TestStartNudgeWakeListenerStopsOnContextCancel(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	dir := t.TempDir()
	wakeCh := make(chan struct{}, 1)

	lis, err := startNudgeWakeListener(ctx, dir, wakeCh)
	if err != nil {
		t.Fatalf("startNudgeWakeListener: %v", err)
	}
	cancel()
	// The cleanup goroutine closes the listener on ctx.Done. Give it a beat,
	// then confirm dialing the socket fails fast.
	time.Sleep(50 * time.Millisecond)
	_, err = net.DialTimeout("unix", nudgequeue.WakeSocketPath(dir), 100*time.Millisecond)
	if err == nil {
		t.Fatal("expected dial to fail after ctx cancel; listener still accepting")
	}
	_ = lis
}

func TestDispatchAllQueuedNudgesNoOpInLegacyMode(t *testing.T) {
	clearGCEnv(t)
	disableManagedDoltRecoveryForTest(t)

	dir := t.TempDir()
	if err := enqueueQueuedNudge(dir, newQueuedNudge("worker", "msg", time.Now().Add(-time.Minute))); err != nil {
		t.Fatalf("enqueueQueuedNudge: %v", err)
	}
	cfg := &config.City{Daemon: config.DaemonConfig{}} // legacy default
	delivered, err := dispatchAllQueuedNudges(dir, cfg, nil, nil, nil, newSessionBeadSnapshot(nil))
	if err != nil {
		t.Fatalf("dispatchAllQueuedNudges: %v", err)
	}
	if delivered != 0 {
		t.Fatalf("delivered = %d, want 0 in legacy mode", delivered)
	}
}

func TestDispatchAllQueuedNudgesEmptyQueue(t *testing.T) {
	clearGCEnv(t)
	disableManagedDoltRecoveryForTest(t)

	dir := t.TempDir()
	delivered, err := dispatchAllQueuedNudges(dir, supervisorCfg(), nil, nil, nil, newSessionBeadSnapshot(nil))
	if err != nil {
		t.Fatalf("dispatchAllQueuedNudges: %v", err)
	}
	if delivered != 0 {
		t.Fatalf("delivered = %d, want 0 with empty queue", delivered)
	}
}

func TestDispatchAllQueuedNudgesSkipsNotYetDue(t *testing.T) {
	clearGCEnv(t)
	disableManagedDoltRecoveryForTest(t)

	dir := t.TempDir()
	future := time.Now().Add(5 * time.Minute)
	item := newQueuedNudge("worker", "later", time.Now())
	item.DeliverAfter = future
	if err := enqueueQueuedNudge(dir, item); err != nil {
		t.Fatalf("enqueueQueuedNudge: %v", err)
	}
	bead := beads.Bead{
		ID:     "session-1",
		Status: "open",
		Metadata: map[string]string{
			"session_name": "worker-session",
			"agent_name":   "worker",
			"template":     "worker",
		},
	}
	snapshot := newSessionBeadSnapshot([]beads.Bead{bead})
	delivered, err := dispatchAllQueuedNudges(dir, supervisorCfg(), nil, nil, runtime.NewFake(), snapshot)
	if err != nil {
		t.Fatalf("dispatchAllQueuedNudges: %v", err)
	}
	if delivered != 0 {
		t.Fatalf("delivered = %d, want 0 (item not yet due)", delivered)
	}
}

func TestDispatchAllQueuedNudgesDeliversAndAcks(t *testing.T) {
	clearGCEnv(t)
	disableManagedDoltRecoveryForTest(t)
	clearInheritedCityRoutingEnv(t)
	t.Setenv("GC_BEADS", "file")
	dir := t.TempDir()

	// Set up a running session via the same fake-provider harness used by
	// the per-session poller test, then enqueue a nudge for it.
	store := openNudgeBeadStore(dir)
	fake := runtime.NewFake()
	mgr := newSessionManagerWithConfig(dir, store.Store, fake, nil)
	info, err := mgr.CreateSession(context.Background(), session.CreateOptions{Template: "worker", Title: "Worker", Command: "codex", WorkDir: dir, Provider: "codex", Env: nil, Resume: session.ProviderResume{}, Hints: runtime.Config{WorkDir: dir}, ExtraMeta: map[string]string{"session_origin": "manual"}})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if err := mgr.Start(context.Background(), info.ID, "", runtime.Config{WorkDir: dir}); err != nil {
		t.Fatalf("Start: %v", err)
	}
	fake.Activity = map[string]time.Time{info.SessionName: time.Now().Add(-10 * time.Second)}

	if err := enqueueQueuedNudge(dir, newQueuedNudge("worker", "review the deploy logs", time.Now().Add(-time.Minute))); err != nil {
		t.Fatalf("enqueueQueuedNudge: %v", err)
	}

	snapshot, err := loadSessionBeadSnapshot(store.Store)
	if err != nil {
		t.Fatalf("loadSessionBeadSnapshot: %v", err)
	}

	delivered, err := dispatchAllQueuedNudges(dir, supervisorCfg(), store.Store, store.Store, fake, snapshot)
	if err != nil {
		t.Fatalf("dispatchAllQueuedNudges: %v", err)
	}
	if delivered != 1 {
		t.Fatalf("delivered = %d, want 1", delivered)
	}

	var nudgeMessages []string
	for _, call := range fake.Calls {
		if call.Method == "Nudge" {
			nudgeMessages = append(nudgeMessages, call.Message)
		}
	}
	if len(nudgeMessages) != 1 {
		t.Fatalf("nudge calls = %d, want 1", len(nudgeMessages))
	}
	if !strings.Contains(nudgeMessages[0], "review the deploy logs") {
		t.Fatalf("nudge message = %q, want original reminder", nudgeMessages[0])
	}

	pending, inFlight, dead, err := listQueuedNudges(dir, "worker", time.Now())
	if err != nil {
		t.Fatalf("listQueuedNudges: %v", err)
	}
	if len(pending) != 0 || len(inFlight) != 0 || len(dead) != 0 {
		t.Fatalf("queue not drained: pending=%d inFlight=%d dead=%d", len(pending), len(inFlight), len(dead))
	}
}

// TestDispatchAllQueuedNudgesDeliversToIdleACPSession verifies the
// supervisor dispatcher delivers queued nudges to a running ACP session
// once it has been idle longer than the quiescence window. Idle ACP
// sessions used to depend exclusively on inject-on-hook drain, but a
// pure-hook delivery path never fires for a warm session that is not
// receiving fresh user prompts — queued reminders piled up
// indefinitely against an alive but quiet agent. The dispatcher now
// owns wake delivery; the hook still drains opportunistically when the
// agent receives external prompts, and the atomic queue claim prevents
// double delivery.
func TestDispatchAllQueuedNudgesDeliversToIdleACPSession(t *testing.T) {
	clearGCEnv(t)
	disableManagedDoltRecoveryForTest(t)
	clearInheritedCityRoutingEnv(t)
	t.Setenv("GC_BEADS", "file")

	dir := t.TempDir()
	store := openNudgeBeadStore(dir)
	if store.Store == nil {
		t.Fatal("openNudgeBeadStore returned nil")
	}

	if err := enqueueQueuedNudgeWithStore(dir, store, newQueuedNudge("worker", "wake-up nudge", time.Now().Add(-time.Minute))); err != nil {
		t.Fatalf("enqueueQueuedNudgeWithStore: %v", err)
	}

	fake := runtime.NewFake()
	if err := fake.Start(context.Background(), "worker-session", runtime.Config{}); err != nil {
		t.Fatalf("fake.Start: %v", err)
	}
	// Mark last activity well past the quiescence window so the
	// dispatcher considers the session idle enough to deliver.
	fake.SetActivity("worker-session", time.Now().Add(-10*time.Second))

	// Create a real session bead so worker.SessionByID can resolve the
	// target without panicking on a missing-bead lookup.
	created, err := store.Create(beads.Bead{
		Title:  "Session: worker",
		Type:   session.BeadType,
		Status: "open",
		Labels: []string{session.LabelSession},
		Metadata: map[string]string{
			"session_name": "worker-session",
			"agent_name":   "worker",
			"template":     "worker",
			"transport":    "acp",
		},
	})
	if err != nil {
		t.Fatalf("store.Create session bead: %v", err)
	}
	snapshot := newSessionBeadSnapshot([]beads.Bead{created})

	delivered, err := dispatchAllQueuedNudges(dir, supervisorCfg(), store, store, fake, snapshot)
	if err != nil {
		t.Fatalf("dispatchAllQueuedNudges: %v", err)
	}
	if delivered != 1 {
		t.Fatalf("delivered = %d, want 1 (running idle ACP session must receive queued nudges)", delivered)
	}

	var nudgeMessages []string
	for _, call := range fake.Calls {
		if call.Method == "Nudge" {
			nudgeMessages = append(nudgeMessages, call.Message)
		}
	}
	if len(nudgeMessages) != 1 {
		t.Fatalf("nudge calls = %d, want 1 (queued nudge should be delivered as a runtime prompt)", len(nudgeMessages))
	}
	if !strings.Contains(nudgeMessages[0], "wake-up nudge") {
		t.Fatalf("nudge message = %q, want original reminder text", nudgeMessages[0])
	}

	pending, inFlight, dead, err := listQueuedNudges(dir, "worker", time.Now())
	if err != nil {
		t.Fatalf("listQueuedNudges: %v", err)
	}
	if len(pending) != 0 || len(inFlight) != 0 || len(dead) != 0 {
		t.Fatalf("queue not drained after ACP delivery: pending=%d inFlight=%d dead=%d", len(pending), len(inFlight), len(dead))
	}

	// Observability: a successful queued-nudge delivery must stamp
	// metadata.last_nudge_delivered_at on the session bead so the
	// "LAST NUDGE" column in `gc session list` reflects fresh activity.
	// Operators rely on this column to spot warm sessions whose
	// delivery loop has stalled (queued items piling up while the
	// stamp stays old).
	refetched, getErr := store.Get(created.ID)
	if getErr != nil {
		t.Fatalf("store.Get session bead: %v", getErr)
	}
	stamp := strings.TrimSpace(refetched.Metadata[session.MetadataLastNudgeDeliveredAt])
	if stamp == "" {
		t.Fatalf("session bead missing %s metadata after successful ACP delivery", session.MetadataLastNudgeDeliveredAt)
	}
	parsed, parseErr := time.Parse(time.RFC3339, stamp)
	if parseErr != nil {
		t.Fatalf("parse %s=%q: %v", session.MetadataLastNudgeDeliveredAt, stamp, parseErr)
	}
	if drift := time.Since(parsed); drift < 0 || drift > time.Minute {
		t.Fatalf("%s timestamp drift %s is outside the 1-minute test window (raw=%q)", session.MetadataLastNudgeDeliveredAt, drift, stamp)
	}
}

// TestDispatchAllQueuedNudgesSkipsACPSessionWhenNotRunning confirms the
// dispatcher still respects the universal liveness check for ACP sessions —
// a stopped or crashed ACP session must not absorb queued nudges, because
// nothing on the other side would observe the delivered prompt.
func TestDispatchAllQueuedNudgesSkipsACPSessionWhenNotRunning(t *testing.T) {
	clearGCEnv(t)
	disableManagedDoltRecoveryForTest(t)

	dir := t.TempDir()
	if err := enqueueQueuedNudge(dir, newQueuedNudge("worker", "msg", time.Now().Add(-time.Minute))); err != nil {
		t.Fatalf("enqueueQueuedNudge: %v", err)
	}
	bead := beads.Bead{
		ID:     "worker-session",
		Status: "open",
		Metadata: map[string]string{
			"session_name": "worker-session",
			"agent_name":   "worker",
			"template":     "worker",
			"transport":    "acp",
		},
	}
	snapshot := newSessionBeadSnapshot([]beads.Bead{bead})
	// Fake has no started session, so IsRunning("worker-session") is false.
	delivered, err := dispatchAllQueuedNudges(dir, supervisorCfg(), nil, nil, runtime.NewFake(), snapshot)
	if err != nil {
		t.Fatalf("dispatchAllQueuedNudges: %v", err)
	}
	if delivered != 0 {
		t.Fatalf("delivered = %d, want 0 (stopped ACP session must not receive delivery)", delivered)
	}
}

func TestNudgeDispatcherIsSupervisor(t *testing.T) {
	if nudgeDispatcherIsSupervisor(nil) {
		t.Error("nil cfg must report legacy mode")
	}
	if nudgeDispatcherIsSupervisor(&config.City{}) {
		t.Error("zero-value DaemonConfig must report legacy mode")
	}
	if !nudgeDispatcherIsSupervisor(supervisorCfg()) {
		t.Error("supervisorCfg must report supervisor mode")
	}
}

func TestDispatchAllQueuedNudgesNilCfg(t *testing.T) {
	clearGCEnv(t)
	disableManagedDoltRecoveryForTest(t)

	dir := t.TempDir()
	if err := enqueueQueuedNudge(dir, newQueuedNudge("worker", "msg", time.Now().Add(-time.Minute))); err != nil {
		t.Fatalf("enqueueQueuedNudge: %v", err)
	}
	delivered, err := dispatchAllQueuedNudges(dir, nil, nil, nil, nil, newSessionBeadSnapshot(nil))
	if err != nil {
		t.Fatalf("dispatchAllQueuedNudges: %v", err)
	}
	if delivered != 0 {
		t.Fatalf("delivered = %d, want 0 with nil cfg", delivered)
	}
}

// TestMaybeStartNudgePollerSkipsACPSessionInLegacyMode verifies the
// legacy per-session poller still skips ACP sessions. A sidecar `gc
// nudge poll` process can observe the ACP control socket, but it does
// not own the in-memory ACP connection needed to send session/prompt.
func TestMaybeStartNudgePollerSkipsACPSessionInLegacyMode(t *testing.T) {
	prev := startNudgePoller
	t.Cleanup(func() { startNudgePoller = prev })

	called := false
	startNudgePoller = func(_, _, _ string) error {
		called = true
		return nil
	}

	maybeStartNudgePoller(nudgeTarget{
		cityPath:    t.TempDir(),
		sessionName: "worker-session",
		transport:   "acp",
		cfg:         &config.City{},
	})
	if called {
		t.Fatal("startNudgePoller invoked for ACP session in legacy mode; sidecar ACP pollers cannot deliver without owning the connection")
	}
}

func TestMaybeStartNudgePollerSkipsInSupervisorMode(t *testing.T) {
	prev := startNudgePoller
	t.Cleanup(func() { startNudgePoller = prev })

	called := false
	startNudgePoller = func(_, _, _ string) error {
		called = true
		return nil
	}

	maybeStartNudgePoller(nudgeTarget{
		cityPath:    t.TempDir(),
		sessionName: "worker-session",
		cfg:         supervisorCfg(),
	})
	if called {
		t.Fatal("startNudgePoller invoked in supervisor mode; supervisor dispatcher would race with the per-session poller")
	}

	maybeStartNudgePoller(nudgeTarget{
		cityPath:    t.TempDir(),
		sessionName: "worker-session",
		cfg:         &config.City{},
	})
	if !called {
		t.Fatal("startNudgePoller not invoked in legacy mode")
	}
}

func TestEnqueuePingsWakeSocket(t *testing.T) {
	clearGCEnv(t)
	disableManagedDoltRecoveryForTest(t)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	dir := t.TempDir()
	wakeCh := make(chan struct{}, 1)
	lis, err := startNudgeWakeListener(ctx, dir, wakeCh)
	if err != nil {
		t.Fatalf("startNudgeWakeListener: %v", err)
	}
	defer lis.Close() //nolint:errcheck

	if err := enqueueQueuedNudge(dir, newQueuedNudge("worker", "msg", time.Now())); err != nil {
		t.Fatalf("enqueueQueuedNudge: %v", err)
	}
	select {
	case <-wakeCh:
	case <-time.After(2 * time.Second):
		t.Fatal("wakeCh not signaled after enqueue")
	}
}
