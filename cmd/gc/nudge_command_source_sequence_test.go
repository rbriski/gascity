package main

import (
	"sort"
	"strings"

	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/nudgequeue"
)

// ListHistoryByControlSequence keeps the command-source store double honest
// against the provider-owned exact-sequence projection contract.
func (tx *nudgeCommandSourceAtomicTx) ListHistoryByControlSequence(query beads.AtomicReadSnapshotControlSequenceQuery) (beads.AtomicReadSnapshotControlSequencePage, error) {
	if query.Limit <= 0 || query.Limit > beads.MaxAtomicReadSnapshotPageSize || query.IDPrefix == "" || query.Sequence == 0 {
		return beads.AtomicReadSnapshotControlSequencePage{}, beads.ErrAtomicReadSnapshotQuery
	}
	rows := make([]beads.Bead, 0, query.Limit)
	for _, row := range tx.rows {
		if row.Ephemeral || row.NoHistory || !strings.HasPrefix(row.ID, query.IDPrefix) {
			continue
		}
		decoded := nudgequeue.DecodeCommand([]byte(row.Metadata[beadmeta.ControlCommandWireMetadataKey]))
		if decoded.Routing.Sequence != query.Sequence {
			continue
		}
		rows = append(rows, cloneNudgeCommandSourceRow(row))
	}
	sort.Slice(rows, func(i, j int) bool { return rows[i].ID < rows[j].ID })
	if len(rows) > query.Limit {
		rows = rows[:query.Limit]
	}
	return beads.AtomicReadSnapshotControlSequencePage{Rows: rows}, nil
}
