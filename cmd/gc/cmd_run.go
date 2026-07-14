package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/graphstore"
	"github.com/gastownhall/gascity/internal/lumen/engine"
	"github.com/gastownhall/gascity/internal/lumen/enginehost"
	"github.com/gastownhall/gascity/internal/lumen/ir"
	"github.com/spf13/cobra"
)

// runDemoCityID is the chain-genesis city id stamped on the throwaway (or
// --db) graphstore a standalone `gc run` invocation opens. It has no
// relationship to any registered city.
const runDemoCityID = "gc-run-demo"

const runCityPollInterval = 250 * time.Millisecond

// runAgentOptions carries the agent-`do` bridge flags for a run. When Command
// is empty, no agent host is built and a do node is refused (today's behavior).
type runAgentOptions struct {
	Command    string        // --agent-cmd, the agent CLI (e.g. "claude")
	PromptFlag string        // --agent-prompt-flag, the prompt flag (e.g. "-p")
	Provider   string        // --session-provider override; else GC_SESSION; else subprocess
	Timeout    time.Duration // --agent-timeout per-do-step bound
}

// newRunCmd builds `gc run <lumen-file>`. In a resolved City it enqueues the
// compiled formula on the City's controller-driven graph path and waits for the
// durable run to seal. Outside a City it preserves the standalone graphstore
// runner.
func newRunCmd(stdout, stderr io.Writer) *cobra.Command {
	var dbPath string
	var keep bool
	var inputJSON string
	var route string
	var agent runAgentOptions
	cmd := &cobra.Command{
		Use:   "run <lumen-file>",
		Short: "Run a compiled Lumen formula",
		Long: `Run a compiled Lumen formula (lumen.ir).

The argument is a Lumen source file (e.g. hello.lumen) or a compiled IR
document (hello.lumen.json). For a source file, gc looks for a sibling compiled
IR next to it. Compile a .lumen source to IR before running it.

When the current directory, --city, or normal City selectors resolve a City,
the run uses that City's controller, durable Beads, and configured Agent pools.
Pass --route for do steps without an explicit Agent binding. The command prints
the run stream immediately, then waits for the terminal formula outcome. Other
terminals can observe the same Agents with gc session list.

Outside a City, the existing standalone runner writes to a throwaway SQLite
store that is deleted afterward. Use --db for a persistent standalone store and
--keep to retain the temporary store.

A standalone formula with an agent 'do' step needs an agent command: pass
--agent-cmd (e.g. --agent-cmd claude --agent-prompt-flag -p). Without it, a do
step is refused. GC_SESSION=fake selects the fake session provider for tests.`,
		Args:          cobra.ExactArgs(1),
		SilenceErrors: true,
		SilenceUsage:  true,
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx, stopSignals := signal.NotifyContext(cmd.Context(), os.Interrupt, syscall.SIGTERM)
			defer stopSignals()
			return exitForCode(doRun(ctx, args[0], dbPath, keep, route, inputJSON, agent, stdout, stderr))
		},
	}
	cmd.Flags().StringVar(&dbPath, "db", "", "path to a persistent graphstore db (default: throwaway temp store)")
	cmd.Flags().BoolVar(&keep, "keep", false, "keep the throwaway temp store instead of deleting it")
	cmd.Flags().StringVar(&inputJSON, "input", "", "run input as a JSON object (default: empty)")
	cmd.Flags().StringVar(&route, "route", "", "default City Agent route for unbound 'do' steps")
	cmd.Flags().StringVar(&agent.Command, "agent-cmd", "", "agent CLI to run 'do' steps (e.g. claude); enables agent steps")
	cmd.Flags().StringVar(&agent.PromptFlag, "agent-prompt-flag", "", "CLI flag the rendered prompt rides (e.g. -p)")
	cmd.Flags().StringVar(&agent.Provider, "session-provider", "", "session runtime provider for agent steps (default: GC_SESSION or subprocess)")
	cmd.Flags().DurationVar(&agent.Timeout, "agent-timeout", 0, "per 'do' step timeout (default: host default)")
	return cmd
}

// buildRunAgentHost constructs the agent host for a do-capable run. It is a
// package var so tests can inject a deterministic host (e.g. a StubHost) without
// spawning real sessions. The returned cleanup is always safe to call.
var buildRunAgentHost = defaultRunAgentHost

// resolveRunCity is the normal City resolver behind a package var so the
// standalone and City-backed command paths can be selected deterministically in
// unit tests.
var resolveRunCity = resolveCity

// runWaitForLumenRun is the read-only terminal observer behind a package var so
// command tests can prove enqueue/wait wiring without running a controller.
var runWaitForLumenRun = waitForLumenRun

// runAgentProviderName resolves the session provider a run will use: the
// --session-provider flag, else GC_SESSION, else the subprocess default.
func runAgentProviderName(opts runAgentOptions) string {
	name := strings.TrimSpace(opts.Provider)
	if name == "" {
		name = strings.TrimSpace(os.Getenv("GC_SESSION"))
	}
	if name == "" {
		name = "subprocess"
	}
	return name
}

func defaultRunAgentHost(_ context.Context, opts runAgentOptions) (enginehost.AgentHost, func(), error) {
	providerName := runAgentProviderName(opts)
	provider, err := runtimeRegistry.New(providerName, config.SessionConfig{}, "", "")
	if err != nil {
		return nil, func() {}, fmt.Errorf("resolving session provider %q: %w", providerName, err)
	}
	host, err := enginehost.NewWorkerHost(enginehost.WorkerHostConfig{
		Store:        beads.NewMemStore(),
		Provider:     provider,
		ProviderName: providerName,
		Command:      opts.Command,
		PromptFlag:   opts.PromptFlag,
		MaxWait:      opts.Timeout,
	})
	if err != nil {
		return nil, func() {}, err
	}
	return host, func() {}, nil
}

func doRun(ctx context.Context, arg, dbPath string, keep bool, route, inputJSON string, agentOpts runAgentOptions, stdout, stderr io.Writer) int {
	irPath, err := resolveLumenIRPath(arg)
	if err != nil {
		fmt.Fprintf(stderr, "gc run: %v\n", err) //nolint:errcheck // best-effort stderr
		return 1
	}

	data, err := os.ReadFile(irPath)
	if err != nil {
		fmt.Fprintf(stderr, "gc run: reading %s: %v\n", irPath, err) //nolint:errcheck // best-effort stderr
		return 1
	}
	doc, err := ir.Decode(data)
	if err != nil {
		fmt.Fprintf(stderr, "gc run: %v\n", err) //nolint:errcheck // best-effort stderr
		return 1
	}

	input, err := parseRunInput(inputJSON)
	if err != nil {
		fmt.Fprintf(stderr, "gc run: %v\n", err) //nolint:errcheck // best-effort stderr
		return 1
	}

	cityPath, cityErr := resolveRunCity()
	if cityErr == nil {
		if strings.TrimSpace(dbPath) != "" || keep || agentOpts != (runAgentOptions{}) {
			fmt.Fprintln(stderr, "gc run: --db, --keep, and --agent-* flags are standalone-only; omit them to run through the current City") //nolint:errcheck // best-effort stderr
			return 1
		}
		return doCityRun(ctx, cityPath, irPath, route, inputJSON, doc, stdout, stderr)
	}
	if !isImplicitRunCityMiss(cityErr) {
		fmt.Fprintf(stderr, "gc run: resolving current City: %v\n", cityErr) //nolint:errcheck // best-effort stderr
		return 1
	}

	storePath, cleanup, err := resolveRunStorePath(dbPath, keep, stderr)
	if err != nil {
		fmt.Fprintf(stderr, "gc run: %v\n", err) //nolint:errcheck // best-effort stderr
		return 1
	}
	defer cleanup()

	store, err := graphstore.Open(ctx, storePath, graphstore.Options{CityID: runDemoCityID})
	if err != nil {
		fmt.Fprintf(stderr, "gc run: opening graphstore: %v\n", err) //nolint:errcheck // best-effort stderr
		return 1
	}
	defer store.Close() //nolint:errcheck // best-effort close of throwaway store

	var opts engine.Options
	if strings.TrimSpace(agentOpts.Command) != "" {
		host, cleanup, hostErr := buildRunAgentHost(ctx, agentOpts)
		if hostErr != nil {
			fmt.Fprintf(stderr, "gc run: %v\n", hostErr) //nolint:errcheck // best-effort stderr
			return 1
		}
		defer cleanup()
		opts.Host = host
	}

	result, err := engine.RunWithOptions(ctx, store, doc, input, opts)
	if err != nil {
		if opts.Host == nil && errors.Is(err, engine.ErrUnsupportedNode) {
			fmt.Fprintf(stderr, "gc run: %v\n", err)                                                                                                     //nolint:errcheck // best-effort stderr
			fmt.Fprintf(stderr, "gc run: this formula has an agent step; pass --agent-cmd to run it (e.g. --agent-cmd claude --agent-prompt-flag -p)\n") //nolint:errcheck // best-effort stderr
			return 1
		}
		fmt.Fprintf(stderr, "gc run: %v\n", err) //nolint:errcheck // best-effort stderr
		return 1
	}

	printRunResult(stdout, doc, result)
	if opts.Host != nil {
		printAgentOutcomeCaveat(stderr, agentOpts, result)
	}
	if result.Outcome == engine.OutcomeFailed {
		return 1
	}
	return 0
}

// isImplicitRunCityMiss distinguishes the ordinary "run from outside a City"
// case from explicit selector, registry, filesystem, and resolver failures.
// Only the former selects the backward-compatible standalone runner.
func isImplicitRunCityMiss(err error) bool {
	return errors.Is(err, errImplicitCityNotFound)
}

// doCityRun enqueues a pool/controller-driven run into the resolved City,
// exposes its durable stream id before blocking, and observes the journal until
// the run seals. Stopping the local waiter never mutates or cancels the run.
func doCityRun(ctx context.Context, cityPath, irPath, route, inputJSON string, doc *ir.IR, stdout, stderr io.Writer) int {
	queued, err := lumenEnqueue(ctx, cityPath, lumenEnqueueRequest{
		IRPath:    irPath,
		Route:     route,
		InputJSON: inputJSON,
	}, stderr)
	if err != nil {
		fmt.Fprintf(stderr, "gc run: %v\n", err) //nolint:errcheck // best-effort stderr
		return 1
	}

	printRunHeader(stdout, doc, queued.StreamID)

	backend, err := loadGraphJournalBackendConfig(cityPath)
	if err != nil {
		fmt.Fprintf(stderr, "gc run: resolving graph journal backend after enqueueing %s: %v\n", queued.StreamID, err) //nolint:errcheck // best-effort stderr
		return 1
	}
	store, err := backend.openGraphStore(ctx, cityPath)
	if err != nil {
		fmt.Fprintf(stderr, "gc run: opening graph store after enqueueing %s: %v\n", queued.StreamID, err) //nolint:errcheck // best-effort stderr
		return 1
	}
	defer func() { _ = store.Close() }()

	result, err := runWaitForLumenRun(ctx, store, queued.StreamID, runCityPollInterval)
	if err != nil {
		if errors.Is(err, context.Canceled) {
			fmt.Fprintf(stderr, "gc run: detached from %s; run continues in city %s\n", queued.StreamID, cityPath) //nolint:errcheck // best-effort stderr
			return 130
		}
		fmt.Fprintf(stderr, "gc run: waiting for %s: %v\n", queued.StreamID, err) //nolint:errcheck // best-effort stderr
		return 1
	}

	printRunCompletion(stdout, doc, result)
	if result.Outcome != engine.OutcomePass {
		return 1
	}
	return 0
}

// waitForLumenRun polls the canonical per-run journal fold until it closes. The
// graph store has no subscription primitive; the controller poke handles prompt
// execution while this read-only loop provides a bounded, cancellable observer.
func waitForLumenRun(ctx context.Context, store *graphstore.Store, streamID string, pollInterval time.Duration) (engine.RunResult, error) {
	if pollInterval <= 0 {
		return engine.RunResult{}, fmt.Errorf("gc run: poll interval must be positive")
	}
	ticker := time.NewTicker(pollInterval)
	defer ticker.Stop()

	for {
		view, err := engine.FoldRunView(ctx, store, streamID)
		if err != nil {
			return engine.RunResult{}, err
		}
		if view.Closed {
			events, err := store.ReadStream(ctx, streamID, 1, 0)
			if err != nil {
				return engine.RunResult{}, fmt.Errorf("reading terminal journal: %w", err)
			}
			return engine.RunResult{StreamID: streamID, Outcome: view.Outcome, Events: events}, nil
		}

		select {
		case <-ctx.Done():
			return engine.RunResult{}, ctx.Err()
		case <-ticker.C:
		}
	}
}

// printAgentOutcomeCaveat warns the demo user, when a run actually executed an
// agent `do` step, that the do-step outcome came from the phase-based worker
// boundary (any process exit ⇒ pass) so the agent must self-report gc.outcome
// for a true pass/fail, and — under the default non-capturing subprocess
// provider — that no output was captured for {{ref}} chaining. It prints
// nothing for exec-only runs (no do step ran).
func printAgentOutcomeCaveat(stderr io.Writer, agentOpts runAgentOptions, result engine.RunResult) {
	ranDoStep := false
	for _, ev := range result.Events {
		if ev.Type == engine.EventEffectScheduled {
			ranDoStep = true
			break
		}
	}
	if !ranDoStep {
		return
	}
	fmt.Fprintln(stderr, "gc run: note: agent outcome is phase-based (process exit ⇒ pass); the agent must self-report gc.outcome for true pass/fail.") //nolint:errcheck // best-effort stderr
	if runAgentProviderName(agentOpts) == "subprocess" {
		fmt.Fprintln(stderr, "gc run: note: the subprocess provider captures no agent output, so {{ref}} chaining from a do step sees an empty value.") //nolint:errcheck // best-effort stderr
	}
}

// resolveLumenIRPath resolves the compiled IR document to load from a user
// argument. A .json path is used verbatim; any other path (e.g. a .lumen
// source) is resolved to a sibling compiled IR, trying <path>.json, then the
// .lumen→.lumen.json rewrite, then <basename>.lumen.json alongside it.
func resolveLumenIRPath(arg string) (string, error) {
	if strings.HasSuffix(arg, ".json") {
		if _, err := os.Stat(arg); err != nil {
			return "", fmt.Errorf("IR file %s: %w", arg, err)
		}
		return arg, nil
	}

	dir := filepath.Dir(arg)
	base := filepath.Base(arg)
	candidates := []string{
		arg + ".json",
		strings.TrimSuffix(arg, ".lumen") + ".lumen.json",
		filepath.Join(dir, strings.TrimSuffix(base, ".lumen")+".lumen.json"),
	}
	seen := make(map[string]struct{}, len(candidates))
	for _, c := range candidates {
		if _, dup := seen[c]; dup {
			continue
		}
		seen[c] = struct{}{}
		if _, err := os.Stat(c); err == nil {
			return c, nil
		}
	}
	return "", fmt.Errorf("no compiled IR found for %s (looked for %s); compile the formula first, e.g. examples/lumen/hello.lumen.json",
		arg, strings.Join(candidates, ", "))
}

// parseRunInput decodes the --input flag into the run input map. An empty flag
// yields an empty (non-nil) map.
func parseRunInput(inputJSON string) (map[string]any, error) {
	input := map[string]any{}
	if strings.TrimSpace(inputJSON) == "" {
		return input, nil
	}
	if err := json.Unmarshal([]byte(inputJSON), &input); err != nil {
		return nil, fmt.Errorf("parsing --input as a JSON object: %w", err)
	}
	return input, nil
}

// resolveRunStorePath returns the graphstore db path to open plus a cleanup
// func. With --db it uses that path and cleanup is a no-op. Otherwise it mints
// a throwaway temp dir; cleanup removes it unless keep is set (in which case it
// reports the retained path to stderr).
func resolveRunStorePath(dbPath string, keep bool, stderr io.Writer) (string, func(), error) {
	if strings.TrimSpace(dbPath) != "" {
		if err := os.MkdirAll(filepath.Dir(dbPath), 0o755); err != nil {
			return "", nil, fmt.Errorf("creating db directory: %w", err)
		}
		return dbPath, func() {}, nil
	}
	tmp, err := os.MkdirTemp("", "gc-run-*")
	if err != nil {
		return "", nil, fmt.Errorf("creating temp store: %w", err)
	}
	path := filepath.Join(tmp, "journal.db")
	cleanup := func() {
		if keep {
			fmt.Fprintf(stderr, "gc run: kept graphstore at %s\n", path) //nolint:errcheck // best-effort stderr
			return
		}
		_ = os.RemoveAll(tmp)
	}
	return path, cleanup, nil
}

// printRunResult renders a human-readable summary of a completed run: a header
// line with the formula name and stream id, one line per settled step (its id,
// node kind, and outcome) with its captured output indented beneath, and the
// aggregated run outcome.
func printRunResult(stdout io.Writer, doc *ir.IR, result engine.RunResult) {
	printRunHeader(stdout, doc, result.StreamID)
	printRunCompletion(stdout, doc, result)
}

func printRunHeader(stdout io.Writer, doc *ir.IR, streamID string) {
	fmt.Fprintf(stdout, "lumen run: %s  (stream %s)\n", doc.Name, streamID) //nolint:errcheck // best-effort stdout
}

func printRunCompletion(stdout io.Writer, doc *ir.IR, result engine.RunResult) {
	kinds := nodeKindsByID(doc)
	for _, ev := range result.Events {
		if ev.Type != engine.EventOutcomeSettled {
			continue
		}
		var p struct {
			Activation string `json:"activation"`
			Outcome    string `json:"outcome"`
			Output     string `json:"output"`
		}
		if err := json.Unmarshal(ev.Payload, &p); err != nil {
			continue
		}
		nodeID := engine.ActivationNodeID(p.Activation)
		kind := kinds[nodeID]
		if kind == "" {
			kind = "?"
		}
		fmt.Fprintf(stdout, "  %s  [%s]  %s\n", nodeID, kind, p.Outcome) //nolint:errcheck // best-effort stdout
		for _, line := range outputLines(p.Output) {
			fmt.Fprintf(stdout, "    %s\n", line) //nolint:errcheck // best-effort stdout
		}
	}

	fmt.Fprintf(stdout, "outcome: %s\n", result.Outcome) //nolint:errcheck // best-effort stdout
}

// nodeKindsByID maps every node id in the document (including nested nodes) to
// its kind, so the run summary can label each settled step.
func nodeKindsByID(doc *ir.IR) map[string]string {
	kinds := map[string]string{}
	doc.WalkNodes(func(node map[string]json.RawMessage) {
		var id, kind string
		_ = json.Unmarshal(node["id"], &id)
		_ = json.Unmarshal(node["kind"], &kind)
		if id != "" {
			kinds[id] = kind
		}
	})
	return kinds
}

// outputLines splits a captured step output into display lines, returning an
// empty slice for empty output so no blank indented line is printed.
func outputLines(output string) []string {
	if output == "" {
		return nil
	}
	return strings.Split(output, "\n")
}
