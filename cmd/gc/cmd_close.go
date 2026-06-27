package main

import (
	"fmt"
	"io"

	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/storeref"
	"github.com/spf13/cobra"
)

// newCloseCmd builds `gc close <id>`: close a graph-class bead (a molecule step,
// wisp, control bead) in the dedicated graph store and a work bead in the work
// store — routed by id. It is the in-process, graph-store-aware close a worker uses
// to finish a step it found via `gc ready`, instead of a raw `bd close` that only
// reaches the Dolt work store. The controller's reconcile reads the close back from
// the shared graph store (graph reads bypass the work cache) and converges the
// molecule.
func newCloseCmd(stdout, stderr io.Writer) *cobra.Command {
	var outcome string
	cmd := &cobra.Command{
		Use:   "close <id>",
		Short: "Close a bead by id (graph beads close in the graph store)",
		Long: `Close a bead by id, routing to its owning store.

When a city sets [beads] graph_store, a graph-class bead is closed in the embedded
graph store and a work bead in the work store — routed by id. Use --outcome to
stamp gc.outcome before closing (e.g. --outcome pass), as a worker does when
finishing a step so the molecule's evaluation can converge.`,
		Args: cobra.ExactArgs(1),
		RunE: func(_ *cobra.Command, args []string) error {
			if doClose(args[0], outcome, stdout, stderr) != 0 {
				return errExit
			}
			return nil
		},
	}
	cmd.Flags().StringVar(&outcome, "outcome", "", "stamp gc.outcome on the bead before closing (e.g. pass)")
	return cmd
}

// doClose opens the city store and closes the bead, routing a graph-resident id to
// the dedicated graph store via storeref.Resolve over [graph, work]. Post-GF the
// per-class Router is gone, so the by-id close must resolve the owning store
// explicitly (the work-store policy wrapper's own Close would miss a gcg- bead).
func doClose(id, outcome string, stdout, stderr io.Writer) int {
	store, cityPath, code := openCityStoreWithPath(stderr, "gc close")
	if store == nil {
		return code
	}
	defer closeBeadStoreHandle(store) //nolint:errcheck // best-effort close

	target := store
	if cfg, err := loadCityConfig(cityPath, stderr); err == nil && graphRelocated(cfg) {
		if graph := resolveGraphStore(store, cfg, cityPath, nil); graph != store {
			// A graph-resident id carries the disjoint gcg- prefix that the graph
			// store owns; route its close there. A work id has no graph-prefix
			// match (PrefixOwner returns nil) and stays on the work store.
			if owner := storeref.PrefixOwner(id, []beads.Store{graph, store}); owner != nil {
				target = owner
			}
		}
	}

	if err := closeBeadThroughStore(target, id, outcome); err != nil {
		fmt.Fprintf(stderr, "gc close: %v\n", err) //nolint:errcheck // best-effort stderr
		return 1
	}
	fmt.Fprintf(stdout, "closed %s\n", id) //nolint:errcheck // best-effort stdout
	return 0
}

// closeBeadThroughStore stamps gc.outcome (when given) and closes the bead on the
// given store. The caller routes a graph-resident id to the graph store; this just
// performs the SetMetadata + Close on whichever store owns the bead.
func closeBeadThroughStore(store beads.Store, id, outcome string) error {
	if outcome != "" {
		if err := store.SetMetadata(id, beadmeta.OutcomeMetadataKey, outcome); err != nil {
			return fmt.Errorf("stamping %s on %q: %w", beadmeta.OutcomeMetadataKey, id, err)
		}
	}
	if err := store.Close(id); err != nil {
		return fmt.Errorf("closing %q: %w", id, err)
	}
	return nil
}
