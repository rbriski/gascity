package main

import (
	"context"
	"fmt"
	"io"
	"strings"

	"github.com/gastownhall/gascity/internal/graphstore"
	"github.com/gastownhall/gascity/internal/lumen/engine"
	"github.com/gastownhall/gascity/internal/lumen/ir"
	"github.com/spf13/cobra"
)

// The v1 agent-driven stepper CLI: `gc lumen step` and `gc lumen settle`, the two thin
// verbs the driver-loop agent calls between its own turns. Each is a short-lived process
// that performs a partial journal drive over the engine's shared rebuild/fold/seal core
// (engine.Step / engine.Settle) — the agent IS the driver; `gc` is a stateless stepper.
// ZERO role names.

// lumenStepperOp is the engine entry a stepper verb runs — Step, or a Settle bound to its
// node/outcome/output flags — over the rebuilt run.
type lumenStepperOp func(ctx context.Context, gs *graphstore.Store, doc *ir.IR, streamID string, input map[string]any) (engine.StepResult, error)

// newLumenStepCmd registers `gc lumen step --run <streamID>`: rebuild the run, drive the
// ready non-do units inline, and print the next ready do (its node id + rendered prompt)
// or `done`.
func newLumenStepCmd(stdout, stderr io.Writer) *cobra.Command {
	var runID string
	var jsonOut bool
	c := &cobra.Command{
		Use:    "step",
		Short:  "Advance a v1 Lumen run one turn: print the next ready do (id + prompt) or `done`",
		Hidden: true,
		Args:   cobra.NoArgs,
		RunE: func(_ *cobra.Command, _ []string) error {
			return runLumenStepper(stdout, stderr, "gc lumen step", runID, jsonOut,
				func(ctx context.Context, gs *graphstore.Store, doc *ir.IR, streamID string, input map[string]any) (engine.StepResult, error) {
					return engine.Step(ctx, gs, doc, streamID, input, engine.Options{})
				})
		},
	}
	c.Flags().StringVar(&runID, "run", "", "the run's journal stream id")
	c.Flags().BoolVar(&jsonOut, "json", false, "emit the result as a JSON object")
	return c
}

// newLumenSettleCmd registers `gc lumen settle --run <streamID> --node <id> --outcome
// <pass|fail|degraded|pending> --output <text>`: record the do's self-reported outcome and
// print the NEXT ready do (fusing the next step) or `done`.
func newLumenSettleCmd(stdout, stderr io.Writer) *cobra.Command {
	var runID, node, outcome, output string
	var jsonOut bool
	c := &cobra.Command{
		Use:    "settle",
		Short:  "Record a v1 do's self-reported outcome and print the next ready do or `done`",
		Hidden: true,
		Args:   cobra.NoArgs,
		RunE: func(_ *cobra.Command, _ []string) error {
			if strings.TrimSpace(node) == "" {
				fmt.Fprintf(stderr, "gc lumen settle: --node is required\n") //nolint:errcheck // best-effort stderr
				return errExit
			}
			if strings.TrimSpace(outcome) == "" {
				fmt.Fprintf(stderr, "gc lumen settle: --outcome is required (pass|fail|degraded|pending)\n") //nolint:errcheck // best-effort stderr
				return errExit
			}
			return runLumenStepper(stdout, stderr, "gc lumen settle", runID, jsonOut,
				func(ctx context.Context, gs *graphstore.Store, doc *ir.IR, streamID string, input map[string]any) (engine.StepResult, error) {
					return engine.Settle(ctx, gs, doc, streamID, input, node, outcome, output, engine.Options{})
				})
		},
	}
	c.Flags().StringVar(&runID, "run", "", "the run's journal stream id")
	c.Flags().StringVar(&node, "node", "", "the do node id being settled (as `gc lumen step` printed it)")
	c.Flags().StringVar(&outcome, "outcome", "", "the do's self-reported outcome: pass|fail|degraded|pending")
	c.Flags().StringVar(&output, "output", "", "the do's result text (consumed by a downstream {{ref}})")
	c.Flags().BoolVar(&jsonOut, "json", false, "emit the result as a JSON object")
	return c
}

// runLumenStepper is the shared body of both verbs: resolve the graph-scoped city, open
// the run's graph store, load its formula + input from the CAS manifest, run the stepper
// op, and print the result.
func runLumenStepper(stdout, stderr io.Writer, cmdName, runID string, jsonOut bool, op lumenStepperOp) error {
	if strings.TrimSpace(runID) == "" {
		fmt.Fprintf(stderr, "%s: --run <streamID> is required\n", cmdName) //nolint:errcheck // best-effort stderr
		return errExit
	}
	cityPath, err := resolveCity()
	if err != nil {
		fmt.Fprintf(stderr, "%s: %v\n", cmdName, err) //nolint:errcheck // best-effort stderr
		return errExit
	}
	if !cityHasGraphScope(cityPath) {
		fmt.Fprintf(stderr, "%s: city %q has no graph journal scope (.gc/graph)\n", cmdName, cityPath) //nolint:errcheck // best-effort stderr
		return errExit
	}
	ctx := context.Background()
	backend, err := loadGraphJournalBackendConfig(cityPath)
	if err != nil {
		fmt.Fprintf(stderr, "%s: resolving graph journal backend: %v\n", cmdName, err) //nolint:errcheck // best-effort stderr
		return errExit
	}
	gs, err := backend.openGraphStore(ctx, cityPath)
	if err != nil {
		fmt.Fprintf(stderr, "%s: opening graph store: %v\n", cmdName, err) //nolint:errcheck // best-effort stderr
		return errExit
	}
	defer func() { _ = gs.Close() }()

	m, err := engine.ReadRunManifest(ctx, gs, runID)
	if err != nil {
		fmt.Fprintf(stderr, "%s: reading run manifest %q: %v\n", cmdName, runID, err) //nolint:errcheck // best-effort stderr
		return errExit
	}
	doc, input, err := loadLumenRunInputs(cityPath, m)
	if err != nil {
		fmt.Fprintf(stderr, "%s: loading run inputs for %q: %v\n", cmdName, runID, err) //nolint:errcheck // best-effort stderr
		return errExit
	}

	res, err := op(ctx, gs, doc, runID, input)
	if err != nil {
		fmt.Fprintf(stderr, "%s: %v\n", cmdName, err) //nolint:errcheck // best-effort stderr
		return errExit
	}
	printLumenStepResult(stdout, stderr, strings.TrimPrefix(cmdName, "gc "), res, jsonOut)
	return nil
}

// lumenStepResultJSON is the machine-readable stepper result the driver-loop worker parses
// (via jq): done + outcome at the seal, else the next ready do's node id + rendered prompt.
// It carries the SchemaVersion/OK/Command discriminators every gc `--json` result exposes:
// the gc front-door intercepts `--json` before cobra and refuses a command with no
// registered result schema (schemas/lumen/{step,settle}/result.schema.json), so a stepper
// verb that emitted a bare object would fail `json_unsupported` and the driver's
// `step="$(gc lumen step … --json)"` would die under `set -e` before the first turn.
type lumenStepResultJSON struct {
	SchemaVersion string `json:"schema_version"`
	OK            bool   `json:"ok"`
	Command       string `json:"command"`
	Done          bool   `json:"done"`
	Node          string `json:"node,omitempty"`
	Activation    string `json:"activation,omitempty"`
	Prompt        string `json:"prompt,omitempty"`
	Outcome       string `json:"outcome,omitempty"`
}

// printLumenStepResult renders a stepper result. --json emits one object per line (the
// driver worker's parse surface, validated against the command's result schema); the human
// form prints `done (outcome: X)` at the seal, else the node id on its own first line
// followed by the rendered prompt. command is the front-door command path ("lumen step" /
// "lumen settle") stamped into the JSON result's command discriminator.
func printLumenStepResult(stdout, stderr io.Writer, command string, res engine.StepResult, jsonOut bool) {
	if jsonOut {
		if err := writeCLIJSONLine(stdout, lumenStepResultJSON{
			SchemaVersion: "1",
			OK:            true,
			Command:       command,
			Done:          res.Done,
			Node:          res.NodeID,
			Activation:    res.Activation,
			Prompt:        res.Prompt,
			Outcome:       res.Outcome,
		}); err != nil {
			fmt.Fprintf(stderr, "gc %s: writing JSON result: %v\n", command, err) //nolint:errcheck // best-effort stderr
		}
		return
	}
	if res.Done {
		fmt.Fprintf(stdout, "done (outcome: %s)\n", res.Outcome) //nolint:errcheck // best-effort stdout
		return
	}
	fmt.Fprintf(stdout, "%s\n%s\n", res.NodeID, res.Prompt) //nolint:errcheck // best-effort stdout
}
