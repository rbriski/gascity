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
	"close":  true,
	"show":   true,
	"ready":  true,
	"update": true,
	"reopen": true,
	"delete": true,
}

// bdUpdateRoutableFlags are the `bd update` flags that map cleanly onto
// beads.UpdateOpts. A bd update carrying any OTHER flag (--claim, --notes,
// --note, --persistent, --unset-metadata, ...) has no faithful in-process
// translation yet, so it passes through to the real bd (byte-identical in the
// identity phase) rather than silently losing the unmapped effect.
var bdUpdateRoutableFlags = map[string]bool{
	"--status":       true,
	"--set-metadata": true,
	"--assignee":     true,
	"--label":        true,
	"--remove-label": true,
	"--title":        true,
	"--type":         true,
	"--priority":     true,
	"--description":  true,
	"--parent":       true,
	"--json":         true,
}

// bdUpdateFlagNeedsValue is the subset of routable update flags that consume the
// following token as their value when written space-separated (--flag value).
var bdUpdateFlagNeedsValue = map[string]bool{
	"--status":       true,
	"--set-metadata": true,
	"--assignee":     true,
	"--label":        true,
	"--remove-label": true,
	"--title":        true,
	"--type":         true,
	"--priority":     true,
	"--description":  true,
	"--parent":       true,
}

// bdUpdateRoutable reports whether a `bd update` arg list uses only flags that
// map onto beads.UpdateOpts, so the shim can serve it in-process.
func bdUpdateRoutable(args []string) bool {
	for _, a := range args {
		if !strings.HasPrefix(a, "-") {
			continue // the id positional or a space-separated flag value
		}
		name := a
		if i := strings.IndexByte(a, '='); i >= 0 {
			name = a[:i]
		}
		if !bdUpdateRoutableFlags[name] {
			return false
		}
	}
	return true
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
	// Discovery predicates the controller's serve loop and the pool-demand probe
	// use. The Router's ReadyQuery cannot express these, so the shim federates
	// store.Ready() and applies them as a Go-side post-filter (parseBdReadyParams
	// / applyBdReadyParams). This is what lets a graph control bead in SQLite be
	// discovered through `bd ready` (the deployed graph_store=sqlite crux).
	"--metadata-field": true,
	"--unassigned":     true,
	"--exclude-type":   true,
	"--sort":           true,
}

// bdReadyRoutable reports whether a `bd ready` arg list uses only flags the shim
// can replicate (directly via ReadyQuery or via the discovery post-filter).
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

// splitBdGlobalFlags finds the bd subcommand past any leading global flags. bd
// accepts global flags before the subcommand (e.g. `bd --readonly --sandbox
// ready ...`, the controller's discovery form), so the verb is not always
// args[0]. It returns the verb and the args that follow it; leading global flags
// are dropped (they govern bd's execution mode, irrelevant to in-process Router
// reads). Returns ("", nil) when there is no subcommand.
func splitBdGlobalFlags(args []string) (string, []string) {
	for i, a := range args {
		if !strings.HasPrefix(a, "-") {
			return a, args[i+1:]
		}
	}
	return "", nil
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
		switch verb {
		case "ready":
			if !bdReadyRoutable(args) {
				return bdPassthrough
			}
		case "update":
			if !bdUpdateRoutable(args) {
				return bdPassthrough
			}
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
// dispatch) so it cannot recurse into the shim. A nil env defaults to the
// process environment; passthrough callers pass the projected bd scope env.
func execRealBd(args []string, dir string, env []string, stdin io.Reader, stdout, stderr io.Writer) int {
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
	if env == nil {
		env = os.Environ()
	}
	cmd.Env = env
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
		p, err := parseBdReadyParams(args)
		if err != nil {
			fmt.Fprintf(stderr, "gc bd-shim: %v\n", err) //nolint:errcheck // best-effort stderr
			return 1
		}
		out, err := store.Ready(p.query)
		if err != nil {
			fmt.Fprintf(stderr, "gc bd-shim: ready: %v\n", err) //nolint:errcheck // best-effort stderr
			return 1
		}
		return writeReadyJSON(applyBdReadyParams(out, p), stdout, stderr)
	case "update":
		id, ok := firstBdPositional(args)
		if !ok {
			fmt.Fprintln(stderr, "gc bd-shim: usage: update <id> [flags]") //nolint:errcheck // best-effort stderr
			return 1
		}
		opts, err := parseBdUpdateOpts(args)
		if err != nil {
			fmt.Fprintf(stderr, "gc bd-shim: update %q: %v\n", id, err) //nolint:errcheck // best-effort stderr
			return 1
		}
		if err := store.Update(id, opts); err != nil {
			fmt.Fprintf(stderr, "gc bd-shim: update %q: %v\n", id, err) //nolint:errcheck // best-effort stderr
			return 1
		}
		return 0
	case "reopen":
		id, ok := firstBdPositional(args)
		if !ok {
			fmt.Fprintln(stderr, "gc bd-shim: usage: reopen <id>") //nolint:errcheck // best-effort stderr
			return 1
		}
		if err := store.Reopen(id); err != nil {
			fmt.Fprintf(stderr, "gc bd-shim: reopen %q: %v\n", id, err) //nolint:errcheck // best-effort stderr
			return 1
		}
		return 0
	case "delete":
		id, ok := firstBdPositional(args)
		if !ok {
			fmt.Fprintln(stderr, "gc bd-shim: usage: delete <id>") //nolint:errcheck // best-effort stderr
			return 1
		}
		if err := store.Delete(id); err != nil {
			fmt.Fprintf(stderr, "gc bd-shim: delete %q: %v\n", id, err) //nolint:errcheck // best-effort stderr
			return 1
		}
		return 0
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

// bdReadyParams is a parsed `bd ready` invocation. query carries the predicates
// the Router's ReadyQuery can express (Assignee); the rest are applied as a
// Go-side post-filter over the federated ready set (so they work against the
// SQLite graph backend the ReadyQuery itself cannot describe). limit is applied
// after filtering — it bounds the post-filtered result, matching bd.
type bdReadyParams struct {
	query          beads.ReadyQuery
	metadataEquals map[string]string // --metadata-field k=v (all must match)
	unassigned     bool              // --unassigned
	excludeTypes   map[string]bool   // --exclude-type=T (repeatable)
	limit          int               // --limit / -n
}

// parseBdReadyParams parses the routable `bd ready` flags. --assignee feeds the
// ReadyQuery; --metadata-field/--unassigned/--exclude-type/--limit feed the
// post-filter; --json/--include-ephemeral/--sort are accepted no-ops (output is
// always JSON, tier expansion is the policy wrapper's job, and the federated
// ready set is already created-asc which is bd's "oldest" order). Non-routable
// flags never reach here — classifyBdShimVerb passes those through.
func parseBdReadyParams(args []string) (bdReadyParams, error) {
	p := bdReadyParams{metadataEquals: map[string]string{}, excludeTypes: map[string]bool{}}
	for i := 0; i < len(args); i++ {
		a := args[i]
		switch {
		case a == "--assignee" && i+1 < len(args):
			p.query.Assignee = args[i+1]
			i++
		case strings.HasPrefix(a, "--assignee="):
			p.query.Assignee = strings.TrimPrefix(a, "--assignee=")
		case a == "--unassigned":
			p.unassigned = true
		case (a == "--metadata-field") && i+1 < len(args):
			if err := addMetadataEquals(p.metadataEquals, args[i+1]); err != nil {
				return p, err
			}
			i++
		case strings.HasPrefix(a, "--metadata-field="):
			if err := addMetadataEquals(p.metadataEquals, strings.TrimPrefix(a, "--metadata-field=")); err != nil {
				return p, err
			}
		case a == "--exclude-type" && i+1 < len(args):
			p.excludeTypes[args[i+1]] = true
			i++
		case strings.HasPrefix(a, "--exclude-type="):
			p.excludeTypes[strings.TrimPrefix(a, "--exclude-type=")] = true
		case (a == "--limit" || a == "-n") && i+1 < len(args):
			n, err := strconv.Atoi(args[i+1])
			if err != nil {
				return p, fmt.Errorf("parse %s %q: %w", a, args[i+1], err)
			}
			p.limit = n
			i++
		case strings.HasPrefix(a, "--limit="):
			n, err := strconv.Atoi(strings.TrimPrefix(a, "--limit="))
			if err != nil {
				return p, fmt.Errorf("parse %q: %w", a, err)
			}
			p.limit = n
		case a == "--sort" && i+1 < len(args):
			i++ // value consumed; federated ready is already created-asc
		}
	}
	return p, nil
}

// addMetadataEquals records a `k=v` --metadata-field predicate.
func addMetadataEquals(into map[string]string, kv string) error {
	k, v, ok := strings.Cut(kv, "=")
	if !ok {
		return fmt.Errorf("--metadata-field expects key=value, got %q", kv)
	}
	into[k] = v
	return nil
}

// applyBdReadyParams filters a federated ready set by the post-filter predicates
// and applies the limit last. The input is assumed created-asc (Router.Ready's
// canonical order), so a `--limit N` after filtering matches `bd ready ... -n N`.
func applyBdReadyParams(in []beads.Bead, p bdReadyParams) []beads.Bead {
	out := make([]beads.Bead, 0, len(in))
	for _, b := range in {
		if p.unassigned && strings.TrimSpace(b.Assignee) != "" {
			continue
		}
		if p.excludeTypes[b.Type] {
			continue
		}
		match := true
		for k, v := range p.metadataEquals {
			if b.Metadata[k] != v {
				match = false
				break
			}
		}
		if !match {
			continue
		}
		out = append(out, b)
	}
	if p.limit > 0 && len(out) > p.limit {
		out = out[:p.limit]
	}
	return out
}

// parseBdUpdateOpts maps the routable `bd update` flags onto a beads.UpdateOpts.
// It ignores the leading id positional; only flags in bdUpdateRoutableFlags
// reach here (classifyBdShimVerb passes the rest through), so an unknown flag is
// silently skipped rather than erroring. --set-metadata is repeatable.
func parseBdUpdateOpts(args []string) (beads.UpdateOpts, error) {
	var opts beads.UpdateOpts
	for i := 0; i < len(args); i++ {
		a := args[i]
		if !strings.HasPrefix(a, "-") {
			continue // the id positional or a consumed value
		}
		name := a
		val := ""
		hasVal := false
		if eq := strings.IndexByte(a, '='); eq >= 0 {
			name, val, hasVal = a[:eq], a[eq+1:], true
		}
		if !hasVal && bdUpdateFlagNeedsValue[name] && i+1 < len(args) {
			val = args[i+1]
			hasVal = true
			i++
		}
		switch name {
		case "--status":
			s := val
			opts.Status = &s
		case "--assignee":
			s := val
			opts.Assignee = &s
		case "--title":
			s := val
			opts.Title = &s
		case "--type":
			s := val
			opts.Type = &s
		case "--description":
			s := val
			opts.Description = &s
		case "--parent":
			s := val
			opts.ParentID = &s
		case "--priority":
			n, err := strconv.Atoi(val)
			if err != nil {
				return opts, fmt.Errorf("parse --priority %q: %w", val, err)
			}
			opts.Priority = &n
		case "--label":
			opts.Labels = append(opts.Labels, val)
		case "--remove-label":
			opts.RemoveLabels = append(opts.RemoveLabels, val)
		case "--set-metadata":
			k, mv, ok := strings.Cut(val, "=")
			if !ok {
				return opts, fmt.Errorf("--set-metadata expects key=value, got %q", val)
			}
			if opts.Metadata == nil {
				opts.Metadata = map[string]string{}
			}
			opts.Metadata[k] = mv
		}
	}
	return opts, nil
}

// runBdShim is the bd-compatible thin-client entry point. It resolves the scope
// (rig vs city) exactly as `gc bd` does, classifies the bd subcommand, and then
// either routes it through the in-process Router (graph-aware), passes it
// through to the real bd in the resolved scope with the projected bd env, or
// refuses it (see the package doc above).
func runBdShim(args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	cityName, rigName, bdArgs := extractBdScopeFlags(args)

	// Expand the gc-only `heartbeat <id>` verb into the bd write that performs
	// it, then route that write by id — shared with `gc bd`.
	bdArgs, err := rewriteBdHeartbeatArgs(bdArgs)
	if err != nil {
		fmt.Fprintf(stderr, "gc bd-shim: %v\n", err) //nolint:errcheck // best-effort stderr
		return 1
	}
	if len(bdArgs) == 0 {
		fmt.Fprintln(stderr, "gc bd-shim: missing bd subcommand") //nolint:errcheck // best-effort stderr
		return 1
	}

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
	target, err := resolveBdScopeTarget(cfg, cityPath, rigName, bdArgs)
	if err != nil {
		fmt.Fprintf(stderr, "gc bd-shim: %v\n", err) //nolint:errcheck // best-effort stderr
		return 1
	}

	// release-if-current is a gc-only verb whose conditional release routes by id
	// through the store's ConditionalAssignmentReleaser (the Router routes it to
	// the owning backend). Reuse the gc bd implementation.
	if id, expectedAssignee, ok, err := parseBdReleaseIfCurrentArgs(bdArgs); ok || err != nil {
		if err != nil {
			fmt.Fprintf(stderr, "gc bd-shim: %v\n", err) //nolint:errcheck // best-effort stderr
			return 1
		}
		return doBdReleaseIfCurrent(cityPath, target, id, expectedAssignee, stdout, stderr)
	}

	verb, verbArgs := splitBdGlobalFlags(bdArgs)
	switch classifyBdShimVerb(verb, verbArgs, graphStoreSQLiteEnabled(cfg)) {
	case bdRoute:
		store, err := openStoreAtForCity(target.ScopeRoot, cityPath)
		if err != nil {
			fmt.Fprintf(stderr, "gc bd-shim: opening store: %v\n", err) //nolint:errcheck // best-effort stderr
			return 1
		}
		defer closeBeadStoreHandle(store) //nolint:errcheck // best-effort close
		return dispatchBdShimVerb(store, verb, verbArgs, stdin, stdout, stderr)
	case bdRefuse:
		fmt.Fprintf(stderr, "gc bd-shim: %q reads or mutates graph-class beads but is not yet routed through the graph store; refusing to pass it to the work-only bd while graph_store=sqlite is active (would silently miss graph beads — see graph-store-rollout-plan.md §X2)\n", verb) //nolint:errcheck // best-effort stderr
		return 1
	default: // bdPassthrough
		env, err := bdCommandEnv(cityPath, cfg, target)
		if err != nil {
			fmt.Fprintf(stderr, "gc bd-shim: %v\n", err) //nolint:errcheck // best-effort stderr
			return 1
		}
		return execRealBd(bdArgs, target.ScopeRoot, workQueryEnvForDir(env, target.ScopeRoot), stdin, stdout, stderr)
	}
}

// isBdShimInvocation reports whether this gc binary was invoked through the bd
// shim — i.e. argv[0]'s basename is exactly `bd`. The PATH install symlinks
// `bd` -> the gc binary; gc invoked under any other name runs normally.
func isBdShimInvocation(arg0 string) bool {
	return filepath.Base(arg0) == "bd"
}

// ensureRealBdResolvable prepends the directory of GC_BD_REAL to PATH so that a
// bare `bd` exec performed in-process resolves to the real bd binary rather than
// this shim. The in-process work BdStore execs "bd" (and the Router probes each
// backend's Get by id, which runs `bd show`), so without this guard a shim
// installed as `bd` first on PATH would recurse on routed verbs. No-op when
// GC_BD_REAL is unset/relative or its directory already leads PATH.
func ensureRealBdResolvable() {
	raw := strings.TrimSpace(os.Getenv(realBdEnvVar))
	if raw == "" || !filepath.IsAbs(raw) {
		return
	}
	dir := filepath.Dir(raw)
	path := os.Getenv("PATH")
	sep := string(os.PathListSeparator)
	if path == dir || strings.HasPrefix(path, dir+sep) {
		return // already first; don't accumulate duplicate entries
	}
	if path == "" {
		_ = os.Setenv("PATH", dir) //nolint:errcheck // best-effort
		return
	}
	_ = os.Setenv("PATH", dir+sep+path) //nolint:errcheck // best-effort
}

// dispatchBdShimArgv0 runs the bd shim when this gc binary was invoked as `bd`,
// returning (exitCode, true). Otherwise it returns (0, false) and the caller
// proceeds with the normal gc command tree. When invoked as bd without
// GC_BD_REAL set it refuses loudly rather than recursing — a bare bd lookup
// would resolve back to this shim.
func dispatchBdShimArgv0(arg0 string, args []string, stdin io.Reader, stdout, stderr io.Writer) (int, bool) {
	if !isBdShimInvocation(arg0) {
		return 0, false
	}
	if strings.TrimSpace(os.Getenv(realBdEnvVar)) == "" {
		fmt.Fprintf(stderr, "bd (gc shim): %s must point at the real bd binary when gc runs as the bd shim; refusing to run (a bare bd lookup would recurse into the shim)\n", realBdEnvVar) //nolint:errcheck // best-effort stderr
		return 1, true
	}
	ensureRealBdResolvable()
	return runBdShim(args, stdin, stdout, stderr), true
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
