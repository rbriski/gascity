package main

import (
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/spf13/cobra"
)

// The bd shim (thin client) is a bd-CLI-compatible front end that routes a
// worker's bead operations through the in-process coordrouter Router, so graph
// beads (relocated to the embedded SQLite store under graph_store=sqlite) are
// seen and mutated, while work beads still reach the real bd binary. Installed
// as `bd` first on an agent's PATH, it makes both raw `bd` and `gc bd` route
// transparently with no prompt changes (graph-store-rollout-plan.md §C2,
// model A in graph-store-session-handoff.md).
//
// Each bd subcommand has one of three dispositions (classifyBdShimVerb):
//
//   - bdRoute       — translated to an in-process Router store op (graph-aware).
//   - bdPassthrough — execed to the real bd (GC_BD_REAL), for ops that provably
//     never touch graph-class data, and for everything in the identity phase
//     (graph_store off → one backend → byte-identical to raw bd).
//   - bdRefuse      — graph-touching ops not yet routed (bd mol / gate / query
//     ephemeral): refused loudly in the split phase rather than silently passed
//     to the work-only bd, where they would drop graph data (§X2). This is the
//     CLOSED-allowlist safety property: passthrough is never a graph-class
//     catch-all.

// realBdEnvVar names the environment variable holding the absolute path of the
// real bd binary. The shim must resolve bd through this, never exec.LookPath,
// because once it is installed as `bd` on PATH a LookPath would resolve back to
// the shim and recurse (graph-store-rollout-plan.md §C2). GC_BD_REAL is
// captured at install time as an absolute path.
const realBdEnvVar = "GC_BD_REAL"

// bdShimDisposition is how the shim handles one bd subcommand.
type bdShimDisposition int

const (
	// bdPassthrough execs the real bd binary unchanged.
	bdPassthrough bdShimDisposition = iota
	// bdRoute translates the verb to an in-process Router store op.
	bdRoute
	// bdRefuse rejects the verb rather than silently bypassing the graph store.
	bdRefuse
)

func (d bdShimDisposition) String() string {
	switch d {
	case bdRoute:
		return "route"
	case bdRefuse:
		return "refuse"
	default:
		return "passthrough"
	}
}

// bdShimRoutedVerbs are bd subcommands the shim translates to in-process Router
// store ops so graph beads in the embedded SQLite store are seen and mutated,
// not just Dolt work beads. Grown incrementally.
var bdShimRoutedVerbs = map[string]bool{
	"close": true,
	"show":  true,
	"ready": true,
}

// bdReadyRoutableFlags are the `bd ready` flags Router.Ready replicates exactly
// (Assignee/Limit, plus output/tier flags that are no-ops here). A ready
// invocation carrying any OTHER flag — the pool-demand predicates
// (--metadata-field, --unassigned, --exclude-type, --sort, --label, ...) — is
// not yet federated (predicate parity is C3/ga-2gap48.11), so it passes through
// to the real bd (byte-identical in the identity phase) rather than silently
// dropping the filter.
var bdReadyRoutableFlags = map[string]bool{
	"--assignee":          true,
	"--limit":             true,
	"-n":                  true,
	"--json":              true,
	"--include-ephemeral": true,
}

// bdReadyRoutable reports whether a `bd ready` arg list uses only flags the
// Router can replicate, so the shim can serve it in-process.
func bdReadyRoutable(args []string) bool {
	for _, a := range args {
		if !strings.HasPrefix(a, "-") {
			continue // a bare value (e.g. a space-separated flag arg) — not a gate
		}
		name := a
		if i := strings.IndexByte(a, '='); i >= 0 {
			name = a[:i]
		}
		if !bdReadyRoutableFlags[name] {
			return false
		}
	}
	return true
}

// bdShimGraphTouchingUnroutedVerbs are bd subcommands that read or mutate
// graph/wisp data but are not yet translated to Router ops. Passing them to the
// real (work-only) bd is byte-identical and safe while graph storage is off
// (the identity phase), but would SILENTLY miss graph beads once
// graph_store=sqlite is on — so in the split phase the shim refuses them loudly
// rather than dropping graph data (graph-store-rollout-plan.md §X2).
var bdShimGraphTouchingUnroutedVerbs = map[string]bool{
	"mol":   true, // bd mol current|progress — molecule topology lives in the graph store
	"gate":  true, // bd gate check --escalate — a mutation on gate beads
	"query": true, // bd query 'ephemeral=...' — the wisp/ephemeral discovery tier
}

// classifyBdShimVerb decides how the shim handles a bd subcommand given whether
// the city is in the split phase (graph_store=sqlite active, so a distinct
// graph backend exists). See the bdShimDisposition docs above.
func classifyBdShimVerb(verb string, args []string, splitPhase bool) bdShimDisposition {
	if bdShimRoutedVerbs[verb] {
		if verb == "ready" && !bdReadyRoutable(args) {
			return bdPassthrough
		}
		return bdRoute
	}
	if splitPhase && bdShimGraphTouchingUnroutedVerbs[verb] {
		return bdRefuse
	}
	return bdPassthrough
}

// resolveRealBdPath returns the absolute path of the real bd binary the shim
// delegates passthrough ops to. It prefers GC_BD_REAL (an install-time absolute
// path) and only falls back to exec.LookPath("bd") when GC_BD_REAL is unset —
// the fallback is unsafe once the shim is installed as bd on PATH, so
// production installs always set GC_BD_REAL.
func resolveRealBdPath() (string, error) {
	if raw := strings.TrimSpace(os.Getenv(realBdEnvVar)); raw != "" {
		if !filepath.IsAbs(raw) {
			return "", fmt.Errorf("%s must be an absolute path, got %q", realBdEnvVar, raw)
		}
		if _, err := os.Stat(raw); err != nil {
			return "", fmt.Errorf("%s=%q: %w", realBdEnvVar, raw, err)
		}
		return raw, nil
	}
	path, err := exec.LookPath("bd")
	if err != nil {
		return "", fmt.Errorf("bd not found: set %s to the real bd binary or put bd on PATH: %w", realBdEnvVar, err)
	}
	return path, nil
}

// execRealBd runs the real bd binary with the given args in dir, streaming its
// stdio and propagating its exit code — preserving bd's exit-code contract. It
// resolves bd via resolveRealBdPath (never a bare LookPath in the shim's own
// dispatch) so it cannot recurse into the shim.
func execRealBd(args []string, dir string, stdin io.Reader, stdout, stderr io.Writer) int {
	bdPath, err := resolveRealBdPath()
	if err != nil {
		fmt.Fprintf(stderr, "gc bd-shim: %v\n", err) //nolint:errcheck // best-effort stderr
		return 1
	}
	cmd := exec.Command(bdPath, args...)
	if dir != "" {
		cmd.Dir = dir
	}
	cmd.Stdin = stdin
	cmd.Stdout = stdout
	cmd.Stderr = stderr
	cmd.Env = os.Environ()
	if err := cmd.Run(); err != nil {
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) {
			return exitErr.ExitCode()
		}
		fmt.Fprintf(stderr, "gc bd-shim: %v\n", err) //nolint:errcheck // best-effort stderr
		return 1
	}
	return 0
}

// dispatchBdShimVerb translates a single routed bd subcommand into an in-process
// store op against the Router-wrapped store. store must be the per-scope Router
// (or its policy wrapper) so by-id ops land on the owning backend (graph vs
// work). Stdout byte-parity with raw bd is deferred to the C2a corpus gate
// (ga-2gap48.10); this enforces the routing + exit-code contract.
func dispatchBdShimVerb(store beads.Store, verb string, args []string, _ io.Reader, stdout, stderr io.Writer) int {
	switch verb {
	case "close":
		if len(args) < 1 {
			fmt.Fprintln(stderr, "gc bd-shim: usage: close <id>") //nolint:errcheck // best-effort stderr
			return 1
		}
		if err := store.Close(args[0]); err != nil {
			fmt.Fprintf(stderr, "gc bd-shim: closing %q: %v\n", args[0], err) //nolint:errcheck // best-effort stderr
			return 1
		}
		return 0
	case "show":
		id, ok := firstBdPositional(args)
		if !ok {
			fmt.Fprintln(stderr, "gc bd-shim: usage: show <id>") //nolint:errcheck // best-effort stderr
			return 1
		}
		bead, err := store.Get(id)
		if err != nil {
			if errors.Is(err, beads.ErrNotFound) {
				// Raw bd prints an empty array (exit 0) for an unknown id; a
				// `bd show ... --json | jq '.[0]'` consumer reads that as absent.
				return writeReadyJSON(nil, stdout, stderr)
			}
			fmt.Fprintf(stderr, "gc bd-shim: show %q: %v\n", id, err) //nolint:errcheck // best-effort stderr
			return 1
		}
		return writeReadyJSON([]beads.Bead{bead}, stdout, stderr)
	case "ready":
		q, err := parseBdReadyQuery(args)
		if err != nil {
			fmt.Fprintf(stderr, "gc bd-shim: %v\n", err) //nolint:errcheck // best-effort stderr
			return 1
		}
		out, err := store.Ready(q)
		if err != nil {
			fmt.Fprintf(stderr, "gc bd-shim: ready: %v\n", err) //nolint:errcheck // best-effort stderr
			return 1
		}
		return writeReadyJSON(out, stdout, stderr)
	default:
		fmt.Fprintf(stderr, "gc bd-shim: no routed handler for %q\n", verb) //nolint:errcheck // best-effort stderr
		return 1
	}
}

// firstBdPositional returns the first non-flag argument (a bead id), or false
// when every argument is a flag.
func firstBdPositional(args []string) (string, bool) {
	for _, a := range args {
		if !strings.HasPrefix(a, "-") {
			return a, true
		}
	}
	return "", false
}

// parseBdReadyQuery maps the routable `bd ready` flags to a beads.ReadyQuery.
// Only --assignee and --limit/-n affect the query; --json and
// --include-ephemeral are accepted no-ops (output is always JSON, and tier
// expansion is the policy wrapper's job above the Router). Non-routable
// predicate flags never reach here — classifyBdShimVerb passes those through.
func parseBdReadyQuery(args []string) (beads.ReadyQuery, error) {
	var q beads.ReadyQuery
	for i := 0; i < len(args); i++ {
		a := args[i]
		switch {
		case a == "--assignee" && i+1 < len(args):
			q.Assignee = args[i+1]
			i++
		case strings.HasPrefix(a, "--assignee="):
			q.Assignee = strings.TrimPrefix(a, "--assignee=")
		case (a == "--limit" || a == "-n") && i+1 < len(args):
			n, err := strconv.Atoi(args[i+1])
			if err != nil {
				return q, fmt.Errorf("parse %s %q: %w", a, args[i+1], err)
			}
			q.Limit = n
			i++
		case strings.HasPrefix(a, "--limit="):
			n, err := strconv.Atoi(strings.TrimPrefix(a, "--limit="))
			if err != nil {
				return q, fmt.Errorf("parse %q: %w", a, err)
			}
			q.Limit = n
		}
	}
	return q, nil
}

// runBdShim is the bd-compatible thin-client entry point. It resolves the city,
// classifies the bd subcommand, and either routes it through the in-process
// Router, passes it through to the real bd, or refuses it (see the package doc
// above). Scope is the city root for now; rig scope resolution and passthrough
// env parity land in a later increment.
func runBdShim(args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	cityName, _, bdArgs := extractBdScopeFlags(args)
	if len(bdArgs) == 0 {
		fmt.Fprintln(stderr, "gc bd-shim: missing bd subcommand") //nolint:errcheck // best-effort stderr
		return 1
	}
	verb := bdArgs[0]

	cityPath, err := resolveBdCity(cityName)
	if err != nil {
		fmt.Fprintf(stderr, "gc bd-shim: %v\n", err) //nolint:errcheck // best-effort stderr
		return 1
	}
	cfg, err := loadCityConfig(cityPath, stderr)
	if err != nil {
		fmt.Fprintf(stderr, "gc bd-shim: loading config: %v\n", err) //nolint:errcheck // best-effort stderr
		return 1
	}

	switch classifyBdShimVerb(verb, bdArgs[1:], graphStoreSQLiteEnabled(cfg)) {
	case bdRoute:
		store, err := openStoreAtForCity(cityPath, cityPath)
		if err != nil {
			fmt.Fprintf(stderr, "gc bd-shim: opening store: %v\n", err) //nolint:errcheck // best-effort stderr
			return 1
		}
		defer closeBeadStoreHandle(store) //nolint:errcheck // best-effort close
		return dispatchBdShimVerb(store, verb, bdArgs[1:], stdin, stdout, stderr)
	case bdRefuse:
		fmt.Fprintf(stderr, "gc bd-shim: %q reads or mutates graph-class beads but is not yet routed through the graph store; refusing to pass it to the work-only bd while graph_store=sqlite is active (would silently miss graph beads — see graph-store-rollout-plan.md §X2)\n", verb) //nolint:errcheck // best-effort stderr
		return 1
	default: // bdPassthrough
		return execRealBd(bdArgs, cityPath, stdin, stdout, stderr)
	}
}

// newBdShimCmd registers the hidden `gc bd-shim` subcommand: the bd-compatible
// thin client. It is hidden because operators invoke it as `bd` (via a PATH
// install), not by name; exposing it as a gc subcommand keeps it testable and
// lets the install point a `bd` shim at `gc bd-shim`.
func newBdShimCmd(stdout, stderr io.Writer) *cobra.Command {
	return &cobra.Command{
		Use:                "bd-shim [bd-args...]",
		Short:              "bd-compatible thin client routing graph beads through the in-process Router",
		Hidden:             true,
		DisableFlagParsing: true,
		RunE: func(_ *cobra.Command, args []string) error {
			return exitForCode(runBdShim(args, os.Stdin, stdout, stderr))
		},
	}
}
