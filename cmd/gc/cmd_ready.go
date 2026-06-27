package main

import (
	"encoding/json"
	"fmt"
	"io"
	"sort"
	"strings"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/spf13/cobra"
)

// newReadyCmd builds `gc ready`: list ready work as a JSON array of beads,
// resolved through the per-class Router so a city that sets [beads] graph_store
// reads the graph-class ready slice from the embedded graph store in-process —
// PATH-independent, with no controller round-trip and no full Dolt ready scan.
// It renders the same routed/unassigned/non-epic predicate as the canonical
// `bd ready` work query (config.bdReadyPoolDemandShell), so `gc ready` is a
// drop-in for the dispatch work_query: a worker discovers ready graph beads in
// the embedded graph store, not just Dolt work beads. When a city does not opt
// into a graph store the Router is in its identity phase and the result is
// exactly the work store's ready set.
func newReadyCmd(stdout, stderr io.Writer) *cobra.Command {
	var (
		assignee         string
		limit            int
		metadataFields   []string
		excludeTypes     []string
		unassigned       bool
		includeEphemeral bool
		sortOrder        string
		jsonOut          bool
	)
	cmd := &cobra.Command{
		Use:   "ready",
		Short: "List ready work as JSON, graph-store aware through the per-class Router",
		Long: `List ready (open, unblocked) work as a JSON array of beads.

The store is opened through the per-class Router, so when a city sets
[beads] graph_store the ready set comes from the embedded graph store (the
graph-class slice), reached in-process without a controller round-trip. The
output is the bead JSON a work_query consumer unmarshals, so 'gc ready' can
stand in for a 'bd ready --json' work_query to make a worker's demand probe
graph-store aware while 'bd ready' keeps its Dolt work-store semantics.`,
		Args: cobra.NoArgs,
		RunE: func(_ *cobra.Command, _ []string) error {
			pred, err := readyPredicateFromFlags(metadataFields, excludeTypes, unassigned, includeEphemeral, sortOrder, limit)
			if err != nil {
				fmt.Fprintf(stderr, "gc ready: %v\n", err) //nolint:errcheck // best-effort stderr
				return errExit
			}
			if doReady(assignee, pred, stdout, stderr) != 0 {
				return errExit
			}
			return nil
		},
	}
	cmd.Flags().StringVar(&assignee, "assignee", "", "filter to beads assigned to this actor")
	cmd.Flags().IntVar(&limit, "limit", 0, "maximum beads to return (0 = no limit)")
	cmd.Flags().StringArrayVar(&metadataFields, "metadata-field", nil, "filter to beads whose metadata key=value (repeatable, AND)")
	cmd.Flags().StringArrayVar(&excludeTypes, "exclude-type", nil, "drop beads of this issue type (repeatable)")
	cmd.Flags().BoolVar(&unassigned, "unassigned", false, "filter to beads with no assignee")
	cmd.Flags().BoolVar(&includeEphemeral, "include-ephemeral", false, "include ephemeral (wisp-tier) beads")
	cmd.Flags().StringVar(&sortOrder, "sort", "", "sort order (oldest = by creation time ascending)")
	// --json is accepted for `bd ready` flag compatibility; gc ready always
	// emits JSON, so the flag is a no-op that lets the work query swap binaries
	// without editing flags.
	cmd.Flags().BoolVar(&jsonOut, "json", false, "emit JSON (always on; accepted for bd compatibility)")
	return cmd
}

// readyPredicate is the in-Go rendering of the `bd ready` work-query selectors
// that the embedded ready set does not filter natively. Applied after the store
// query so `gc ready` returns the same shape as `bd ready` across the routed,
// run_target-fallback, and ephemeral predicate branches.
type readyPredicate struct {
	metadataEquals map[string]string
	excludeTypes   map[string]bool
	unassigned     bool
	tierBoth       bool
	sortOldest     bool
	limit          int
}

func readyPredicateFromFlags(metadataFields, excludeTypes []string, unassigned, includeEphemeral bool, sortOrder string, limit int) (readyPredicate, error) {
	pred := readyPredicate{
		unassigned: unassigned,
		tierBoth:   includeEphemeral,
		limit:      limit,
	}
	for _, raw := range metadataFields {
		key, value, ok := strings.Cut(raw, "=")
		key = strings.TrimSpace(key)
		if !ok || key == "" {
			return readyPredicate{}, fmt.Errorf("invalid --metadata-field %q: want key=value", raw)
		}
		if pred.metadataEquals == nil {
			pred.metadataEquals = map[string]string{}
		}
		pred.metadataEquals[key] = value
	}
	for _, t := range excludeTypes {
		if t = strings.TrimSpace(t); t != "" {
			if pred.excludeTypes == nil {
				pred.excludeTypes = map[string]bool{}
			}
			pred.excludeTypes[t] = true
		}
	}
	switch strings.TrimSpace(sortOrder) {
	case "", "oldest":
		pred.sortOldest = strings.TrimSpace(sortOrder) == "oldest"
	default:
		return readyPredicate{}, fmt.Errorf("unsupported --sort %q: only \"oldest\" is supported", sortOrder)
	}
	return pred, nil
}

// applyReadyPredicate filters and orders ready beads to match the `bd ready`
// work-query selectors. It is pure so the predicate semantics are unit-tested
// without a live store.
func applyReadyPredicate(in []beads.Bead, p readyPredicate) []beads.Bead {
	out := make([]beads.Bead, 0, len(in))
	for _, b := range in {
		if p.unassigned && strings.TrimSpace(b.Assignee) != "" {
			continue
		}
		if len(p.excludeTypes) > 0 && p.excludeTypes[b.Type] {
			continue
		}
		match := true
		for key, want := range p.metadataEquals {
			if b.Metadata[key] != want {
				match = false
				break
			}
		}
		if !match {
			continue
		}
		out = append(out, b)
	}
	if p.sortOldest {
		sort.SliceStable(out, func(i, j int) bool {
			return out[i].CreatedAt.Before(out[j].CreatedAt)
		})
	}
	if p.limit > 0 && len(out) > p.limit {
		out = out[:p.limit]
	}
	return out
}

// doReady opens the city store and prints its graph-store-aware ready set as
// JSON, after applying the work-query predicate.
func doReady(assignee string, pred readyPredicate, stdout, stderr io.Writer) int {
	store, cityPath, code := openCityStoreWithPath(stderr, "gc ready")
	if store == nil {
		return code
	}
	defer closeBeadStoreHandle(store) //nolint:errcheck // best-effort close
	cfg, _ := loadCityConfig(cityPath, io.Discard)
	query := beads.ReadyQuery{Assignee: assignee}
	if pred.tierBoth {
		query.TierMode = beads.TierBoth
	}
	out, err := readyStoreSet(store, cfg, cityPath, query)
	if err != nil {
		fmt.Fprintf(stderr, "gc ready: %v\n", err) //nolint:errcheck // best-effort stderr
		return 1
	}
	return writeReadyJSON(applyReadyPredicate(out, pred), stdout, stderr)
}

// readyStoreSet returns the graph-class ready slice when the graph class is
// relocated (graph_store=sqlite/postgres), else the full ready set from the work
// store. Reading the dedicated graph store directly is the class-aware successor
// to the policy-wrapped Router's ReadyGraphOnly: it keeps the worker demand probe
// off the Dolt ready hot path and is the cheap "formula-step ready" slice. The
// graph leg reads with TierMode upgraded TierIssues->TierBoth, replicating the
// policy read-tier expansion the Router's forwarder (policyGraphOnlyReader ->
// expandPolicyReadyQuery) applied, so graph wisps stay visible. At the default
// `bd` backend resolveGraphStore returns the work store, so this is byte-identical
// to store.Ready(query).
func readyStoreSet(store beads.Store, cfg *config.City, cityPath string, query beads.ReadyQuery) ([]beads.Bead, error) {
	graph := resolveGraphStore(store, cfg, cityPath, nil)
	if graph != store {
		if query.TierMode == beads.TierIssues {
			query.TierMode = beads.TierBoth
		}
		return graph.Ready(query)
	}
	return store.Ready(query)
}

// writeReadyJSON encodes the ready beads as a JSON array — never null, so a
// work_query consumer that unmarshals into []beads.Bead always sees valid JSON.
func writeReadyJSON(out []beads.Bead, stdout, stderr io.Writer) int {
	if out == nil {
		out = []beads.Bead{}
	}
	if err := json.NewEncoder(stdout).Encode(out); err != nil {
		fmt.Fprintf(stderr, "gc ready: encoding: %v\n", err) //nolint:errcheck // best-effort stderr
		return 1
	}
	return 0
}
