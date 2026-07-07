package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
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
// --db) graphstore a `gc run` invocation opens. It has no relationship to any
// registered city — `gc run` is deliberately standalone and never resolves one.
const runDemoCityID = "gc-run-demo"

// runAgentOptions carries the agent-`do` bridge flags for a run. When Command
// is empty, no agent host is built and a do node is refused (today's behavior).
type runAgentOptions struct {
	Command    string        // --agent-cmd, the agent CLI (e.g. "claude")
	PromptFlag string        // --agent-prompt-flag, the prompt flag (e.g. "-p")
	Provider   string        // --session-provider override; else GC_SESSION; else subprocess
	Timeout    time.Duration // --agent-timeout per-do-step bound
}

// newRunCmd builds the standalone `gc run <lumen-file>` command: the
// proof-of-concept that executes a compiled Lumen formula on the native
// graphstore journal substrate. It resolves and opens its own store and never
// requires (or discovers) a city.
func newRunCmd(stdout, stderr io.Writer) *cobra.Command {
	var dbPath string
	var keep bool
	var inputJSON string
	var agent runAgentOptions
	cmd := &cobra.Command{
		Use:   "run <lumen-file>",
		Short: "Run a compiled Lumen formula on the graph substrate",
		Long: `Run a compiled Lumen formula (lumen.ir) directly on the native
graphstore journal substrate.

The argument is a Lumen source file (e.g. hello.lumen) or a compiled IR
document (hello.lumen.json). For a source file, gc looks for a sibling compiled
IR next to it. Compile a .lumen source to IR before running it.

By default the run writes to a throwaway SQLite store in a temp directory that
is deleted afterward, so repeated runs of the same formula do not collide on
the deterministic stream id. Use --db to run against a persistent store and
--keep to retain the temp store for inspection.

A formula with an agent 'do' step needs an agent command to run it: pass
--agent-cmd (e.g. --agent-cmd claude --agent-prompt-flag -p). Without it, a do
step is refused. GC_SESSION=fake selects the fake session provider for tests.`,
		Args:          cobra.ExactArgs(1),
		SilenceErrors: true,
		SilenceUsage:  true,
		RunE: func(cmd *cobra.Command, args []string) error {
			if doRun(cmd, args[0], dbPath, keep, inputJSON, agent, stdout, stderr) != 0 {
				return errExit
			}
			return nil
		},
	}
	cmd.Flags().StringVar(&dbPath, "db", "", "path to a persistent graphstore db (default: throwaway temp store)")
	cmd.Flags().BoolVar(&keep, "keep", false, "keep the throwaway temp store instead of deleting it")
	cmd.Flags().StringVar(&inputJSON, "input", "", "run input as a JSON object (default: empty)")
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

func doRun(cmd *cobra.Command, arg, dbPath string, keep bool, inputJSON string, agentOpts runAgentOptions, stdout, stderr io.Writer) int {
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

	storePath, cleanup, err := resolveRunStorePath(dbPath, keep, stderr)
	if err != nil {
		fmt.Fprintf(stderr, "gc run: %v\n", err) //nolint:errcheck // best-effort stderr
		return 1
	}
	defer cleanup()

	ctx := cmd.Context()
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
	fmt.Fprintf(stdout, "lumen run: %s  (stream %s)\n", doc.Name, result.StreamID) //nolint:errcheck // best-effort stdout

	kinds := nodeKindsByID(doc)
	for _, ev := range result.Events {
		if ev.Type != engine.EventNodeSettled {
			continue
		}
		var p struct {
			ID      string `json:"id"`
			Outcome string `json:"outcome"`
			Output  string `json:"output"`
		}
		if err := json.Unmarshal(ev.Payload, &p); err != nil {
			continue
		}
		kind := kinds[p.ID]
		if kind == "" {
			kind = "?"
		}
		fmt.Fprintf(stdout, "  %s  [%s]  %s\n", p.ID, kind, p.Outcome) //nolint:errcheck // best-effort stdout
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
