package main

import (
	"fmt"
	"io"
	"strings"

	"github.com/gastownhall/gascity/internal/api"
	"github.com/gastownhall/gascity/internal/beads"
)

// bdMolAPIClient constructs the API client used to federate
// `gc bd mol current|progress` across a split city's work and infra stores. It
// is a package var so tests can point it at a fake controller; in production it
// resolves to the running controller/supervisor client for the city (nil when
// no controller is up).
var bdMolAPIClient = apiClient

// molStepJSON mirrors bd's StepStatus JSON (beads cmd/bd/mol_current.go): the
// step bead, its molecule-relative status, and whether it is the in_progress
// step. bd renders open steps as [ready]/[blocked] using within-molecule
// dependency edges; the graph endpoint returns parent-child topology but not
// blocking deps, so open steps render as "pending" here (mol is LLM-facing
// situational awareness, so a faithful render suffices).
type molStepJSON struct {
	Issue     beads.Bead `json:"issue"`
	Status    string     `json:"status"`
	IsCurrent bool       `json:"is_current"`
}

// molProgressJSON mirrors bd's MoleculeProgress JSON (beads cmd/bd/mol_current.go).
// `bd mol current --json` emits a JSON ARRAY of these; matching that shape keeps
// the `.steps` field a populated array instead of null, which is what a split
// city's single-store passthrough returns (the step beads live in the infra
// store, invisible to the work-scoped bd).
type molProgressJSON struct {
	MoleculeID    string        `json:"molecule_id"`
	MoleculeTitle string        `json:"molecule_title"`
	Assignee      string        `json:"assignee,omitempty"`
	Steps         []molStepJSON `json:"steps"`
	Completed     int           `json:"completed"`
	Total         int           `json:"total"`
}

// molProgressSummaryJSON mirrors bd's `mol progress --json` object (beads
// cmd/bd/mol_progress.go): an efficient counts-only summary. Percent is a
// pointer so it is omitted for an empty molecule, exactly as bd omits it when
// total is 0.
type molProgressSummaryJSON struct {
	MoleculeID    string   `json:"molecule_id"`
	MoleculeTitle string   `json:"molecule_title"`
	Total         int      `json:"total"`
	Completed     int      `json:"completed"`
	InProgress    int      `json:"in_progress"`
	CurrentStepID string   `json:"current_step_id"`
	Percent       *float64 `json:"percent,omitempty"`
}

// maybeRouteBdMolViaAPI intercepts `gc bd mol current|progress <id> [--json]` on
// a split city and answers it from the controller's federated bead graph instead
// of the single-store `bd` exec. It returns (exitCode, true) when it handled the
// command, or (0, false) to let doBd fall through to the normal passthrough.
//
// The passthrough is left unchanged when: the args are not a routable mol read
// (other mol subcommands, an omitted id, or view flags bd infers/filters on);
// the city is single-store (one backend already sees every step); or no
// controller is reachable (best-effort — no worse than today's single-store
// behavior).
func maybeRouteBdMolViaAPI(cityPath string, bdArgs []string, stdout, stderr io.Writer) (int, bool) {
	if len(bdArgs) == 0 || bdArgs[0] != "mol" {
		return 0, false
	}
	sub, id, jsonOut, ok := bdMolRoutable(bdArgs[1:])
	if !ok {
		return 0, false
	}
	// A single-store city's `bd` already sees every step bead — leave it on the
	// byte-identical passthrough. Only a split city hides graph-class step beads
	// in the infra store from the work-scoped `bd`.
	if !cityHasInfraStore(cityPath) {
		return 0, false
	}
	client := bdMolAPIClient(cityPath)
	if client == nil {
		// No controller/supervisor is up to federate the graph. Fall back to the
		// single-store passthrough rather than hard-failing the read.
		return 0, false
	}
	return dispatchBdMolViaAPI(client, sub, id, jsonOut, stdout, stderr), true
}

// bdMolRoutable reports whether a `bd mol` arg list (the args after "mol") is a
// routable read — `current` or `progress` with an explicit molecule id and at
// most --json — and returns the parsed subcommand/id/json. Other subcommands
// (pour/wisp/bond/...), an omitted id (bd infers it from in_progress issues,
// which the rooted graph endpoint cannot express), or view flags
// (--for/--limit/--range) are not faithfully routable.
func bdMolRoutable(args []string) (sub, id string, jsonOut, ok bool) {
	if len(args) < 2 {
		return "", "", false, false
	}
	sub = args[0]
	if sub != "current" && sub != "progress" {
		return "", "", false, false
	}
	for _, a := range args[1:] {
		switch {
		case a == "--json":
			jsonOut = true
		case strings.HasPrefix(a, "-"):
			return "", "", false, false // --for/--limit/--range: not routable
		default:
			if id != "" {
				return "", "", false, false
			}
			id = a
		}
	}
	if id == "" {
		return "", "", false, false
	}
	return sub, id, jsonOut, true
}

// bdMolRoutableArgs reports whether args (the tokens after "mol") are a routable
// `current|progress <id>` read.
func bdMolRoutableArgs(args []string) bool {
	_, _, _, ok := bdMolRoutable(args)
	return ok
}

// dispatchBdMolViaAPI fetches the federated bead graph for id and renders
// `bd mol current|progress` from the returned topology. The routed source
// reaches infra-store-resident steps the single-store passthrough cannot see.
func dispatchBdMolViaAPI(client *api.Client, sub, id string, jsonOut bool, stdout, stderr io.Writer) int {
	g, err := client.GetBeadGraph(id)
	if err != nil {
		fmt.Fprintf(stderr, "gc bd: mol %s %q via API: %v\n", sub, id, err) //nolint:errcheck // best-effort stderr
		return 1
	}
	steps := molSteps(g)
	if sub == "progress" {
		return renderBdMolProgress(g, steps, jsonOut, stdout, stderr)
	}
	return renderBdMolCurrent(g, steps, jsonOut, stdout, stderr)
}

// molSteps returns the molecule's step beads (every graph bead except the root),
// preserving the endpoint's order.
func molSteps(g api.BeadGraph) []beads.Bead {
	steps := make([]beads.Bead, 0, len(g.Beads))
	for _, b := range g.Beads {
		if b.ID == g.Root.ID {
			continue
		}
		steps = append(steps, b)
	}
	return steps
}

// molStepIndicator maps a step bead's status to bd's molecule-relative status
// token (done=closed, current=in_progress, pending=open/other).
func molStepIndicator(b beads.Bead) string {
	switch b.Status {
	case "closed":
		return "done"
	case "in_progress":
		return "current"
	default:
		return "pending"
	}
}

// renderBdMolCurrent renders `bd mol current`. In --json mode it emits the shape
// bd emits — a JSON array of molecule-progress objects with a populated `steps`
// field (each {issue,status,is_current}) plus progress counts — so a split city
// no longer returns steps: null. In text mode it prints a faithful step list for
// LLM situational awareness.
func renderBdMolCurrent(g api.BeadGraph, steps []beads.Bead, jsonOut bool, stdout, stderr io.Writer) int {
	if jsonOut {
		return writeMolJSON(stdout, stderr, []molProgressJSON{molProgressFromGraph(g, steps)})
	}
	done := 0
	for _, b := range steps {
		if b.Status == "closed" {
			done++
		}
	}
	fmt.Fprintf(stdout, "Molecule %s — %s (%d/%d done)\n", g.Root.ID, g.Root.Title, done, len(steps)) //nolint:errcheck // best-effort stdout
	for _, b := range steps {
		fmt.Fprintf(stdout, "  [%s] %s %s\n", molStepIndicator(b), b.ID, b.Title) //nolint:errcheck // best-effort stdout
	}
	return 0
}

// molProgressFromGraph builds the bd MoleculeProgress projection for one
// molecule from its root and step beads.
func molProgressFromGraph(g api.BeadGraph, steps []beads.Bead) molProgressJSON {
	out := molProgressJSON{
		MoleculeID:    g.Root.ID,
		MoleculeTitle: g.Root.Title,
		Assignee:      g.Root.Assignee,
		Steps:         make([]molStepJSON, 0, len(steps)),
		Total:         len(steps),
	}
	for _, b := range steps {
		status := molStepIndicator(b)
		out.Steps = append(out.Steps, molStepJSON{
			Issue:     b,
			Status:    status,
			IsCurrent: status == "current",
		})
		if status == "done" {
			out.Completed++
		}
	}
	return out
}

// renderBdMolProgress renders `bd mol progress`. In --json mode it emits bd's
// progress object (molecule_id/total/completed/in_progress/current_step_id and,
// when total>0, percent). In text mode it prints the one-line summary.
func renderBdMolProgress(g api.BeadGraph, steps []beads.Bead, jsonOut bool, stdout, stderr io.Writer) int {
	done, inProgress := 0, 0
	currentStepID := ""
	for _, b := range steps {
		switch b.Status {
		case "closed":
			done++
		case "in_progress":
			inProgress++
			if currentStepID == "" {
				currentStepID = b.ID
			}
		}
	}
	total := len(steps)
	if jsonOut {
		summary := molProgressSummaryJSON{
			MoleculeID:    g.Root.ID,
			MoleculeTitle: g.Root.Title,
			Total:         total,
			Completed:     done,
			InProgress:    inProgress,
			CurrentStepID: currentStepID,
		}
		if total > 0 {
			pct := float64(done) * 100 / float64(total)
			summary.Percent = &pct
		}
		return writeMolJSON(stdout, stderr, summary)
	}
	pct := 0
	if total > 0 {
		pct = done * 100 / total
	}
	fmt.Fprintf(stdout, "%s: %d/%d steps complete (%d%%)\n", g.Root.ID, done, total, pct) //nolint:errcheck // best-effort stdout
	return 0
}

// writeMolJSON marshals a mol render payload to stdout, reporting a non-zero
// exit and a stderr diagnostic on an encoding failure.
func writeMolJSON(stdout, stderr io.Writer, value any) int {
	if err := writeJSON(stdout, value); err != nil {
		fmt.Fprintf(stderr, "gc bd: mol: %v\n", err) //nolint:errcheck // best-effort stderr
		return 1
	}
	return 0
}
