package main

import (
	"encoding/json"
	"fmt"
	"io"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/spf13/cobra"
)

// newReadyCmd builds `gc ready`: list ready work as a JSON array of beads,
// federated across the work and graph stores through the per-class Router. It is
// the in-process, graph-store-aware demand probe — a drop-in for a
// `bd ready --json` work_query so a worker discovers ready graph beads in the
// embedded graph store, not just Dolt work beads. When a city does not opt into a
// graph store the Router is in its identity phase and the result is exactly the
// work store's ready set.
func newReadyCmd(stdout, stderr io.Writer) *cobra.Command {
	var (
		assignee string
		limit    int
	)
	cmd := &cobra.Command{
		Use:   "ready",
		Short: "List ready work as JSON, federated across the work and graph stores",
		Long: `List ready (open, unblocked) work as a JSON array of beads.

The store is opened through the per-class Router, so when a city sets
[beads] graph_store the result federates ready work from both the Dolt-backed
work store and the embedded graph store. The output is the bead JSON a work_query
consumer unmarshals, so 'gc ready' can stand in for a 'bd ready --json' work_query
to make a worker's demand probe graph-store aware.`,
		Args: cobra.NoArgs,
		RunE: func(_ *cobra.Command, _ []string) error {
			if doReady(assignee, limit, stdout, stderr) != 0 {
				return errExit
			}
			return nil
		},
	}
	cmd.Flags().StringVar(&assignee, "assignee", "", "filter to beads assigned to this actor")
	cmd.Flags().IntVar(&limit, "limit", 0, "maximum beads to return (0 = no limit)")
	return cmd
}

// doReady opens the city store and prints its federated ready set as JSON.
func doReady(assignee string, limit int, stdout, stderr io.Writer) int {
	store, code := openCityStore(stderr, "gc ready")
	if store == nil {
		return code
	}
	defer closeBeadStoreHandle(store) //nolint:errcheck // best-effort close
	out, err := store.Ready(beads.ReadyQuery{Assignee: assignee, Limit: limit})
	if err != nil {
		fmt.Fprintf(stderr, "gc ready: %v\n", err) //nolint:errcheck // best-effort stderr
		return 1
	}
	return writeReadyJSON(out, stdout, stderr)
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
