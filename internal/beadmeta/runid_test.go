package beadmeta

import "testing"

func TestResolveRunID(t *testing.T) {
	cases := []struct {
		name       string
		metadata   map[string]string
		selfID     string
		fallbackID string
		want       string
	}{
		{
			name:       "workflow_id wins (graph workflow)",
			metadata:   map[string]string{"workflow_id": "wf-1", "molecule_id": "mol-1", RootBeadIDMetadataKey: "root-1"},
			selfID:     "b1",
			fallbackID: "s1",
			want:       "wf-1",
		},
		{
			name:       "molecule_id next (poured/wisp)",
			metadata:   map[string]string{"molecule_id": "mol-1", RootBeadIDMetadataKey: "root-1"},
			selfID:     "b1",
			fallbackID: "s1",
			want:       "mol-1",
		},
		{
			name:       "gc.root_bead_id next (nested)",
			metadata:   map[string]string{RootBeadIDMetadataKey: "root-1"},
			selfID:     "b1",
			fallbackID: "s1",
			want:       "root-1",
		},
		{
			// P5-OBS.2: a pooled session bead carries only gc.current_run_id (stamped at
			// claim = the run it is executing) and no structural root of its own — it must
			// resolve to that run, not its own id, so its usage facts roll up per-run.
			name:       "gc.current_run_id next (pooled session bead)",
			metadata:   map[string]string{CurrentRunIDMetadataKey: "gcg-run-1"},
			selfID:     "sess-1",
			fallbackID: "",
			want:       "gcg-run-1",
		},
		{
			// The structural nesting root outranks the session's current-run pointer, so a
			// WORK bead that (hypothetically) carried both still resolves via its own root.
			name:       "gc.root_bead_id outranks gc.current_run_id",
			metadata:   map[string]string{RootBeadIDMetadataKey: "root-1", CurrentRunIDMetadataKey: "gcg-run-1"},
			selfID:     "b1",
			fallbackID: "s1",
			want:       "root-1",
		},
		{
			name:       "blank gc.current_run_id skipped, falls to self id",
			metadata:   map[string]string{CurrentRunIDMetadataKey: "  "},
			selfID:     "sess-1",
			fallbackID: "",
			want:       "sess-1",
		},
		{
			name:       "self id fallback (plain work bead, worker path)",
			metadata:   nil,
			selfID:     "b1",
			fallbackID: "s1",
			want:       "b1",
		},
		{
			name:       "final fallback (manual chat: no bead, session id)",
			metadata:   nil,
			selfID:     "",
			fallbackID: "s1",
			want:       "s1",
		},
		{
			name:       "compute path: empty final fallback yields self id",
			metadata:   nil,
			selfID:     "session-bead-9",
			fallbackID: "",
			want:       "session-bead-9",
		},
		{
			name:       "blank chain values are skipped",
			metadata:   map[string]string{"workflow_id": "  ", "molecule_id": "mol-3"},
			selfID:     "b1",
			fallbackID: "s1",
			want:       "mol-3",
		},
		{
			name:       "blank self id falls through to fallback",
			metadata:   nil,
			selfID:     "   ",
			fallbackID: "s1",
			want:       "s1",
		},
		{
			name:       "all empty yields empty",
			metadata:   map[string]string{},
			selfID:     "",
			fallbackID: "",
			want:       "",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := ResolveRunID(tc.metadata, tc.selfID, tc.fallbackID); got != tc.want {
				t.Fatalf("ResolveRunID(%v, %q, %q) = %q, want %q", tc.metadata, tc.selfID, tc.fallbackID, got, tc.want)
			}
		})
	}
}
