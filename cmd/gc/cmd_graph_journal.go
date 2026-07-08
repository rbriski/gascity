package main

import (
	"context"
	"fmt"
	"io"
	"text/tabwriter"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/spf13/cobra"
)

// newGraphJournalCmd is the hidden operator/debug window onto unified settlement
// provenance (P5.4): it prints a root's provenance folded from the city's shared
// graph journal — the one place lumen (fine-grained terminal), v2, and v1
// provenance surface together. Because a root's two journal streams
// (settlement/<root> and the lumen run stream <root>) each carry an INDEPENDENT
// dense seq, the output is grouped PER STREAM, each with its own seq-ordered,
// engine-tagged table — never a single fake global sequence across streams.
// Hidden and minimal by design: plain text only. Typed/JSON, dashboard/API, and
// gc-trace integration are deliberate follow-up (P5 makes zero wire changes), so
// this command intentionally does not opt into the --json schema contract.
func newGraphJournalCmd(stdout, stderr io.Writer) *cobra.Command {
	return &cobra.Command{
		Use:    "journal <root-id>",
		Short:  "Print the unified settlement provenance timeline for a root",
		Hidden: true,
		Args:   cobra.ExactArgs(1),
		RunE: func(_ *cobra.Command, args []string) error {
			if cmdGraphJournal(args[0], stdout, stderr) != 0 {
				return errExit
			}
			return nil
		},
	}
}

// cmdGraphJournal resolves the city's shared graph journal and prints rootID's
// provenance timeline.
func cmdGraphJournal(rootID string, stdout, stderr io.Writer) int {
	cityPath, err := resolveCity()
	if err != nil {
		fmt.Fprintf(stderr, "gc graph journal: %v\n", err) //nolint:errcheck // best-effort stderr
		return 1
	}
	journal := cachedCityGraphJournal(cityPath)
	if journal == nil {
		fmt.Fprintf(stderr, "gc graph journal: city has no graph journal scope (.gc/graph); nothing to show\n") //nolint:errcheck // best-effort stderr
		return 1
	}
	return writeProvenanceTimeline(journal, rootID, stdout, stderr)
}

// writeProvenanceTimeline folds rootID's settlement provenance from journal and
// renders it grouped BY STREAM: one seq-ordered, engine-tagged table per stream
// under a stream header. The SEQ column is per-stream (honest — each stream has
// its own dense seq); no global sequence is presented across streams, since the
// two streams' seqs are not cross-comparable. Separated from city resolution so
// the fold+render is directly testable over an in-memory journal.
func writeProvenanceTimeline(journal beads.Store, rootID string, stdout, stderr io.Writer) int {
	streams, err := beads.ProvenanceTimeline(context.Background(), journal, rootID)
	if err != nil {
		fmt.Fprintf(stderr, "gc graph journal: %v\n", err) //nolint:errcheck // best-effort stderr
		return 1
	}

	if len(streams) == 0 {
		fmt.Fprintf(stdout, "no settlement provenance for %s\n", rootID) //nolint:errcheck // best-effort stdout
		return 0
	}

	for i, s := range streams {
		if i > 0 {
			fmt.Fprintln(stdout) //nolint:errcheck // best-effort stdout
		}
		// Per-stream header: the stream id makes the SEQ column's scope explicit,
		// so a reader never mistakes two independent per-stream sequences for one
		// global order.
		fmt.Fprintf(stdout, "stream %s (%d fact(s)):\n", s.StreamID, len(s.Facts)) //nolint:errcheck // best-effort stdout
		tw := tabwriter.NewWriter(stdout, 0, 2, 2, ' ', 0)
		fmt.Fprintln(tw, "SEQ\tENGINE\tTYPE\tROOT\tBEAD\tOUTCOME\tATTEMPT") //nolint:errcheck // best-effort stdout
		for _, f := range s.Facts {
			attempt := ""
			if f.Attempt > 0 {
				attempt = fmt.Sprintf("%d", f.Attempt)
			}
			fmt.Fprintf(tw, "%d\t%s\t%s\t%s\t%s\t%s\t%s\n", //nolint:errcheck // best-effort stdout
				f.Seq, f.Engine, f.Type, f.Root, f.Bead, f.Outcome, attempt)
		}
		tw.Flush() //nolint:errcheck // best-effort stdout
	}
	return 0
}
