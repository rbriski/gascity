package engine_test

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/graphstore"
	"github.com/gastownhall/gascity/internal/graphstore/fold"
	"github.com/gastownhall/gascity/internal/lumen/engine"
)

// Shared projection/journal read helpers for the engine fold, DAG, and DET tests.

func nodeMeta(t *testing.T, store *graphstore.Store, nodeID, key string) string {
	t.Helper()
	var v string
	_ = store.DB().QueryRowContext(context.Background(),
		`SELECT value FROM node_metadata WHERE node_id = ? AND key = ?`, nodeID, key).Scan(&v)
	return v
}

func inFrontier(t *testing.T, store *graphstore.Store, streamID, activation string) bool {
	t.Helper()
	var n int
	if err := store.DB().QueryRowContext(context.Background(),
		`SELECT COUNT(*) FROM frontier WHERE root_id = ? AND node_id = ?`, streamID, engine.ActivationNodeID(activation)).Scan(&n); err != nil {
		t.Fatalf("query frontier: %v", err)
	}
	return n > 0
}

func countJournalType(t *testing.T, store *graphstore.Store, streamID, typ string) int {
	t.Helper()
	var n int
	if err := store.DB().QueryRowContext(context.Background(),
		`SELECT COUNT(*) FROM journal WHERE stream_id = ? AND type = ?`, streamID, typ).Scan(&n); err != nil {
		t.Fatalf("count %s: %v", typ, err)
	}
	return n
}

func readFoldEvents(t *testing.T, store *graphstore.Store, streamID string) []fold.Event {
	t.Helper()
	stored, err := store.ReadStream(context.Background(), streamID, 1, 0)
	if err != nil {
		t.Fatalf("read stream: %v", err)
	}
	out := make([]fold.Event, len(stored))
	for i, e := range stored {
		out[i] = fold.Event{
			StreamID:          e.StreamID,
			Seq:               e.Seq,
			Engine:            e.Engine,
			Substream:         e.Substream,
			Type:              e.Type,
			IRContractVersion: e.IRContractVersion,
			IdemToken:         e.IdemToken,
			Payload:           e.Payload,
		}
	}
	return out
}

func collapseNodeUpserts(deltas []fold.Delta) map[string]fold.NodeRow {
	out := map[string]fold.NodeRow{}
	for _, d := range deltas {
		for _, n := range d.NodeUpserts {
			out[n.ID] = n
		}
	}
	return out
}

func assertProjectionEqualsRefold(t *testing.T, store *graphstore.Store, streamID string) {
	t.Helper()
	before := projectionSnapshot(t, store, streamID)
	if err := store.RebuildTierA(context.Background(), engine.Reducer(), streamID); err != nil {
		t.Fatalf("rebuild: %v", err)
	}
	after := projectionSnapshot(t, store, streamID)
	if before != after {
		t.Fatalf("live projection != drop+refold:\n--- live ---\n%s\n--- refold ---\n%s", before, after)
	}
}

func projectionSnapshot(t *testing.T, store *graphstore.Store, streamID string) string {
	t.Helper()
	ctx := context.Background()
	var b strings.Builder
	nodeRows, err := store.DB().QueryContext(ctx,
		`SELECT id, status, COALESCE(assignee,''), bead_type, COALESCE(parent_id,'')
		   FROM nodes WHERE stream_id = ? AND fold_owned = 1 ORDER BY id`, streamID)
	if err != nil {
		t.Fatalf("query nodes: %v", err)
	}
	defer func() { _ = nodeRows.Close() }()
	for nodeRows.Next() {
		var id, status, assignee, bt, parent string
		if err := nodeRows.Scan(&id, &status, &assignee, &bt, &parent); err != nil {
			t.Fatalf("scan node: %v", err)
		}
		fmt.Fprintf(&b, "node %s status=%s assignee=%s type=%s parent=%s\n", id, status, assignee, bt, parent)
	}
	metaRows, err := store.DB().QueryContext(ctx,
		`SELECT m.node_id, m.key, m.value FROM node_metadata m
		   JOIN nodes n ON n.id = m.node_id
		  WHERE n.stream_id = ? AND n.fold_owned = 1 ORDER BY m.node_id, m.key`, streamID)
	if err != nil {
		t.Fatalf("query metadata: %v", err)
	}
	defer func() { _ = metaRows.Close() }()
	var metaLines []string
	for metaRows.Next() {
		var id, k, v string
		if err := metaRows.Scan(&id, &k, &v); err != nil {
			t.Fatalf("scan meta: %v", err)
		}
		metaLines = append(metaLines, fmt.Sprintf("meta %s %s=%s", id, k, v))
	}
	sort.Strings(metaLines)
	b.WriteString(strings.Join(metaLines, "\n"))
	b.WriteString("\n")
	frontierRows, err := store.DB().QueryContext(ctx,
		`SELECT node_id FROM frontier WHERE root_id = ? ORDER BY node_id`, streamID)
	if err != nil {
		t.Fatalf("query frontier: %v", err)
	}
	defer func() { _ = frontierRows.Close() }()
	for frontierRows.Next() {
		var id string
		if err := frontierRows.Scan(&id); err != nil {
			t.Fatalf("scan frontier: %v", err)
		}
		fmt.Fprintf(&b, "frontier %s\n", id)
	}
	return b.String()
}
