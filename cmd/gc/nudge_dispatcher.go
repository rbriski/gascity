package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"runtime/debug"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/nudgequeue"
	"github.com/gastownhall/gascity/internal/runtime"
)

// pingNudgeWakeSocketDialTimeout bounds how long a producer waits to dial
// the supervisor wake socket. Producers must not block on a stale or
// missing socket — legacy-mode cities and pre-start producers expect the
// dial to fail fast.
const pingNudgeWakeSocketDialTimeout = 200 * time.Millisecond

// maxConcurrentNudgeWakeHintReaders bounds slow or malicious same-UID clients
// without letting one connection head-of-line block legacy global wakes. When
// all slots are occupied, the accepted connection still produces the global
// wake and is then closed without exact decoding.
const maxConcurrentNudgeWakeHintReaders = 8

type nudgeWakeHint = nudgequeue.SessionWakeHint

const nudgeWakeHintMaxPayloadBytes = nudgequeue.MaxSessionWakeHintWireBytes

// pingNudgeWakeSocket sends a best-effort wake signal to the supervisor's
// nudge dispatcher. Callers invoke this after enqueueing a queued nudge so
// the supervisor delivers within sub-second latency instead of waiting for
// the next patrol tick. Failures (no listener, dial timeout, write error)
// are intentionally silent: the patrol-tick fallback in supervisor mode
// and the per-session poller in legacy mode each guarantee eventual
// delivery without the wake.
func pingNudgeWakeSocket(cityPath string) {
	pingNudgeWakeSocketPayload(cityPath, []byte{1})
}

// pingNudgeWakeSocketHint sends an exact-target advisory when both durable IDs
// fit the versioned protocol. Invalid or legacy identifiers retain the global
// one-byte wake; a hint can never make durable enqueue fail.
func pingNudgeWakeSocketHint(cityPath string, hint nudgeWakeHint) {
	payload, err := nudgequeue.EncodeSessionWakeHint(hint)
	if err != nil {
		pingNudgeWakeSocket(cityPath)
		return
	}
	pingNudgeWakeSocketPayload(cityPath, payload)
}

func pingNudgeWakeSocketPayload(cityPath string, payload []byte) {
	if cityPath == "" {
		return
	}
	path := nudgequeue.WakeSocketPath(cityPath)
	conn, err := net.DialTimeout("unix", path, pingNudgeWakeSocketDialTimeout)
	if err != nil {
		return
	}
	defer conn.Close() //nolint:errcheck // best-effort signaling
	_ = conn.SetWriteDeadline(time.Now().Add(pingNudgeWakeSocketDialTimeout))
	for len(payload) > 0 {
		n, err := conn.Write(payload)
		if err != nil || n == 0 {
			return
		}
		payload = payload[n:]
	}
}

// startNudgeWakeListener opens the supervisor wake socket and spawns an
// accept loop that signals wakeCh on every connection. The returned
// listener is closed when ctx is canceled. Returns nil, nil when the
// socket cannot be opened (e.g. permission, path-too-long); callers fall
// back to patrol-interval dispatching.
func startNudgeWakeListener(ctx context.Context, cityPath string, wakeCh chan<- struct{}) (net.Listener, error) {
	return startNudgeWakeListenerWithHints(ctx, cityPath, wakeCh, nil, nil, "")
}

// startNudgeWakeListenerWithHints preserves the global wake for every accepted
// connection and additionally decodes valid exact-target hints. The exact
// callback is advisory, may run concurrently, and must remain O(1) and
// thread-safe. Malformed input or saturated exact-reader capacity never
// suppresses the legacy dispatcher signal.
func startNudgeWakeListenerWithHints(ctx context.Context, cityPath string, wakeCh chan<- struct{}, onExact func(nudgeWakeHint), stderr io.Writer, logPrefix string) (net.Listener, error) {
	path := nudgequeue.WakeSocketPath(cityPath)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, fmt.Errorf("creating nudge wake dir: %w", err)
	}
	// A stale socket from a prior supervisor crash blocks Listen with
	// "address already in use". Removing it is safe because flock-based
	// queue access protects state; the socket carries no data of its own.
	_ = os.Remove(path)
	lis, err := net.Listen("unix", path)
	if err != nil {
		return nil, fmt.Errorf("listening on nudge wake socket: %w", err)
	}
	// TOCTOU: there is a narrow window between Listen and Chmod where
	// the socket exists at the umask-default permissions and a co-local
	// user could connect. The versioned payload carries only advisory command
	// and session IDs, never authority or message content; worst case in this
	// phase is a spurious legacy dispatch plus effect-free shadow enqueue. A
	// future hardening pass could set
	// umask before Listen, or use platform-specific abstract namespace
	// sockets where supported.
	if err := os.Chmod(path, 0o600); err != nil {
		_ = lis.Close()
		return nil, fmt.Errorf("chmod nudge wake socket: %w", err)
	}
	go func() {
		<-ctx.Done()
		_ = lis.Close()
	}()
	readerSlots := make(chan struct{}, maxConcurrentNudgeWakeHintReaders)
	readConnection := func(conn net.Conn) {
		readNudgeWakeHintConnection(ctx, conn, onExact, stderr, logPrefix)
	}
	go func() {
		for {
			conn, err := lis.Accept()
			if err != nil {
				if errors.Is(err, net.ErrClosed) {
					return
				}
				if stderr != nil {
					fmt.Fprintf(stderr, "%s: nudge wake accept: %v\n", logPrefix, err) //nolint:errcheck
				}
				continue
			}
			dispatchAcceptedNudgeWake(conn, wakeCh, readerSlots, readConnection)
		}
	}()
	return lis, nil
}

func dispatchAcceptedNudgeWake(conn net.Conn, wakeCh chan<- struct{}, readerSlots chan struct{}, readConnection func(net.Conn)) {
	// Acceptance itself is the legacy signal. Never wait for an exact frame
	// before waking the established global dispatcher.
	select {
	case wakeCh <- struct{}{}:
	default:
		// Already-pending wake covers this enqueue; coalesced.
	}
	select {
	case readerSlots <- struct{}{}:
		go func() {
			defer func() { <-readerSlots }()
			readConnection(conn)
		}()
	default:
		_ = conn.Close()
	}
}

func readNudgeWakeHintConnection(ctx context.Context, conn net.Conn, onExact func(nudgeWakeHint), stderr io.Writer, logPrefix string) {
	defer conn.Close() //nolint:errcheck // best-effort advisory connection
	if onExact == nil {
		return
	}
	// One connection is one frame. The limit and EOF framing handle fragmented
	// stream writes without allowing an untrusted local client to allocate an
	// unbounded buffer.
	_ = conn.SetReadDeadline(time.Now().Add(100 * time.Millisecond))
	payload, err := io.ReadAll(io.LimitReader(conn, nudgeWakeHintMaxPayloadBytes+1))
	if err != nil || ctx.Err() != nil {
		return
	}
	if hint, ok := nudgequeue.DecodeSessionWakeHint(payload); ok {
		invokeNudgeWakeHint(onExact, hint, stderr, logPrefix)
	}
}

func invokeNudgeWakeHint(onExact func(nudgeWakeHint), hint nudgeWakeHint, stderr io.Writer, logPrefix string) {
	defer func() {
		if recovered := recover(); recovered != nil && stderr != nil {
			fmt.Fprintf(stderr, "%s: nudge exact wake callback panicked: %v\n%s", logPrefix, recovered, debug.Stack()) //nolint:errcheck // preserve listener; legacy wake already fired
		}
	}()
	onExact(hint)
}

// dispatchAllQueuedNudges runs one supervisor-side dispatcher pass: scan
// the queue for pending agents, resolve each to a nudgeTarget via
// sessionBeads, and try delivery. Returns the number of targets that
// successfully delivered at least one item.
//
// This is a no-op when the dispatcher is configured for "legacy" mode —
// the per-session `gc nudge poll` processes own delivery in that case.
func dispatchAllQueuedNudges(cityPath string, cfg *config.City, store, sessStore beads.Store, sp runtime.Provider, sessionBeads *sessionBeadSnapshot) (int, error) {
	if cfg == nil || sessionBeads == nil || cityPath == "" {
		return 0, nil
	}
	if !nudgeDispatcherIsSupervisor(cfg) {
		return 0, nil
	}
	state, err := nudgequeue.LoadState(cityPath)
	if err != nil {
		return 0, fmt.Errorf("loading nudge queue: %w", err)
	}
	if len(state.Pending) == 0 && len(state.InFlight) == 0 {
		return 0, nil
	}
	now := time.Now()
	pendingAgents := make(map[string]bool, len(state.Pending))
	for _, item := range state.Pending {
		if item.Agent == "" {
			continue
		}
		if !item.DeliverAfter.IsZero() && item.DeliverAfter.After(now) {
			continue
		}
		pendingAgents[item.Agent] = true
	}
	// In-flight items with expired leases are recoverable on the next
	// claim attempt. Including their agents lets us retry without waiting
	// for the patrol tick to discover them.
	for _, item := range state.InFlight {
		if item.Agent == "" {
			continue
		}
		if item.LeaseUntil.IsZero() || !item.LeaseUntil.Before(now) {
			continue
		}
		pendingAgents[item.Agent] = true
	}
	if len(pendingAgents) == 0 {
		return 0, nil
	}

	// The dispatcher receives the nudges-class store (store) PLUS the session-class
	// store (sessStore) the caller resolved from the WORK store — the controller
	// threads cr.sessionsBeadStore().Store, whose fallback is the work store, NOT
	// the nudges store. The session observe below and the queue-delivery path's
	// session ops route through sessStore; the queue record/dead-letter stays on
	// store. Identity today; corrects the pre-existing controller-side class mix
	// (deriving sessStore from the nudges base would mis-resolve session beads once
	// nudges relocates independently of sessions).
	delivered := 0
	var firstErr error
	for _, info := range sessionBeads.OpenInfos() {
		target := resolveNudgeTargetFromSessionInfo(cityPath, cfg, info)
		if target.sessionName == "" {
			continue
		}
		// ACP sessions also flow through this dispatcher. The inject-on-hook
		// drain path still catches deliveries when the agent receives external
		// prompts, but a warm-idle ACP session never fires its hook on its
		// own — queued patrol wisps would otherwise pile up forever. The
		// atomic queue claim in claimDueQueuedNudgesForTarget guarantees a
		// nudge is delivered exactly once across the dispatcher + drain paths.
		matched := false
		for _, key := range target.queueKeys() {
			if pendingAgents[key] {
				matched = true
				break
			}
		}
		if !matched {
			continue
		}
		obs, err := workerObserveNudgeTarget(target, sessStore, sp)
		if err != nil {
			if firstErr == nil {
				firstErr = err
			}
			continue
		}
		if !obs.Running {
			continue
		}
		ok, err := tryDeliverQueuedNudgesByPoller(target, store, sessStore, sp, defaultNudgePollQuiescence, obs)
		if err != nil && firstErr == nil {
			firstErr = err
		}
		if ok {
			delivered++
		}
	}
	return delivered, firstErr
}
