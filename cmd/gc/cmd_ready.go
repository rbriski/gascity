package main

import (
	"fmt"
	"io"
	"strings"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/spf13/cobra"
)

// newReadyCmd builds `gc ready`: the composite, in-process drop-in for the
// external single-store `bd ready` work_query. On a split city it federates the
// work store and the infra store (where graph-class step beads live) so workers
// and the control plane can claim graph work; on a legacy single-store city it
// reads the one store, byte-identical. Output is a JSON array of beads with the
// same wire tags `bd ready` emits (issue_type/parent), so it is drop-in
// parseable by the hook's work-query decode path.
func newReadyCmd(stdout, stderr io.Writer) *cobra.Command {
	var opts readyOpts
	var jsonOut bool
	cmd := &cobra.Command{
		Use: "ready",
		// gc ready emits a bd-compatible JSON array (issue_type/parent wire tags)
		// that the hook decode and reconciler jq paths already parse, so it opts out
		// of the structured --json contract the same way `gc bd` does: it owns its
		// (bd-shaped) payload. This lets the split-city work_query pass --json
		// through to it verbatim instead of the contract rejecting it.
		Annotations: map[string]string{jsonRawPassthroughAnnotation: "true"},
		Short:       "List ready (claimable) work across a split city's stores",
		Long: `List ready, claimable work as a JSON array, federating the work store and the
infra store (graph-class steps) on a split city.

It is the composite, in-process drop-in for the external, single-store
"bd ready" work_query, so workers and the control plane see graph step beads
that live in the infra store. On a legacy single-store city it reads the one
store, byte-identical.

The flags mirror the "bd ready" contract the default work_query builds:
  gc ready --metadata-field "gc.routed_to=$target" --unassigned \
           --exclude-type=epic --sort oldest --limit 20`,
		Args: cobra.NoArgs,
		RunE: func(_ *cobra.Command, _ []string) error {
			if cmdReady(opts, stdout, stderr) != 0 {
				return errExit
			}
			return nil
		},
	}
	cmd.Flags().StringVar(&opts.assignee, "assignee", "", "only work assigned to this identity")
	cmd.Flags().BoolVar(&opts.unassigned, "unassigned", false, "only unassigned work")
	cmd.Flags().StringArrayVar(&opts.metadataFields, "metadata-field", nil, "require metadata key=value (repeatable)")
	cmd.Flags().StringArrayVar(&opts.excludeTypes, "exclude-type", nil, "drop beads of this issue type (repeatable)")
	cmd.Flags().StringVar(&opts.sortOrder, "sort", "", "sort order: oldest|newest (default: ready priority order)")
	cmd.Flags().IntVar(&opts.limit, "limit", 0, "max beads to return (0 = unlimited)")
	cmd.Flags().BoolVar(&opts.includeEphemeral, "include-ephemeral", false, "include the wisp/ephemeral tier")
	cmd.Flags().StringVar(&opts.status, "status", "", "status mode: empty = ready work; in_progress = list assigned in-progress work")
	cmd.Flags().BoolVar(&opts.count, "count", false, "print the number of matching beads instead of the array")
	// --json is accepted for parity with `bd ready --json` (the work_query carries
	// it); output is always a JSON array regardless. gc ready is a raw-JSON
	// passthrough (see the command Annotations) so this flag is not the structured
	// --json contract flag.
	cmd.Flags().BoolVar(&jsonOut, "json", true, "accept --json for bd-ready parity (output is always a JSON array)")
	return cmd
}

// readyOpts carries the parsed `gc ready` flags.
type readyOpts struct {
	assignee         string
	unassigned       bool
	metadataFields   []string
	excludeTypes     []string
	sortOrder        string
	limit            int
	includeEphemeral bool
	status           string
	count            bool
}

func cmdReady(opts readyOpts, stdout, stderr io.Writer) int {
	cityPath, err := resolveCity()
	if err != nil {
		fmt.Fprintf(stderr, "gc ready: %v\n", err) //nolint:errcheck
		return 1
	}
	cfg, err := loadCityConfig(cityPath, stderr)
	if err != nil {
		fmt.Fprintf(stderr, "gc ready: %v\n", err) //nolint:errcheck
		return 1
	}
	cs, err := newClaimableStore(cityPath, cfg)
	if err != nil {
		fmt.Fprintf(stderr, "gc ready: %v\n", err) //nolint:errcheck
		return 1
	}
	items, err := readyBeadsForOpts(cs, opts)
	if err != nil {
		fmt.Fprintf(stderr, "gc ready: %v\n", err) //nolint:errcheck
		return 1
	}
	items = filterReadyBeads(items, opts)
	sortReadyOutput(items, opts.sortOrder)
	items = applyBeadLimit(items, opts.limit)
	if opts.count {
		if err := writeJSON(stdout, len(items)); err != nil {
			fmt.Fprintf(stderr, "gc ready: %v\n", err) //nolint:errcheck
			return 1
		}
		return 0
	}
	if err := renderReadyBeads(stdout, items); err != nil {
		fmt.Fprintf(stderr, "gc ready: %v\n", err) //nolint:errcheck
		return 1
	}
	return 0
}

// readyBeadsForOpts reads the merged candidate set from the composite store. The
// empty (default) status mode returns ready work; status=in_progress lists
// assigned in-progress work for crash recovery of a graph step whose worker died.
func readyBeadsForOpts(cs *claimableStore, opts readyOpts) ([]beads.Bead, error) {
	tier := beads.TierIssues
	if opts.includeEphemeral {
		tier = beads.TierBoth
	}
	if strings.TrimSpace(opts.status) == "in_progress" {
		return cs.List(beads.ListQuery{
			Status:   "in_progress",
			Assignee: opts.assignee,
			TierMode: tier,
			Live:     true,
		})
	}
	return cs.Ready(beads.ReadyQuery{Assignee: opts.assignee, TierMode: tier})
}

// filterReadyBeads applies the post-read predicates the ready store cannot
// express natively: --unassigned, --metadata-field, and --exclude-type.
func filterReadyBeads(items []beads.Bead, opts readyOpts) []beads.Bead {
	metaWant := parseMetadataFieldFilters(opts.metadataFields)
	exclude := make(map[string]bool, len(opts.excludeTypes))
	for _, t := range opts.excludeTypes {
		if t = strings.TrimSpace(t); t != "" {
			exclude[t] = true
		}
	}
	out := make([]beads.Bead, 0, len(items))
	for _, b := range items {
		if opts.unassigned && strings.TrimSpace(b.Assignee) != "" {
			continue
		}
		if exclude[b.Type] {
			continue
		}
		if !beadMatchesMetadata(b, metaWant) {
			continue
		}
		out = append(out, b)
	}
	return out
}

// parseMetadataFieldFilters splits repeated "key=value" flags into a map. A flag
// without "=" requires only that the key be present (any non-empty value).
func parseMetadataFieldFilters(fields []string) map[string]string {
	if len(fields) == 0 {
		return nil
	}
	want := make(map[string]string, len(fields))
	for _, f := range fields {
		key, value, _ := strings.Cut(f, "=")
		want[strings.TrimSpace(key)] = value
	}
	return want
}

func beadMatchesMetadata(b beads.Bead, want map[string]string) bool {
	for key, value := range want {
		if b.Metadata[key] != value {
			return false
		}
	}
	return true
}

// sortReadyOutput imposes the requested output order. "oldest"/"newest" map to
// the canonical created-at order; anything else keeps the ready priority order
// (priority, created_at, id) the composite already produced.
func sortReadyOutput(items []beads.Bead, order string) {
	switch strings.TrimSpace(order) {
	case "oldest":
		beads.SortBeads(items, beads.SortCreatedAsc)
	case "newest":
		beads.SortBeads(items, beads.SortCreatedDesc)
	default:
		sortReadyBeadsCanonical(items)
	}
}

// renderReadyBeads writes items as a JSON array with bd's wire tags
// (issue_type/parent), emitting [] rather than null for an empty result so the
// hook's work-query decode is a faithful drop-in for `bd ready --json`.
func renderReadyBeads(w io.Writer, items []beads.Bead) error {
	if items == nil {
		items = []beads.Bead{}
	}
	return writeJSON(w, items)
}
