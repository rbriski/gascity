package exec //nolint:revive // internal package, always imported with alias

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
)

func writeReadySnapshotProtocolFixture(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "ready-provider")
	if err := os.WriteFile(path, []byte("#!/bin/sh\n"+body), 0o755); err != nil {
		t.Fatalf("write ready protocol fixture: %v", err)
	}
	return path
}

func TestReadySnapshotFilterMatchesDirectExecQueries(t *testing.T) {
	script := writeReadySnapshotProtocolFixture(t, `
case "$1" in
  ready)
    echo '[
      {"id":"EX-a1","title":"alice first","status":"open","type":"task","priority":1,"created_at":"2026-07-12T10:00:00Z","updated_at":"2026-07-12T10:01:00Z","assignee":"alice","from":"dispatcher","parent_id":"EX-root","ref":"step.a","needs":["setup"],"description":"first alice row","labels":["ready","alpha"],"metadata":{"route":"alpha","attempt":2,"approved":true}},
      {"id":"EX-b1","title":"bob ephemeral","status":"open","type":"task","priority":0,"created_at":"2026-07-12T10:02:00Z","assignee":"bob","ref":"step.b","description":"bob row","labels":["ready","beta"],"metadata":{"route":"beta"},"ephemeral":true},
      {"id":"EX-a2","title":"alice second","status":"open","type":"bug","created_at":"2026-07-12T10:03:00Z","assignee":"alice","ref":"step.c","description":"second alice row","labels":["ready","alpha"],"metadata":{"route":"alpha","attempt":"3"},"no_history":true},
      {"id":"EX-u1","title":"unassigned","status":"open","type":"task","created_at":"2026-07-12T10:04:00Z","description":"unassigned row","labels":["ready"],"metadata":{"route":"worker"}}
    ]'
    ;;
  *) exit 2 ;;
esac
`)
	store := NewStore(script)

	snapshot, err := store.Ready(beads.ReadyQuery{TierMode: beads.TierBoth})
	if err != nil {
		t.Fatalf("unfiltered Ready(TierBoth): %v", err)
	}
	if len(snapshot) != 4 {
		t.Fatalf("unfiltered Ready(TierBoth) rows = %d, want 4: %+v", len(snapshot), snapshot)
	}

	queries := []struct {
		name  string
		query beads.ReadyQuery
	}{
		{name: "all", query: beads.ReadyQuery{TierMode: beads.TierBoth}},
		{name: "all limit two", query: beads.ReadyQuery{Limit: 2, TierMode: beads.TierBoth}},
		{name: "alice", query: beads.ReadyQuery{Assignee: "alice", TierMode: beads.TierBoth}},
		{name: "alice limit one", query: beads.ReadyQuery{Assignee: "alice", Limit: 1, TierMode: beads.TierBoth}},
		{name: "bob", query: beads.ReadyQuery{Assignee: "bob", Limit: 3, TierMode: beads.TierBoth}},
		{name: "missing", query: beads.ReadyQuery{Assignee: "missing", Limit: 1, TierMode: beads.TierBoth}},
	}
	for _, tc := range queries {
		t.Run(tc.name, func(t *testing.T) {
			want, err := store.Ready(tc.query)
			if err != nil {
				t.Fatalf("direct Ready(%+v): %v", tc.query, err)
			}
			got := beads.FilterReadySnapshot(snapshot, tc.query)
			if !reflect.DeepEqual(got, want) {
				t.Fatalf("filtered snapshot differs from direct exec Ready\n got: %#v\nwant: %#v", got, want)
			}
		})
	}
}

func TestReadySnapshotExecErrorsCannotBecomeEmptySuccess(t *testing.T) {
	tests := []struct {
		name       string
		scriptBody string
		wantError  func(script string) string
		assertType func(*testing.T, error)
	}{
		{
			name: "nonzero exit",
			scriptBody: `
case "$1" in
  ready)
    echo 'exec-ready-nonzero-sentinel' >&2
    exit 23
    ;;
  *) exit 2 ;;
esac
`,
			wantError: func(script string) string {
				return fmt.Sprintf("exec beads ready: exec beads %s ready --include-ephemeral: exec-ready-nonzero-sentinel", script)
			},
		},
		{
			name: "malformed protocol JSON",
			scriptBody: `
case "$1" in
  ready)
    printf 'not-json'
    ;;
  *) exit 2 ;;
esac
`,
			assertType: func(t *testing.T, err error) {
				t.Helper()
				var syntaxErr *json.SyntaxError
				if !errors.As(err, &syntaxErr) {
					t.Fatalf("protocol error = %T %v, want wrapped *json.SyntaxError", err, err)
				}
			},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			script := writeReadySnapshotProtocolFixture(t, tc.scriptBody)
			store := NewStore(script)
			query := beads.ReadyQuery{Assignee: "alice", Limit: 1, TierMode: beads.TierBoth}

			snapshotRows, snapshotErr := store.Ready(beads.ReadyQuery{TierMode: beads.TierBoth})
			if snapshotErr == nil {
				t.Fatalf("unfiltered snapshot returned empty success: rows=%#v", snapshotRows)
			}
			if snapshotRows != nil {
				t.Fatalf("unfiltered snapshot rows = %#v, want nil with error", snapshotRows)
			}

			directRows, directErr := store.Ready(query)
			if directErr == nil {
				t.Fatalf("direct filtered Ready returned empty success: rows=%#v", directRows)
			}
			if directRows != nil {
				t.Fatalf("direct filtered Ready rows = %#v, want nil with error", directRows)
			}
			if snapshotErr.Error() != directErr.Error() {
				t.Fatalf("snapshot error = %q, direct error = %q; want exact preservation", snapshotErr, directErr)
			}
			if tc.wantError != nil {
				if want := tc.wantError(script); snapshotErr.Error() != want {
					t.Fatalf("Ready error = %q, want %q", snapshotErr, want)
				}
			}
			if tc.assertType != nil {
				tc.assertType(t, snapshotErr)
				tc.assertType(t, directErr)
			}
		})
	}
}
