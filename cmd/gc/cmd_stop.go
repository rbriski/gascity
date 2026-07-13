package main

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/citylayout"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/events"
	"github.com/gastownhall/gascity/internal/runtime"
	"github.com/gastownhall/gascity/internal/session"
	"github.com/spf13/cobra"
)

func newStopCmd(stdout, stderr io.Writer) *cobra.Command {
	var wallClockTimeout time.Duration
	var force bool
	var jsonOut bool
	cmd := &cobra.Command{
		Use:   "stop [path|name]",
		Short: "Stop all agent sessions in the city",
		Long: `Stop all agent sessions in the city with graceful shutdown.

Sends interrupt signals to running agents, waits for the configured
shutdown timeout, then force-kills any remaining sessions. Also stops
the Dolt server and cleans up orphan sessions. If a controller is
running, delegates shutdown to it.

Use --timeout=DURATION as one command completion deadline/SLO. It bounds
admission of each new stop effect; if a native provider call enters before
the deadline, gc keeps lifecycle ownership and waits for that call to return.
It is not a hard wall-clock cap. The default budgets configured interrupt and
stop waves, the shutdown grace wait, and a second orphan cleanup pass. Use
--force to skip the interrupt grace period and go straight to kill.`,
		Args:              cobra.MaximumNArgs(1),
		ValidArgsFunction: completeCityNames,
		RunE: func(_ *cobra.Command, args []string) error {
			if cmdStopJSON(args, stdout, stderr, wallClockTimeout, force, jsonOut) != 0 {
				return errExit
			}
			return nil
		},
	}
	cmd.Flags().DurationVar(&wallClockTimeout, "timeout", 0, "completion deadline/SLO for the stop sequence (0 = derive from city config)")
	cmd.Flags().BoolVar(&force, "force", false, "skip the interrupt grace period and force-kill all sessions immediately")
	cmd.Flags().BoolVar(&jsonOut, "json", false, "emit JSONL summary")
	return cmd
}

var (
	sessionProviderForStopCity           = newSessionProviderForCity
	openCityStoreForStop                 = openCityStoreAt
	stopCompletionNow                    = time.Now
	acquireStopControllerUntilForCommand = func(cityPath string, result controllerStopResult, deadline time.Time, timeoutLabel time.Duration) (*controllerLockLease, error) {
		ops := defaultControllerLockWaitOps()
		ops.now = stopCompletionNow
		switch result.outcome {
		case controllerStopAcknowledged:
			return waitForControllerExitAndAcquireUntilWithOps(cityPath, result, deadline, timeoutLabel, ops)
		case controllerStopDefinitePreEntryUnavailable:
			return acquireControllerLockForStopUntilWithOps(cityPath, result, deadline, ops)
		default:
			return nil, fmt.Errorf("%w: cannot acquire ownership for outcome %s", errControllerStopOwnershipUnproven, result.outcome)
		}
	}
	closeEventRecorderForStop = func(rec events.Recorder) error {
		closer, ok := rec.(interface{ Close() error })
		if !ok {
			return nil
		}
		return closer.Close()
	}
)

var errStopCompletionDeadline = errors.New("stop completion deadline expired")

// stopCompletionBudget carries one absolute, monotonic command deadline across
// every direct-stop effect. An entered native call remains owned until return;
// the deadline only denies the next effect and makes the command fail.
type stopCompletionBudget struct {
	deadline time.Time
	timeout  time.Duration
	now      func() time.Time
}

func newStopCompletionBudget(started time.Time, timeout time.Duration, now func() time.Time) stopCompletionBudget {
	return stopCompletionBudget{deadline: started.Add(timeout), timeout: timeout, now: now}
}

func (b stopCompletionBudget) remaining() time.Duration {
	if b.now == nil {
		return 0
	}
	remaining := b.deadline.Sub(b.now())
	if remaining <= 0 {
		return 0
	}
	return remaining
}

func (b stopCompletionBudget) expired() bool { return b.remaining() == 0 }

func stopEffectAdmitted(budget *stopCompletionBudget) bool {
	return budget == nil || !budget.expired()
}

func reportStopCompletionDeadline(stderr io.Writer, timeout time.Duration) {
	fmt.Fprintf(stderr, "gc stop: stop sequence exceeded its completion deadline of %s; cleanup remained joined until entered provider operations returned — retry with --force if stop is wedged, or raise --timeout for large stop sets\n", timeout) //nolint:errcheck
}

// cmdStop stops the city by terminating all configured agent sessions.
// If a path is given, operates there; otherwise uses cwd.
//
// wallClockTimeout is the command's completion deadline/SLO; if 0, a default
// derived from cfg.Daemon.ShutdownTimeoutDuration is used. An entered native
// call remains joined after that deadline until it returns. force=true skips
// the interrupt grace period (gracefulStopAll runs with timeout=0, going
// straight to kill).
func cmdStop(args []string, stdout, stderr io.Writer, wallClockTimeout time.Duration, force bool) int {
	return cmdStopJSON(args, stdout, stderr, wallClockTimeout, force, false)
}

func cmdStopJSON(args []string, stdout, stderr io.Writer, wallClockTimeout time.Duration, force bool, jsonOut bool) (exitCode int) {
	stopStarted := stopCompletionNow()
	cityPath, err := resolveStopCityPath(args)
	if err != nil {
		fmt.Fprintf(stderr, "gc stop: %v\n", err) //nolint:errcheck // best-effort stderr
		return 1
	}
	completionTimeout := stopCompletionTimeoutAtEntry(cityPath, wallClockTimeout)
	completionBudget := newStopCompletionBudget(stopStarted, completionTimeout, stopCompletionNow)
	deadlineFailure := func() int {
		reportStopCompletionDeadline(stderr, completionTimeout)
		return 1
	}
	if completionBudget.expired() {
		return deadlineFailure()
	}

	stopStdout := stdout
	if jsonOut {
		stopStdout = io.Discard
	}

	var stopLease *controllerLockLease
	leaseReleased := false
	releaseStopLease := func() error {
		if leaseReleased || stopLease == nil {
			return nil
		}
		leaseReleased = true
		return stopLease.Close()
	}
	defer func() {
		if err := releaseStopLease(); err != nil {
			fmt.Fprintf(stderr, "gc stop: releasing controller ownership: %v\n", err) //nolint:errcheck
			exitCode = 1
		}
	}()

	unregisterResult := unregisterCityFromSupervisorWithForceResultUntil(
		cityPath,
		stopStdout,
		stderr,
		"gc stop",
		force,
		completionBudget.deadline,
		completionTimeout,
	)
	stopLease = unregisterResult.ownership
	if completionBudget.expired() {
		return deadlineFailure()
	}
	var stopResult controllerStopResult
	hasStopResult := false
	switch unregisterResult.state {
	case supervisorUnregisterManagedCleanupComplete:
		if stopLease == nil {
			if _, statErr := os.Stat(cityPath); !errors.Is(statErr, os.ErrNotExist) {
				if statErr != nil {
					fmt.Fprintf(stderr, "gc stop: verifying absent city after supervisor cleanup: %v\n", statErr) //nolint:errcheck
				} else {
					fmt.Fprintln(stderr, "gc stop: supervisor cleanup completed without returning controller ownership") //nolint:errcheck
				}
				return 1
			}
		} else {
			if completionBudget.expired() {
				return deadlineFailure()
			}
			warnInvalidConfigAfterSuccessfulStop(cityPath, stderr)
			if completionBudget.expired() {
				return deadlineFailure()
			}
			if err := releaseStopLease(); err != nil {
				fmt.Fprintf(stderr, "gc stop: releasing controller ownership: %v\n", err) //nolint:errcheck
				return 1
			}
		}
		if completionBudget.expired() {
			return deadlineFailure()
		}
		if jsonOut {
			return writeCityStopSuccess(stdout, stderr, cityPath, force)
		}
		fmt.Fprintln(stdout, "City stopped.") //nolint:errcheck // best-effort stdout
		return 0
	case supervisorUnregisterNotRegistered:
		// No managed owner completed the stop. Continue to the characterized
		// direct fallback; the next P1.0D slice adds continuous lock ownership.
	case supervisorUnregisterDirectFallbackRequired:
		if !unregisterResult.hasStopResult {
			fmt.Fprintln(stderr, "gc stop: supervisor unregister omitted the required standalone stop result") //nolint:errcheck // best-effort stderr
			return 1
		}
		stopResult = unregisterResult.stopResult
		hasStopResult = true
	case supervisorUnregisterFailed, supervisorUnregisterInvalid:
		return 1
	default:
		fmt.Fprintf(stderr, "gc stop: invalid supervisor unregister result %d\n", unregisterResult.state) //nolint:errcheck // best-effort stderr
		return 1
	}
	if !hasStopResult {
		if completionBudget.expired() {
			return deadlineFailure()
		}
		stopResult = controllerStopRequestUntilForCommand(cityPath, force, completionBudget.deadline)
		if completionBudget.expired() {
			return deadlineFailure()
		}
	}
	switch stopResult.outcome {
	case controllerStopAcknowledged, controllerStopDefinitePreEntryUnavailable:
		// The transport is authoritative enough to continue. The controller or
		// direct-owner path below consumes this exact result without reissuing it.
	case controllerStopMayHaveEntered, controllerStopOutcomeInvalid:
		fmt.Fprintf(stderr, "gc stop: %v\n", stopResult.failClosedError()) //nolint:errcheck // best-effort stderr
		return 1
	default:
		fmt.Fprintf(stderr, "gc stop: %v\n", stopResult.failClosedError()) //nolint:errcheck // best-effort stderr
		return 1
	}
	if stopResult.outcome == controllerStopAcknowledged {
		fmt.Fprintln(stopStdout, "Controller stopping...") //nolint:errcheck // best-effort stdout
	}
	if completionBudget.expired() {
		return deadlineFailure()
	}
	stopLease, err = acquireStopControllerUntilForCommand(
		cityPath,
		stopResult,
		completionBudget.deadline,
		completionTimeout,
	)
	if err != nil {
		fmt.Fprintf(stderr, "gc stop: acquiring controller ownership: %v\n", err) //nolint:errcheck
		return 1
	}
	if stopLease == nil {
		fmt.Fprintln(stderr, "gc stop: acquiring controller ownership: no lease returned") //nolint:errcheck
		return 1
	}
	if completionBudget.expired() {
		return deadlineFailure()
	}

	cfg, err := loadCityConfig(cityPath, stderr)
	if err != nil {
		if completionBudget.expired() {
			return deadlineFailure()
		}
		if handled, code := stopManagedRuntimeWithoutConfigWithHeldOwnership(cityPath, err, stderr, stopResult); handled {
			if code != 0 {
				return code
			}
			if completionBudget.expired() {
				return deadlineFailure()
			}
			if err := releaseStopLease(); err != nil {
				fmt.Fprintf(stderr, "gc stop: releasing controller ownership: %v\n", err) //nolint:errcheck
				return 1
			}
			if jsonOut {
				return writeCityStopSuccess(stdout, stderr, cityPath, force)
			}
			fmt.Fprintln(stdout, "City stopped.") //nolint:errcheck
			return 0
		}
		fmt.Fprintf(stderr, "gc stop: %v\n", err) //nolint:errcheck // best-effort stderr
		return 1
	}

	if completionBudget.expired() {
		return deadlineFailure()
	}
	code := cmdStopBodyWithHeldOwnership(cityPath, cfg, force, stopResult, stopStdout, stderr, &completionBudget)
	if completionBudget.expired() {
		return deadlineFailure()
	}
	if code != 0 {
		return code
	}
	if err := releaseStopLease(); err != nil {
		fmt.Fprintf(stderr, "gc stop: releasing controller ownership: %v\n", err) //nolint:errcheck
		return 1
	}
	if jsonOut {
		return writeCityStopSuccess(stdout, stderr, cityPath, force)
	}
	fmt.Fprintln(stdout, "City stopped.") //nolint:errcheck
	return 0
}

func writeCityStopSuccess(stdout, stderr io.Writer, cityPath string, force bool) int {
	return writeLifecycleActionJSONOrExit(stdout, stderr, "gc stop", lifecycleActionJSON{
		Command:  "stop",
		Action:   "stop",
		Message:  "City stopped.",
		CityPath: cityPath,
		Force:    lifecycleBoolPtr(force),
	})
}

func resolveStopCityPath(args []string) (string, error) {
	if len(args) == 0 {
		return resolveStopCityWithoutArg()
	}
	// A name-shaped positional may be a registered city name or a local rig
	// directory; route it through the shared name resolver so a slashless rig
	// dir still resolves to its owning city without reopening the bare-name
	// walk-up footgun. Path-shaped args keep the exact stop path resolver.
	if classifyCityRef(args[0]) == cityRefName {
		ctx, err := resolveCityNameContextWithRigResolver(args[0], func(name string) (resolvedContext, error) {
			cp, perr := stopCityPathFromArg(name)
			return resolvedContext{CityPath: cp}, perr
		}, resolveRigPathToContextReadOnly)
		if err != nil {
			return "", err
		}
		return ctx.CityPath, nil
	}
	return stopCityPathFromArg(args[0])
}

func resolveStopCityWithoutArg() (string, error) {
	if cityFlag != "" {
		return resolveCityFlagValue(cityFlag)
	}
	if rigFlag != "" {
		ctx, err := resolveRigToContextReadOnly(rigFlag)
		return ctx.CityPath, err
	}
	if cityPath, ok := resolveExplicitCityPathEnv(); ok {
		return cityPath, nil
	}
	if rigName := strings.TrimSpace(os.Getenv("GC_RIG")); rigName != "" {
		ctx, err := resolveRigToContextReadOnly(rigName)
		return ctx.CityPath, err
	}
	if gcDir := strings.TrimSpace(os.Getenv("GC_DIR")); gcDir != "" {
		if citylayout.HasRuntimeRoot(gcDir) && !citylayout.HasCityConfig(gcDir) {
			if ctx, ok := lookupRigFromCwdWithConfigLoader(gcDir, loadCityConfigWithoutBuiltinPackRefresh); ok {
				return ctx.CityPath, nil
			}
		}
		if cityPath, ok := resolveCityPathFromGCDir(); ok {
			return cityPath, nil
		}
	}
	cwd, err := os.Getwd()
	if err != nil {
		return "", err
	}
	if ctx, ok := lookupRigFromCwdWithConfigLoader(cwd, loadCityConfigWithoutBuiltinPackRefresh); ok {
		return ctx.CityPath, nil
	}
	return findCity(cwd)
}

// stopCityPathFromArg resolves a path-shaped stop argument (or a local city) to
// a city path, trying an exact city path, then a rig path, then an upward city
// walk — the original path-only behavior, now invoked as resolveCityRef's path
// resolver.
func stopCityPathFromArg(ref string) (string, error) {
	abs, err := filepath.Abs(ref)
	if err != nil {
		return "", err
	}
	if cityPath, err := validateCityPath(abs); err == nil {
		return cityPath, nil
	}
	ctx, ok, rigErr := resolveRigPathToContextReadOnly(abs)
	if rigErr == nil && ok {
		return ctx.CityPath, nil
	}
	cityPath, findErr := findCity(abs)
	if findErr == nil {
		return cityPath, nil
	}
	if rigErr != nil {
		return "", rigErr
	}
	return "", findErr
}

func stopCompletionTimeoutAtEntry(cityPath string, requested time.Duration) time.Duration {
	if requested > 0 {
		return requested
	}
	cfg, err := loadSupervisorIntentConfig(cityPath)
	if err != nil {
		return defaultStopWallClockTimeout(nil)
	}
	return defaultStopWallClockTimeout(cfg)
}

// defaultStopWallClockTimeout returns the completion deadline used by cmdStop
// when --timeout is not set. Each pass budgets three sequential phases:
// interrupt provider dispatch, the configured post-interrupt grace wait, and
// bounded force-stop waves. A second pass covers orphan cleanup. Unknown extra
// live pool sessions or orphans can still require an explicit --timeout from
// the operator.
func defaultStopWallClockTimeout(cfg *config.City) time.Duration {
	base := 5 * time.Second
	if cfg != nil {
		if d := cfg.Daemon.ShutdownTimeoutDuration(); d > 0 {
			base = d
		}
	}
	targets := estimatedConfiguredStopTargets(cfg)
	interruptWaves := ceilDiv(targets, defaultMaxParallelInterrupts)
	stopWaves := ceilDiv(targets, defaultMaxParallelStopsPerWave)
	onePass := time.Duration(interruptWaves)*interruptPerTargetTimeout(cfg) +
		base +
		time.Duration(stopWaves)*stopPerTargetTimeoutDefault
	return 2*onePass + stopPerTargetTimeoutDefault
}

func estimatedConfiguredStopTargets(cfg *config.City) int {
	if cfg == nil || len(cfg.Agents) == 0 {
		return 1
	}
	total := 0
	for i := range cfg.Agents {
		agent := &cfg.Agents[i]
		if len(agent.NamepoolNames) > 0 {
			total += len(agent.NamepoolNames)
			continue
		}
		if maxSessions := agent.EffectiveMaxActiveSessions(); maxSessions != nil {
			switch {
			case *maxSessions == 0:
				continue
			case *maxSessions > 0:
				total += *maxSessions
				continue
			}
		}
		if minSessions := agent.EffectiveMinActiveSessions(); minSessions > 1 {
			total += minSessions
			continue
		}
		total++
	}
	if total < 1 {
		return 1
	}
	return total
}

func ceilDiv(n, d int) int {
	if n <= 0 {
		return 0
	}
	if d <= 0 {
		return n
	}
	return (n + d - 1) / d
}

// cmdStopBodyWithHeldOwnership runs synchronously while its caller retains the
// exact same-path controller lease. Each native call that entered before the
// deadline is joined through return; no later effect begins once the original
// command deadline is exhausted. The caller releases ownership and reports
// terminal success only after this function returns successfully.
func cmdStopBodyWithHeldOwnership(cityPath string, cfg *config.City, force bool, stopResult controllerStopResult, stdout, stderr io.Writer, budget *stopCompletionBudget) (exitCode int) {
	cityName := loadedCityName(cfg, cityPath)
	deadlineExpired := func() bool { return !stopEffectAdmitted(budget) }

	switch stopResult.outcome {
	case controllerStopAcknowledged:
		// The controller handled runtime shutdown before returning ownership;
		// the retained caller still owns the required bead-provider teardown.
		if err := shutdownBeadsProviderForStop(cityPath); err != nil {
			fmt.Fprintf(stderr, "gc stop: bead store: %v\n", err) //nolint:errcheck // best-effort stderr
			return 1
		}
		if deadlineExpired() {
			return 1
		}
		return 0
	case controllerStopDefinitePreEntryUnavailable:
		// The request provably did not enter a controller. Continue to the
		// characterized direct fallback; the next slice adds lock ownership.
	case controllerStopMayHaveEntered, controllerStopOutcomeInvalid:
		fmt.Fprintf(stderr, "gc stop: %v\n", stopResult.failClosedError()) //nolint:errcheck // best-effort stderr
		return 1
	default:
		fmt.Fprintf(stderr, "gc stop: %v\n", stopResult.failClosedError()) //nolint:errcheck // best-effort stderr
		return 1
	}

	if deadlineExpired() {
		return 1
	}
	store, err := openCityStoreForStop(cityPath)
	if err != nil {
		fmt.Fprintf(stderr, "gc stop: opening bead store: %v\n", err) //nolint:errcheck
		return 1
	}
	if store == nil {
		fmt.Fprintln(stderr, "gc stop: opening bead store: provider returned no store") //nolint:errcheck
		return 1
	}
	storeClosed := false
	closeStore := func() error {
		if storeClosed {
			return nil
		}
		storeClosed = true
		closer, ok := store.(interface{ CloseStore() error })
		if !ok {
			return nil
		}
		return closer.CloseStore()
	}
	defer func() { _ = closeStore() }()
	if deadlineExpired() {
		return 1
	}
	// Every store consumer in this stop flow is session-class (sleep-reason marks,
	// session-name lookups, session-runtime stop, orphan cleanup), so route the
	// whole flow through the session coordination-class store for relocation-safety.
	sessStore := cliSessionStore(store, cfg, cityPath)
	if err := markCityStopSessionSleepReasonWithBudget(sessionFrontDoor(sessStore), stderr, budget); err != nil {
		return 1
	}

	if deadlineExpired() {
		return 1
	}
	sp, err := sessionProviderForStopCity(cfg, cityPath)
	if err != nil {
		fmt.Fprintf(stderr, "gc stop: %v\n", err) //nolint:errcheck // best-effort stderr
		return 1
	}
	if deadlineExpired() {
		return 1
	}
	st := cfg.Workspace.SessionTemplate
	var sessionNames []string
	desired := make(map[string]bool, len(cfg.Agents))
	for _, a := range cfg.Agents {
		if deadlineExpired() {
			return 1
		}
		sp0 := scaleParamsFor(&a)
		qn := a.QualifiedName()
		if !a.SupportsInstanceExpansion() {
			// Non-expanding template.
			sn := lookupSessionNameOrLegacy(sessStore, cityName, qn, st)
			sessionNames = append(sessionNames, sn)
			desired[sn] = true
		} else {
			// Pool agent: resolve runtime session names from beads first, then legacy discovery.
			for _, ref := range resolvePoolSessionRefs(sessStore, cfg, a.Name, a.Dir, sp0, &a, cityName, st, sp, stderr) {
				sessionNames = append(sessionNames, ref.sessionName)
				desired[ref.sessionName] = true
			}
		}
	}
	if deadlineExpired() {
		return 1
	}
	recorder := events.Discard
	recorderOpened := false
	if fr, err := newFileEventsRecorder(
		filepath.Join(cityPath, ".gc", "events.jsonl"), cfg.Events, stderr); err == nil {
		recorder = fr
		recorderOpened = true
	}
	recorderClosed := false
	closeRecorder := func() error {
		if !recorderOpened || recorderClosed {
			return nil
		}
		recorderClosed = true
		return closeEventRecorderForStop(recorder)
	}
	defer func() {
		if err := closeRecorder(); err != nil {
			fmt.Fprintf(stderr, "gc stop: closing event recorder: %v\n", err) //nolint:errcheck
			exitCode = 1
		}
	}()
	if deadlineExpired() {
		return 1
	}

	graceTimeout := cfg.Daemon.ShutdownTimeoutDuration()
	if force {
		// gracefulStopAll treats timeout=0 as "skip interrupt pass, kill immediately".
		graceTimeout = 0
	}

	code := doStopWithoutSuccessMessageWithBudget(sessionNames, sp, cfg, sessStore, graceTimeout, recorder, stdout, stderr, budget)

	// Clean up orphan sessions (sessions with the city prefix that are
	// not in the current config).
	if deadlineExpired() {
		return 1
	}
	if err := stopOrphansWithBudget(sp, desired, cfg, sessionFrontDoor(sessStore), graceTimeout, recorder, stdout, stderr, budget); err != nil {
		code = 1
	}

	if deadlineExpired() {
		return 1
	}
	if err := teardownServerForStop(sp); err != nil {
		fmt.Fprintf(stderr, "gc stop: teardown server: %v\n", err) //nolint:errcheck
		code = 1
	}
	if err := closeRecorder(); err != nil {
		fmt.Fprintf(stderr, "gc stop: closing event recorder: %v\n", err) //nolint:errcheck
		code = 1
	}

	// Stop bead store's backing service after agents.
	if deadlineExpired() {
		return 1
	}
	if err := shutdownBeadsProviderForStop(cityPath); err != nil {
		fmt.Fprintf(stderr, "gc stop: bead store: %v\n", err) //nolint:errcheck // best-effort stderr
		code = 1
	}
	if deadlineExpired() {
		return 1
	}
	if err := closeStore(); err != nil {
		fmt.Fprintf(stderr, "gc stop: closing bead store: %v\n", err) //nolint:errcheck
		code = 1
	}
	return code
}

func teardownServerForStop(sp runtime.Provider) error {
	lifecycle, ok := sp.(runtime.ServerLifecycleProvider)
	if !ok {
		return nil
	}
	return lifecycle.TeardownServer()
}

func markCityStopSessionSleepReason(sessFront *session.Store, stderr io.Writer) {
	_ = markCityStopSessionSleepReasonWithBudget(sessFront, stderr, nil)
}

func markCityStopSessionSleepReasonWithBudget(sessFront *session.Store, stderr io.Writer, budget *stopCompletionBudget) error {
	if !sessFront.Backed() {
		return nil
	}
	if !stopEffectAdmitted(budget) {
		return errStopCompletionDeadline
	}
	// The label-only, closed-excluded, IsSessionBeadOrRepairable-UNfiltered Info
	// lister is byte-identical to the former ListByLabel("gc:session") + closed-skip
	// sweep: it keeps damaged gc:session-labeled beads with a non-"session" type (which
	// the narrowing Store.List would drop) and reads each row's classifier through the
	// typed twin (sessionMetadataStateInfo) + the Info.SleepReason mirror.
	sessions, err := sessFront.ListLabeledSessionInfosUnfiltered()
	if err != nil {
		fmt.Fprintf(stderr, "gc stop: marking sessions: %v\n", err) //nolint:errcheck // best-effort warning
		return err
	}
	if !stopEffectAdmitted(budget) {
		return errStopCompletionDeadline
	}
	for _, info := range sessions {
		if sessionMetadataStateInfo(info) != "active" {
			continue
		}
		if strings.TrimSpace(info.SleepReason) != "" {
			continue
		}
		if !stopEffectAdmitted(budget) {
			return errStopCompletionDeadline
		}
		if err := sessFront.SetMarker(info.ID, "sleep_reason", string(session.SleepReasonCityStop)); err != nil {
			fmt.Fprintf(stderr, "gc stop: marking session %s: %v\n", info.ID, err) //nolint:errcheck // best-effort warning
		}
		if !stopEffectAdmitted(budget) {
			return errStopCompletionDeadline
		}
	}
	return nil
}

func stopCityManagedBeadsProviderWithHeldOwnership(cityPath string) (bool, error) {
	if rawBeadsProvider(cityPath) != "bd" {
		return false, nil
	}
	if currentResolvableManagedDoltPort(cityPath) == "" {
		return false, nil
	}
	return true, shutdownBeadsProviderForStop(cityPath)
}

var shutdownBeadsProviderForStop = shutdownBeadsProvider

func stopManagedRuntimeWithoutConfigWithHeldOwnership(cityPath string, cfgErr error, stderr io.Writer, stopResult controllerStopResult) (bool, int) {
	controllerStopped := stopResult.outcome == controllerStopAcknowledged
	stopped, stopErr := stopCityManagedBeadsProviderWithHeldOwnership(cityPath)
	if stopErr != nil {
		fmt.Fprintf(stderr, "gc stop: bead store: %v\n", stopErr) //nolint:errcheck // best-effort stderr
		return true, 1
	}
	if !controllerStopped && !stopped {
		return false, 0
	}
	warnInvalidConfigStopSuccess(cfgErr, stderr)
	return true, 0
}

func warnInvalidConfigAfterSuccessfulStop(cityPath string, stderr io.Writer) {
	if _, err := loadSupervisorIntentConfig(cityPath); err != nil {
		warnInvalidConfigStopSuccess(err, stderr)
	}
}

func warnInvalidConfigStopSuccess(err error, stderr io.Writer) {
	if err == nil {
		return
	}
	fmt.Fprintf(stderr, "gc stop: stopped city despite invalid config: %v\n", err) //nolint:errcheck // best-effort stderr
}

func stopOrphansWithBudget(sp runtime.Provider, desired map[string]bool, cfg *config.City, sessFront *session.Store,
	timeout time.Duration, rec events.Recorder, stdout, stderr io.Writer, budget *stopCompletionBudget,
) error {
	if !stopEffectAdmitted(budget) {
		return errStopCompletionDeadline
	}
	running, err := sp.ListRunning("")
	partialList := runtime.IsPartialListError(err)
	if err != nil && !partialList {
		fmt.Fprintf(stderr, "gc stop: listing sessions: %v\n", err) //nolint:errcheck // best-effort stderr
		return err
	}
	if partialList {
		fmt.Fprintf(stderr, "gc stop: listing sessions partially failed: %v\n", err) //nolint:errcheck // best-effort stderr
	}
	var orphans []string
	for _, name := range running {
		if desired[name] {
			continue
		}
		orphans = append(orphans, name)
	}
	if !stopEffectAdmitted(budget) {
		return errStopCompletionDeadline
	}
	return gracefulStopAllWithOwnership(orphans, sp, timeout, rec, cfg, sessFront.Store(), stdout, stderr, nil, true, budget)
}

func waitForStandaloneControllerStop(cityPath string, timeout time.Duration) error {
	if timeout <= 0 {
		timeout = 5 * time.Second
	}
	deadline := time.Now().Add(timeout)
	for {
		pid := controllerAlive(cityPath)
		lock, err := acquireControllerLock(cityPath)
		switch {
		case err == nil && pid == 0:
			lock.Close() //nolint:errcheck // best-effort probe cleanup
			return nil
		case err == nil:
			lock.Close() //nolint:errcheck // best-effort probe cleanup
		case !errors.Is(err, errControllerAlreadyRunning):
			return fmt.Errorf("probing standalone controller: %w", err)
		}
		if time.Now().After(deadline) {
			if pid != 0 {
				return fmt.Errorf("timed out waiting for standalone controller (PID %d) to stop", pid)
			}
			return fmt.Errorf("timed out waiting for standalone controller to release its lock")
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// doStop is the pure logic for "gc stop". Filters to running sessions and
// performs graceful shutdown (interrupt → wait → kill). Accepts session names,
// provider, timeout, and recorder for testability.
//
//nolint:unparam // store is an intentional lifecycle-ledger seam; legacy callers currently exercise it with nil.
func doStop(sessionNames []string, sp runtime.Provider, cfg *config.City, store beads.Store, timeout time.Duration,
	rec events.Recorder, stdout, stderr io.Writer,
) int {
	code := doStopWithoutSuccessMessageWithBudget(sessionNames, sp, cfg, store, timeout, rec, stdout, stderr, nil)
	if code == 0 {
		fmt.Fprintln(stdout, "City stopped.") //nolint:errcheck
	}
	return code
}

func doStopWithoutSuccessMessageWithBudget(sessionNames []string, sp runtime.Provider, cfg *config.City, store beads.Store, timeout time.Duration,
	rec events.Recorder, stdout, stderr io.Writer, budget *stopCompletionBudget,
) int {
	visible := map[string]bool{}
	var observationErr error
	if sp != nil {
		if !stopEffectAdmitted(budget) {
			return 1
		}
		names, err := sp.ListRunning("")
		partialList := runtime.IsPartialListError(err)
		if err != nil && !partialList {
			fmt.Fprintf(stderr, "gc stop: listing sessions: %v\n", err) //nolint:errcheck // best-effort stderr
			observationErr = err
			names = nil
		}
		if partialList {
			fmt.Fprintf(stderr, "gc stop: listing sessions partially failed: %v\n", err) //nolint:errcheck // best-effort stderr
		}
		for _, name := range names {
			if name = strings.TrimSpace(name); name != "" {
				visible[name] = true
			}
		}
	}
	var running []string
	for _, sn := range sessionNames {
		sn = strings.TrimSpace(sn)
		if sn == "" {
			continue
		}
		if !stopEffectAdmitted(budget) {
			return 1
		}
		if alive, err := workerSessionTargetRunningWithConfig("", store, sp, cfg, sn); err == nil && alive {
			running = append(running, sn)
			continue
		}
		if visible[sn] {
			running = append(running, sn)
		}
	}
	if !stopEffectAdmitted(budget) {
		return 1
	}
	stopErr := gracefulStopAllWithOwnership(running, sp, timeout, rec, cfg, beads.SessionStore{Store: store}, stdout, stderr, nil, true, budget)
	if observationErr != nil || stopErr != nil {
		return 1
	}
	return 0
}
